package session

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

var (
	// ErrInvalidPRRef is returned when a claim request does not name a PR/MR URL or positive PR number.
	ErrInvalidPRRef = errors.New("session: invalid pr ref")
	// ErrPRNotFound is returned when the SCM provider has no matching pull request.
	ErrPRNotFound = errors.New("session: pr not found")
	// ErrPRNotOpen is returned when a PR is merged or closed and therefore cannot be claimed.
	// Draft PRs are open work and remain claimable.
	ErrPRNotOpen = errors.New("session: pr not open")
	// ErrSCMUnavailable is returned when live SCM facts cannot be fetched.
	ErrSCMUnavailable = errors.New("session: scm unavailable")
	// ErrProjectMismatch is returned when the PR repository does not match the session project repository.
	ErrProjectMismatch = errors.New("session: pr project mismatch")
	// ErrSessionNotClaimable is returned when a session is not allowed to claim a PR.
	ErrSessionNotClaimable = errors.New("session: not claimable")
	// ErrSessionNoWorkspace is returned when a session has no workspace path to associate with PR work.
	ErrSessionNoWorkspace = errors.New("session: no workspace")
)

// ClaimPROptions controls PR claim conflict behavior.
type ClaimPROptions struct {
	AllowTakeover bool
}

// ClaimPRResult is the session PR read model returned after a claim.
type ClaimPRResult struct {
	PRs                []domain.PRFacts
	BranchChanged      bool
	TakenOverFrom      []domain.SessionID
	DonorWasTerminated bool
}

// ListPRs returns all PRs currently owned by a session, ordered for display.
func (s *Service) ListPRs(ctx context.Context, id domain.SessionID) ([]domain.PRFacts, error) {
	_, ok, err := s.store.GetSession(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("get %s: %w", id, err)
	}
	if !ok {
		return nil, apierr.NotFound("SESSION_NOT_FOUND", "Unknown session")
	}
	return s.listPRFacts(ctx, id)
}

// ClaimPR attaches a live pull request (GitHub PR or GitLab MR) to a worker session and persists the current SCM facts atomically.
func (s *Service) ClaimPR(ctx context.Context, id domain.SessionID, ref string, opts ClaimPROptions) (ClaimPRResult, error) {
	rec, ok, err := s.store.GetSession(ctx, id)
	if err != nil {
		return ClaimPRResult{}, fmt.Errorf("get %s: %w", id, err)
	}
	if !ok {
		return ClaimPRResult{}, apierr.NotFound("SESSION_NOT_FOUND", "Unknown session")
	}
	if rec.IsTerminated {
		return ClaimPRResult{}, sessionmanagerAPIError("SESSION_TERMINATED", "Session is terminated")
	}
	if rec.Kind == domain.KindOrchestrator {
		return ClaimPRResult{}, ErrSessionNotClaimable
	}
	if strings.TrimSpace(rec.Metadata.WorkspacePath) == "" {
		return ClaimPRResult{}, ErrSessionNoWorkspace
	}
	project, ok, err := s.store.GetProject(ctx, string(rec.ProjectID))
	if err != nil {
		return ClaimPRResult{}, fmt.Errorf("project %s: %w", rec.ProjectID, err)
	}
	if !ok {
		return ClaimPRResult{}, apierr.Invalid("PROJECT_NOT_RESOLVABLE", "Project is not registered or has no repo — register it with `ao project add`", nil)
	}
	if project.Kind.WithDefault() == domain.ProjectKindScratch {
		return ClaimPRResult{}, ErrSessionNotClaimable
	}
	prURL, number, err := normalizePRRef(ref, project.RepoOriginURL, s.scm)
	if err != nil {
		return ClaimPRResult{}, err
	}
	if err := requireSameRepo(prURL, project.RepoOriginURL); err != nil {
		return ClaimPRResult{}, err
	}
	if s.scm == nil || s.prClaimer == nil {
		return ClaimPRResult{}, ErrSCMUnavailable
	}
	repo, err := scmRepoForClaim(s.scm, project.RepoOriginURL, prURL)
	if err != nil {
		return ClaimPRResult{}, err
	}
	refSpec := ports.SCMPRRef{Repo: repo, Number: number, URL: prURL}
	obs, err := s.fetchClaimObservation(ctx, refSpec)
	if err != nil {
		return ClaimPRResult{}, err
	}
	if obs.PR.Number == 0 {
		obs.PR.Number = number
	}
	if obs.PR.URL == "" {
		obs.PR.URL = prURL
	}
	// Draft PRs are still open work: workers must be able to claim them without
	// first marking the PR ready for review. Only terminal states are rejected;
	// the draft fact itself is preserved on the persisted PR row below.
	if obs.PR.Merged || obs.PR.Closed {
		return ClaimPRResult{}, ErrPRNotOpen
	}
	reviewMode, err := s.enrichClaimReviews(ctx, refSpec, &obs)
	if err != nil {
		return ClaimPRResult{}, err
	}
	now := s.clock().UTC()
	pr, checks, reviews, threads, comments := claimRowsFromSCM(id, obs, now, rec)
	outcome, err := s.prClaimer.ClaimPR(ctx, pr, checks, reviews, threads, comments, reviewMode, opts.AllowTakeover)
	if err != nil {
		return ClaimPRResult{}, err
	}
	prs, err := s.listPRFacts(ctx, id)
	if err != nil {
		return ClaimPRResult{}, err
	}
	prs = claimedFirst(prs, prURL)
	// TODO: implement workspace branch checkout. Until then, leave BranchChanged
	// false and let CLI output omit the checkout line rather than claiming the
	// session was already on the PR branch.
	res := ClaimPRResult{PRs: prs, BranchChanged: false, DonorWasTerminated: outcome.OwnerTerminated}
	if outcome.PreviousOwner != "" && outcome.PreviousOwner != id {
		res.TakenOverFrom = []domain.SessionID{outcome.PreviousOwner}
	}
	return res, nil
}

func (s *Service) fetchClaimObservation(ctx context.Context, ref ports.SCMPRRef) (ports.SCMObservation, error) {
	batch, err := s.scm.FetchPullRequests(ctx, []ports.SCMPRRef{ref})
	if err != nil {
		if errors.Is(err, ports.ErrSCMNotFound) {
			return ports.SCMObservation{}, ErrPRNotFound
		}
		return ports.SCMObservation{}, fmt.Errorf("%w: %w", ErrSCMUnavailable, err)
	}
	if len(batch) == 0 {
		return ports.SCMObservation{}, ErrPRNotFound
	}
	obs := batch[0]
	if errors.Is(obs.Error, ports.ErrSCMNotFound) {
		return ports.SCMObservation{}, ErrPRNotFound
	}
	if !obs.Fetched {
		return ports.SCMObservation{}, ErrSCMUnavailable
	}
	return obs, nil
}

func (s *Service) enrichClaimReviews(ctx context.Context, ref ports.SCMPRRef, obs *ports.SCMObservation) (ports.ReviewWriteMode, error) {
	review, err := s.scm.FetchReviewThreads(ctx, ref)
	if err != nil {
		if errors.Is(err, ports.ErrSCMNotFound) {
			return ports.ReviewWritePreserve, ErrPRNotFound
		}
		// Review thread fetch failed but the PR itself was observed — proceed
		// with ReviewWritePreserve so the claim still succeeds.  Review
		// threads will be retried on the next observer poll.
		if s.logger != nil {
			s.logger.Warn("claim: review thread fetch failed, proceeding without threads",
				"pr", ref.URL,
				"error", err,
			)
		}
		return ports.ReviewWritePreserve, nil
	}
	if review.Decision != "" {
		obs.Review.Decision = review.Decision
	}
	obs.Review.Threads = review.Threads
	obs.Review.Reviews = review.Reviews
	obs.Review.Partial = review.Partial
	if review.Partial {
		return ports.ReviewWriteMerge, nil
	}
	return ports.ReviewWriteReplace, nil
}

func scmRepoForClaim(provider scmProvider, projectOrigin, prURL string) (ports.SCMRepo, error) {
	if repo, ok := provider.ParseRepository(projectOrigin); ok {
		return repo, nil
	}
	parts, err := parsePRURL(prURL)
	if err != nil {
		return ports.SCMRepo{}, ErrInvalidPRRef
	}
	// The origin could not be classified by the configured providers, so fall
	// back to classifying by the PR URL's path shape (/pulls/ → forgejo, /pull/
	// → github, /-/merge_requests/ → gitlab). This keeps a forgejo PR routed to
	// the forgejo adapter even when no forgejo provider is configured.
	return ports.SCMRepo{Provider: parts.provider, Host: parts.host, Scheme: parts.scheme, Owner: parts.owner, Name: parts.name, Repo: parts.owner + "/" + parts.name}, nil
}

// providerKey maps a hostname to the normalized provider key used by the
// multi-provider dispatcher. GitHub hosts return "github"; everything else is
// treated as GitLab to match the multi-provider's registration order.
func providerKey(host string) string {
	host = strings.ToLower(host)
	if host == "github.com" || host == "www.github.com" || host == "api.github.com" ||
		strings.HasSuffix(host, ".github.com") || strings.HasSuffix(host, ".ghe.io") {
		return "github"
	}
	return "gitlab"
}

func claimRowsFromSCM(sessionID domain.SessionID, obs ports.SCMObservation, now time.Time, sessionRecord domain.SessionRecord) (domain.PullRequest, []domain.PullRequestCheck, []domain.PullRequestReview, []domain.PullRequestReviewThread, []domain.PullRequestComment) {
	observedAt := obs.ObservedAt
	if observedAt.IsZero() {
		observedAt = now
	}
	pr := domain.PullRequest{
		URL:                      firstNonEmpty(obs.PR.URL, obs.PR.HTMLURL),
		SessionID:                sessionID,
		Number:                   obs.PR.Number,
		Draft:                    obs.PR.Draft,
		Merged:                   obs.PR.Merged,
		Closed:                   obs.PR.Closed,
		CI:                       domain.CIState(firstNonEmpty(obs.CI.Summary, string(domain.CIUnknown))),
		Review:                   domain.ReviewDecision(firstNonEmpty(obs.Review.Decision, string(domain.ReviewNone))),
		Mergeability:             domain.Mergeability(firstNonEmpty(obs.Mergeability.State, string(domain.MergeUnknown))),
		UpdatedAt:                now,
		Provider:                 obs.Provider,
		Host:                     obs.Host,
		Repo:                     obs.Repo,
		SourceBranch:             obs.PR.SourceBranch,
		TargetBranch:             obs.PR.TargetBranch,
		HeadSHA:                  obs.PR.HeadSHA,
		Title:                    obs.PR.Title,
		Additions:                obs.PR.Additions,
		Deletions:                obs.PR.Deletions,
		ChangedFiles:             obs.PR.ChangedFiles,
		Author:                   obs.PR.Author,
		BaseSHA:                  obs.PR.BaseSHA,
		MergeCommitSHA:           obs.PR.MergeCommitSHA,
		ProviderState:            obs.PR.ProviderState,
		ProviderMergeable:        obs.PR.ProviderMergeable,
		ProviderMergeStateStatus: obs.PR.ProviderMergeStateStatus,
		HTMLURL:                  obs.PR.HTMLURL,
		CreatedAtProvider:        obs.PR.CreatedAtProvider,
		UpdatedAtProvider:        obs.PR.UpdatedAtProvider,
		MergedAtProvider:         obs.PR.MergedAtProvider,
		ClosedAtProvider:         obs.PR.ClosedAtProvider,
		ObservedAt:               observedAt,
		CIObservedAt:             observedAt,
		ReviewObservedAt:         observedAt,
	}
	checks := make([]domain.PullRequestCheck, 0, len(obs.CI.Checks))
	for _, ch := range obs.CI.Checks {
		checks = append(checks, domain.PullRequestCheck{Name: ch.Name, CommitHash: obs.CI.HeadSHA, Status: domain.PRCheckStatus(ch.Status), Conclusion: ch.Conclusion, URL: ch.URL, Details: ch.ProviderID, LogTail: ch.LogTail, CreatedAt: now})
	}
	reviews := make([]domain.PullRequestReview, 0, len(obs.Review.Reviews))
	for _, review := range obs.Review.Reviews {
		submittedAt := review.SubmittedAt
		if submittedAt.IsZero() {
			submittedAt = now
		}
		reviews = append(reviews, domain.PullRequestReview{
			ID:               review.ID,
			Author:           review.Author,
			State:            domain.ReviewDecision(firstNonEmpty(review.State, string(domain.ReviewNone))),
			URL:              review.URL,
			Body:             review.Body,
			IsBot:            review.IsBot,
			TargetSHA:        review.TargetSHA,
			SubmittedAt:      submittedAt,
			AutoInjectReview: sessionRecord.AutoInjectReview,
		})
	}
	threads := make([]domain.PullRequestReviewThread, 0, len(obs.Review.Threads))
	commentCount := 0
	for _, th := range obs.Review.Threads {
		commentCount += len(th.Comments)
	}
	comments := make([]domain.PullRequestComment, 0, commentCount)
	for _, th := range obs.Review.Threads {
		threads = append(threads, domain.PullRequestReviewThread{ThreadID: th.ID, Path: th.Path, Line: th.Line, Resolved: th.Resolved, IsBot: th.IsBot, UpdatedAt: now})
		for _, c := range th.Comments {
			comments = append(comments, domain.PullRequestComment{ThreadID: th.ID, ReviewID: c.ReviewID, ID: c.ID, Author: c.Author, File: th.Path, Line: th.Line, Body: c.Body, URL: c.URL, Resolved: th.Resolved, IsBot: c.IsBot || th.IsBot, CreatedAt: now, AutoInjectReview: sessionRecord.AutoInjectReview})
		}
	}
	return pr, checks, reviews, threads, comments
}

func sessionmanagerAPIError(code, message string) error {
	return apierr.Conflict(code, message, nil)
}

func (s *Service) listPRFacts(ctx context.Context, id domain.SessionID) ([]domain.PRFacts, error) {
	prs, err := s.store.ListPRsBySession(ctx, id)
	if err != nil {
		return nil, err
	}
	groups := groupPullRequestAliases(prs)
	facts := make([]domain.PRFacts, 0, len(groups))
	for _, group := range groups {
		var comments []domain.PullRequestComment
		for _, pr := range group.aliases {
			prComments, err := s.store.ListPRComments(ctx, pr.URL)
			if err != nil {
				return nil, err
			}
			comments = append(comments, prComments...)
		}
		facts = append(facts, pullRequestFacts(group.primary, comments))
	}
	sortPRFacts(facts)
	return facts, nil
}

func pullRequestFacts(pr domain.PullRequest, comments []domain.PullRequestComment) domain.PRFacts {
	unresolved := false
	for _, c := range comments {
		if !c.Resolved {
			unresolved = true
			break
		}
	}
	return domain.PRFacts{URL: pr.URL, Number: pr.Number, Draft: pr.Draft, Merged: pr.Merged, Closed: pr.Closed, CI: pr.CI, Review: pr.Review, Mergeability: pr.Mergeability, ReviewComments: unresolved, SourceBranch: pr.SourceBranch, TargetBranch: pr.TargetBranch, HeadSHA: pr.HeadSHA, UpdatedAt: pr.UpdatedAt}
}

func sortPRFacts(prs []domain.PRFacts) {
	sort.SliceStable(prs, func(i, j int) bool {
		ia, ja := prActive(prs[i]), prActive(prs[j])
		if ia != ja {
			return ia
		}
		return prs[i].UpdatedAt.After(prs[j].UpdatedAt)
	})
}

func prActive(pr domain.PRFacts) bool { return !pr.Merged && !pr.Closed }

func claimedFirst(prs []domain.PRFacts, prURL string) []domain.PRFacts {
	idx := -1
	for i, pr := range prs {
		if pr.URL == prURL {
			idx = i
			break
		}
	}
	if idx <= 0 {
		return prs
	}
	claimed := prs[idx]
	copy(prs[1:idx+1], prs[0:idx])
	prs[0] = claimed
	return prs
}

func normalizePRRef(ref, repoOrigin string, provider scmProvider) (string, int, error) {
	ref = strings.TrimPrefix(strings.TrimSpace(ref), "#")
	if ref == "" {
		return "", 0, ErrInvalidPRRef
	}
	if n, err := strconv.Atoi(ref); err == nil && n > 0 {
		host, owner, repo, err := repoFromURL(repoOrigin)
		if err != nil {
			return "", 0, ErrInvalidPRRef
		}
		// A numeric ref needs the origin's provider. The configured SCM
		// provider classifies it (a forgejo origin looks like any other
		// self-hosted git remote, so host heuristics cannot tell forgejo from
		// gitlab); fall back to the host-based default when no provider is
		// wired or it cannot classify.
		originProvider := providerFromOrigin(repoOrigin)
		if provider != nil {
			if repo, ok := provider.ParseRepository(repoOrigin); ok && repo.Provider != "" {
				originProvider = repo.Provider
			}
		}
		return prURLFromParts(host, owner, repo, n, originProvider, schemeOfRaw(repoOrigin)), n, nil
	}
	parts, err := parsePRURL(ref)
	if err != nil || parts.host == "" || parts.owner == "" || parts.name == "" || parts.number <= 0 {
		return "", 0, ErrInvalidPRRef
	}
	// The ref is a full PR URL: its path shape is authoritative for the
	// provider (parsePRURL classifies /pulls/ → forgejo, /pull/ → github,
	// /-/merge_requests/ → gitlab and rejects unrecognized shapes).
	return prURLFromParts(parts.host, parts.owner, parts.name, parts.number, parts.provider, parts.scheme), parts.number, nil
}

// providerFromOrigin classifies the provider of a project origin URL by host.
// github.com hosts are github; every other host is treated as gitlab (the
// pre-forgejo behavior). Forgejo is distinguished by PR URL shape (see
// parsePRURL), not by origin host, because a forgejo origin looks like any
// other self-hosted git remote. When the origin cannot be classified
// (empty), gitlab is the safe default so a numeric ref still resolves.
func providerFromOrigin(repoOrigin string) string {
	host := hostFromRaw(repoOrigin)
	if host == "" {
		return "gitlab"
	}
	host = strings.ToLower(host)
	if host == "github.com" || host == "www.github.com" || host == "api.github.com" ||
		strings.HasSuffix(host, ".github.com") || strings.HasSuffix(host, ".ghe.io") {
		return "github"
	}
	return "gitlab"
}

// hostFromRaw extracts the host from a git origin URL (https/ssh).
func hostFromRaw(raw string) string {
	if strings.HasPrefix(raw, "git@") {
		rest := strings.TrimPrefix(raw, "git@")
		colonIdx := strings.Index(rest, ":")
		if colonIdx < 0 {
			return ""
		}
		return rest[:colonIdx]
	}
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Host
}

// prURLFromParts constructs the canonical PR/MR URL for a provider and scheme.
// GitHub uses /pull/N; GitLab uses /-/merge_requests/N; Forgejo uses /pulls/N.
// The scheme (http/https) is preserved so a plain-HTTP self-hosted instance
// keeps its URL scheme in the persisted/claimed PR URL, which downstream code
// uses to derive the provider's API base scheme.
func prURLFromParts(host, owner, repo string, number int, provider, scheme string) string {
	if scheme != "http" && scheme != "https" {
		scheme = "https"
	}
	switch provider {
	case "github":
		return fmt.Sprintf("%s://%s/%s/%s/pull/%d", scheme, host, owner, repo, number)
	case "forgejo":
		return fmt.Sprintf("%s://%s/%s/%s/pulls/%d", scheme, host, owner, repo, number)
	default:
		return fmt.Sprintf("%s://%s/%s/%s/-/merge_requests/%d", scheme, host, owner, repo, number)
	}
}

// schemeOfRaw returns the http(s) scheme of a raw URL, or "" when absent.
func schemeOfRaw(raw string) string {
	if u, err := url.Parse(raw); err == nil && (u.Scheme == "http" || u.Scheme == "https") {
		return u.Scheme
	}
	return ""
}

func requireSameRepo(prURL, repoOrigin string) error {
	if strings.TrimSpace(repoOrigin) == "" {
		return nil
	}
	parts, err := parsePRURL(prURL)
	if err != nil {
		return ErrInvalidPRRef
	}
	originHost, originOwner, originRepo, err := repoFromURL(repoOrigin)
	if err != nil {
		return ErrInvalidPRRef
	}
	// Compare provider (derived from host) + host + full namespace. Same
	// owner/repo name on GitHub and GitLab must not validate, and a
	// gitlab.com origin must not accept a self-managed GitLab MR (review
	// finding #6).
	if !strings.EqualFold(parts.host, originHost) {
		return ErrProjectMismatch
	}
	if !strings.EqualFold(parts.owner, originOwner) || !strings.EqualFold(parts.name, originRepo) {
		return ErrProjectMismatch
	}
	return nil
}

// prURLParts is the parsed form of a PR/MR URL.
type prURLParts struct {
	host     string
	owner    string
	name     string
	number   int
	scheme   string
	provider string
}

// parsePRURL parses a PR/MR URL into its parts, classifying the provider by
// path shape: /pulls/N → forgejo (checked before the GitHub singular form),
// /pull/N → github, /-/merge_requests/N → gitlab. The host preserves any port
// (e.g. 127.0.0.1:3000) so a self-hosted/plain-HTTP instance matches its
// allowlist and builds the correct API base.
func parsePRURL(raw string) (prURLParts, error) {
	var out prURLParts
	u, err := url.Parse(raw)
	if err != nil {
		return out, err
	}
	if !strings.EqualFold(u.Scheme, "https") && !strings.EqualFold(u.Scheme, "http") {
		return out, ErrInvalidPRRef
	}
	out.scheme = strings.ToLower(u.Scheme)
	out.host = u.Host
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")

	// Forgejo: /owner/repo/pulls/N → 4 parts, parts[2] == "pulls" (PLURAL —
	// checked BEFORE the GitHub /pull/ form so the two never collide).
	if len(parts) == 4 && parts[2] == "pulls" {
		n, parseErr := strconv.Atoi(parts[3])
		if parseErr != nil || n <= 0 {
			return out, ErrInvalidPRRef
		}
		out.owner, out.name, out.number, out.provider = parts[0], strings.TrimSuffix(parts[1], ".git"), n, "forgejo"
		return out, nil
	}

	// GitHub: /owner/repo/pull/N → 4 parts, parts[2] == "pull"
	if len(parts) == 4 && parts[2] == "pull" {
		n, parseErr := strconv.Atoi(parts[3])
		if parseErr != nil || n <= 0 {
			return out, ErrInvalidPRRef
		}
		out.owner, out.name, out.number, out.provider = parts[0], strings.TrimSuffix(parts[1], ".git"), n, "github"
		return out, nil
	}

	// GitLab: /owner/repo/-/merge_requests/N
	// Supports nested groups: /group/subgroup/repo/-/merge_requests/N
	if len(parts) >= 5 && parts[len(parts)-2] == "merge_requests" && parts[len(parts)-3] == "-" {
		n, parseErr := strconv.Atoi(parts[len(parts)-1])
		if parseErr != nil || n <= 0 {
			return out, ErrInvalidPRRef
		}
		repoParts := parts[:len(parts)-3]
		if len(repoParts) < 2 {
			return out, ErrInvalidPRRef
		}
		out.owner = strings.Join(repoParts[:len(repoParts)-1], "/")
		out.name = strings.TrimSuffix(repoParts[len(repoParts)-1], ".git")
		out.number, out.provider = n, "gitlab"
		return out, nil
	}

	return out, ErrInvalidPRRef
}

func repoFromURL(raw string) (host, owner, name string, err error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", "", "", ErrInvalidPRRef
	}
	if strings.HasPrefix(raw, "git@") {
		rest := strings.TrimPrefix(raw, "git@")
		colonIdx := strings.Index(rest, ":")
		if colonIdx < 0 {
			return "", "", "", ErrInvalidPRRef
		}
		host = rest[:colonIdx]
		path := strings.TrimSuffix(rest[colonIdx+1:], ".git")
		parts := strings.Split(path, "/")
		if len(parts) < 2 {
			return "", "", "", ErrInvalidPRRef
		}
		for _, seg := range parts {
			if seg == "" {
				return "", "", "", ErrInvalidPRRef
			}
		}
		name = parts[len(parts)-1]
		owner = strings.Join(parts[:len(parts)-1], "/")
		return host, owner, name, nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", "", "", err
	}
	// u.Host preserves any port (e.g. gitlab.internal:8443 or 127.0.0.1:3000)
	// so self-managed and local instances match their allowlist entries and
	// build the correct API base.
	host = u.Host
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) < 2 {
		return "", "", "", ErrInvalidPRRef
	}
	for _, seg := range parts {
		if seg == "" {
			return "", "", "", ErrInvalidPRRef
		}
	}
	name = strings.TrimSuffix(parts[len(parts)-1], ".git")
	owner = strings.Join(parts[:len(parts)-1], "/")
	return host, owner, name, nil
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
