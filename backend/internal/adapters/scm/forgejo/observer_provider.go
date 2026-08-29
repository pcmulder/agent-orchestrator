package forgejo

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

const (
	reviewCommentLimitPerThread = 5
	fetchConcurrency            = 5
	perPage                     = 50
)

// ---------------------------------------------------------------------------
// ParseRepository
// ---------------------------------------------------------------------------

// ParseRepository parses a Forgejo remote URL and returns an SCMRepo if the
// host is in the provider's allowlist. Every Forgejo host is self-hosted, so a
// host that is not allowlisted is rejected — credentials are never attached to
// an untrusted host.
//
// Supported remote formats:
//
//   - HTTPS: https://forgejo.example.com/owner/repo.git
//   - HTTP:  http://127.0.0.1:3000/owner/repo.git     (scheme preserved)
//   - SSH:   git@forgejo.example.com:owner/repo.git
//   - SSH:   ssh://git@forgejo.example.com:3000/owner/repo.git
//
// The API scheme for the returned repo is taken from the remote URL when it is
// http(s); ssh remotes default to https.
func (p *Provider) ParseRepository(remote string) (ports.SCMRepo, bool) {
	remote = strings.TrimSpace(remote)
	if remote == "" {
		return ports.SCMRepo{}, false
	}

	// ssh://git@host[:port]/owner/repo.git
	if strings.HasPrefix(remote, "ssh://") {
		u, err := url.Parse(remote)
		if err != nil || u.Host == "" {
			return ports.SCMRepo{}, false
		}
		if !p.isHostAllowed(u.Host) {
			return ports.SCMRepo{}, false
		}
		owner, name, ok := splitOwnerRepo(strings.TrimSuffix(strings.TrimPrefix(u.Path, "/"), ".git"))
		if !ok {
			return ports.SCMRepo{}, false
		}
		return makeRepo(u.Host, "https", owner, name), true
	}

	// SSH scp-like: git@host:owner/repo.git  (also git@host:22,owner/repo is
	// not a git remote form; only the standard form is matched).
	if m := sshRemoteRe.FindStringSubmatch(remote); m != nil {
		host := m[1]
		if !p.isHostAllowed(host) {
			return ports.SCMRepo{}, false
		}
		owner, name, ok := splitOwnerRepo(strings.TrimSuffix(m[2], ".git"))
		if !ok {
			return ports.SCMRepo{}, false
		}
		return makeRepo(host, "https", owner, name), true
	}

	// http(s)://host[:port]/owner/repo.git
	u, err := url.Parse(remote)
	if err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != "" {
		host := u.Host // includes port if present, e.g. "127.0.0.1:3000"
		if !p.isHostAllowed(host) {
			return ports.SCMRepo{}, false
		}
		owner, name, ok := splitOwnerRepo(strings.TrimSuffix(strings.TrimPrefix(u.Path, "/"), ".git"))
		if !ok {
			return ports.SCMRepo{}, false
		}
		return makeRepo(host, u.Scheme, owner, name), true
	}

	return ports.SCMRepo{}, false
}

var sshRemoteRe = regexp.MustCompile(`^git@([^:/]+):(.+)$`)

// splitOwnerRepo splits an owner/repo path into owner (possibly nested) and
// name (the final segment). Single segments or empty parts are rejected.
func splitOwnerRepo(p string) (string, string, bool) {
	parts := strings.Split(p, "/")
	if len(parts) < 2 {
		return "", "", false
	}
	for _, seg := range parts {
		if seg == "" {
			return "", "", false
		}
	}
	name := parts[len(parts)-1]
	owner := strings.Join(parts[:len(parts)-1], "/")
	return owner, name, true
}

func makeRepo(host, scheme, owner, name string) ports.SCMRepo {
	return ports.SCMRepo{
		Provider: "forgejo",
		Host:     host,
		Scheme:   strings.ToLower(scheme),
		Owner:    owner,
		Name:     name,
		Repo:     owner + "/" + name,
	}
}

// ---------------------------------------------------------------------------
// RepoPRListGuard
// ---------------------------------------------------------------------------

// RepoPRListGuard reports whether the open-PR list of a repository should be
// refreshed. The Forgejo API has no ETag revalidation, so the guard fetches
// the open-PR listing and returns a sha256 fingerprint of it as the ETag:
// an unchanged listing yields NotModified=true so the observer skips the
// (cheaper) re-list and the expensive per-PR detail fetches, while any
// change (new PR, closed PR, edited PR) flips the guard. The observer's
// DefaultPRMaxAge backstop still bounds staleness for the head-SHA-unchanged
// case (a PR whose metadata changed without updating its head, e.g. a title
// edit with no push).
func (p *Provider) RepoPRListGuard(ctx context.Context, repo ports.SCMRepo, etag string) (ports.SCMGuardResult, error) {
	q := url.Values{"state": {"open"}, "limit": {strconv.Itoa(perPage)}}
	hc, err := p.clientForRepoErr(repo)
	if err != nil {
		return ports.SCMGuardResult{}, err
	}
	var body []byte
	_, err = hc.doGETPaginated(ctx, repoPath(repo.Owner, repo.Name, "pulls"), q, func(b []byte) error {
		body = append(body, b...)
		return nil
	})
	if err != nil {
		return ports.SCMGuardResult{}, err
	}
	h := sha256.Sum256(body)
	fp := hex.EncodeToString(h[:])
	return ports.SCMGuardResult{
		ETag:        fp,
		NotModified: etag == fp && etag != "",
	}, nil
}

// ---------------------------------------------------------------------------
// ListPRsByRepo
// ---------------------------------------------------------------------------

// ListPRsByRepo lists pull requests in a repository. The Forgejo API has no
// updated_after filter, so the full open listing is returned on every call
// (updatedAfter is ignored); terminal PRs are not listed, which matches the
// GitHub-style discovery the observer already handles (see
// reconcileTerminalGitHubPRs, which is GitHub-only — Forgejo open PRs that
// merge between polls are picked up by the lifecycle merge path).
func (p *Provider) ListPRsByRepo(ctx context.Context, repo ports.SCMRepo, _ time.Time) ([]ports.SCMPRObservation, error) {
	var result []ports.SCMPRObservation
	q := url.Values{"state": {"open"}, "limit": {strconv.Itoa(perPage)}}
	hc, err := p.clientForRepoErr(repo)
	if err != nil {
		return nil, err
	}

	_, err = hc.doGETPaginated(ctx, repoPath(repo.Owner, repo.Name, "pulls"), q, func(body []byte) error {
		var pulls []restPull
		if err := json.Unmarshal(body, &pulls); err != nil {
			return fmt.Errorf("forgejo scm: unmarshal PR list: %w", err)
		}
		for i := range pulls {
			result = append(result, pullToSCMPRObservation(repo, &pulls[i]))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// restPull is the subset of the Forgejo PullRequest response the provider uses.
// Forgejo's state is open/closed (merged PRs keep state=closed with
// merged_at set); draft and mergeable are independent booleans.
type restPull struct {
	ID        int64  `json:"id"`
	Number    int64  `json:"number"`
	Title     string `json:"title"`
	State     string `json:"state"`
	Draft     bool   `json:"draft"`
	HTMLURL   string `json:"html_url"`
	Mergeable bool   `json:"mergeable"`
	HasMerged bool   `json:"merged"`
	MergeBase string `json:"merge_base"`
	HeadSHA   string `json:"head_sha"`
	User      struct {
		Login string `json:"login"`
	} `json:"user"`
	Base struct {
		Ref  string `json:"ref"`
		Sha  string `json:"sha"`
		Repo struct {
			FullName string `json:"full_name"`
		} `json:"repo"`
	} `json:"base"`
	Head struct {
		Label string `json:"label"`
		Ref   string `json:"ref"`
		Sha   string `json:"sha"`
		Repo  struct {
			FullName string `json:"full_name"`
		} `json:"repo"`
	} `json:"head"`
	Created *time.Time `json:"created_at"`
	Updated *time.Time `json:"updated_at"`
	Merged  *time.Time `json:"merged_at"`
	Closed  *time.Time `json:"closed_at"`
}

func pullToSCMPRObservation(repo ports.SCMRepo, pr *restPull) ports.SCMPRObservation {
	merged := pr.HasMerged || (pr.Merged != nil && !pr.Merged.IsZero())
	closed := !merged && pr.State == "closed"
	headRepo := repo.Repo
	if pr.Head.Repo.FullName != "" && pr.Head.Ref != "" {
		// Fork PRs carry a distinct head repository full name; same-repo PRs
		// have head.full_name == repo.Repo.
		headRepo = pr.Head.Repo.FullName
	}
	return ports.SCMPRObservation{
		ProviderID:        providerID(pr.ID),
		URL:               pr.HTMLURL,
		HTMLURL:           pr.HTMLURL,
		Number:            int(pr.Number),
		State:             string(normalizePRState(pr.Draft, merged, closed)),
		Draft:             pr.Draft,
		Merged:            merged,
		Closed:            closed,
		SourceBranch:      pr.Head.Ref,
		HeadRepo:          headRepo,
		TargetBranch:      pr.Base.Ref,
		HeadSHA:           pr.Head.Sha,
		Title:             pr.Title,
		Author:            pr.User.Login,
		BaseSHA:           baseSHA(pr),
		ProviderState:     pr.State,
		ProviderMergeable: strconv.FormatBool(pr.Mergeable),
		CreatedAtProvider: safeTime(pr.Created),
		UpdatedAtProvider: safeTime(pr.Updated),
		MergedAtProvider:  safeTime(pr.Merged),
		ClosedAtProvider:  safeTime(pr.Closed),
	}
}

// baseSHA returns the base branch SHA of a pull request. Forgejo exposes
// the base ref SHA as base.sha; merge_base is the common ancestor and is a
// fallback when base.sha is absent.
func baseSHA(pr *restPull) string {
	if pr.Base.Sha != "" {
		return pr.Base.Sha
	}
	return pr.MergeBase
}

func providerID(id int64) string {
	if id <= 0 {
		return ""
	}
	return strconv.FormatInt(id, 10)
}

func normalizePRState(draft, merged, closed bool) domain.PRState {
	switch {
	case merged:
		return domain.PRStateMerged
	case closed:
		return domain.PRStateClosed
	case draft:
		return domain.PRStateDraft
	default:
		return domain.PRStateOpen
	}
}

func safeTime(t *time.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return *t
}

// ---------------------------------------------------------------------------
// CommitChecksGuard
// ---------------------------------------------------------------------------

// CommitChecksGuard returns a hash-based guard for the commit statuses of a
// head SHA, allowing the observer to skip unchanged CI state. The Forgejo API
// has no ETag, so the guard computes a sha256 fingerprint over the combined
// status set; a change in any status state flips the guard.
func (p *Provider) CommitChecksGuard(ctx context.Context, repo ports.SCMRepo, headSHA, etag string) (ports.SCMGuardResult, error) {
	if strings.TrimSpace(headSHA) == "" {
		return ports.SCMGuardResult{}, fmt.Errorf("forgejo scm: empty head SHA: %w", ErrNotFound)
	}
	path := repoPath(repo.Owner, repo.Name, "commits", headSHA, "statuses")
	hc, err := p.clientForRepoErr(repo)
	if err != nil {
		return ports.SCMGuardResult{}, err
	}
	resp, err := hc.doREST(ctx, http.MethodGet, path, nil)
	if err != nil {
		return ports.SCMGuardResult{}, err
	}
	var statuses []restStatus
	if err := json.Unmarshal(resp.Body, &statuses); err != nil {
		return ports.SCMGuardResult{}, fmt.Errorf("forgejo scm: unmarshal commit statuses: %w", err)
	}
	h := sha256.Sum256(resp.Body)
	newETag := hex.EncodeToString(h[:])
	_ = statuses
	return ports.SCMGuardResult{
		ETag:        newETag,
		NotModified: etag == newETag,
	}, nil
}

// restStatus is one commit status as returned by GET /commits/{sha}/statuses.
type restStatus struct {
	ID          int64  `json:"id"`
	State       string `json:"status"`
	Context     string `json:"context"`
	Description string `json:"description"`
	TargetURL   string `json:"target_url"`
	URL         string `json:"url"`
	Created     string `json:"created_at"`
}

// ---------------------------------------------------------------------------
// FetchPullRequests
// ---------------------------------------------------------------------------

// FetchPullRequests fetches detailed observations for a batch of pull
// requests, including PR metadata, CI statuses, reviews, and mergeability.
//
// A transient failure (timeout, 429, 5xx, malformed payload) on any sub-fetch
// is propagated as a non-nil error AND a Fetched=false placeholder observation
// at the same index, so the observer preserves the last durable state and the
// sync cursor is not advanced.
func (p *Provider) FetchPullRequests(ctx context.Context, refs []ports.SCMPRRef) ([]ports.SCMObservation, error) {
	if len(refs) > 25 {
		return nil, fmt.Errorf("forgejo scm: batch size %d exceeds limit 25", len(refs))
	}
	results := make([]ports.SCMObservation, len(refs))
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, fetchConcurrency)
	var firstErr error

	for i, ref := range refs {
		wg.Add(1)
		go func(idx int, r ports.SCMPRRef) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			obs, err := p.fetchSinglePR(ctx, r)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				p.logger.Warn("forgejo scm: fetch PR failed", "repo", r.Repo.Repo, "pr", r.Number, "err", err)
				if firstErr == nil {
					firstErr = err
				}
				results[idx] = ports.SCMObservation{
					Fetched:  false,
					Provider: "forgejo",
					Host:     r.Repo.Host,
					Repo:     r.Repo.Repo,
					PR:       ports.SCMPRObservation{Number: r.Number, URL: r.URL},
					Error:    err,
				}
				return
			}
			results[idx] = obs
		}(i, ref)
	}
	wg.Wait()
	return results, firstErr
}

func (p *Provider) fetchSinglePR(ctx context.Context, ref ports.SCMPRRef) (ports.SCMObservation, error) {
	repo := ref.Repo
	now := time.Now()

	hc, err := p.clientForRepoErr(repo)
	if err != nil {
		return ports.SCMObservation{}, err
	}

	// 1. Fetch PR detail.
	prPath := repoPath(repo.Owner, repo.Name, "pulls", strconv.Itoa(ref.Number))
	resp, err := hc.doREST(ctx, http.MethodGet, prPath, nil)
	if err != nil {
		return ports.SCMObservation{}, err
	}
	var pr restPull
	if err := json.Unmarshal(resp.Body, &pr); err != nil {
		return ports.SCMObservation{}, fmt.Errorf("forgejo scm: unmarshal PR detail: %w", err)
	}

	prObs := pullToSCMPRObservation(repo, &pr)
	if requestedURL := strings.TrimSpace(ref.URL); requestedURL != "" && requestedURL != strings.TrimSpace(prObs.URL) {
		prObs.URLAlias = requestedURL
	}
	prObs.MergeCommitSHA = ""
	if pr.HasMerged {
		// merge_commit_sha is only populated after merge; best effort.
		prObs.MergeCommitSHA = ""
	}

	// 2. Fetch CI (commit statuses). A transient failure propagates so the
	// observer preserves the last durable CI state rather than overwriting it
	// with "unknown".
	ciObs, err := p.fetchCI(ctx, repo, pr.Head.Sha)
	if err != nil {
		return ports.SCMObservation{}, fmt.Errorf("forgejo scm: fetch CI: %w", err)
	}

	// 3. Fetch reviews for the review decision and summaries.
	reviews, reviewDecision, err := p.fetchReviews(ctx, repo, ref.Number)
	if err != nil {
		return ports.SCMObservation{}, fmt.Errorf("forgejo scm: fetch reviews: %w", err)
	}

	// 4. Build mergeability from the mergeable bool + layered blockers.
	mergeObs := mergeabilityFromPR(&pr, ciObs.Summary, string(reviewDecision))

	return ports.SCMObservation{
		Fetched:      true,
		ObservedAt:   now,
		Provider:     "forgejo",
		Host:         repo.Host,
		Repo:         repo.Repo,
		PR:           prObs,
		CI:           ciObs,
		Review:       ports.SCMReviewObservation{Decision: string(reviewDecision), Reviews: reviews},
		Mergeability: mergeObs,
	}, nil
}

func (p *Provider) fetchCI(ctx context.Context, repo ports.SCMRepo, headSHA string) (ports.SCMCIObservation, error) {
	if headSHA == "" {
		return ports.SCMCIObservation{Summary: string(domain.CIUnknown)}, nil
	}
	path := repoPath(repo.Owner, repo.Name, "commits", headSHA, "statuses")
	hc, err := p.clientForRepoErr(repo)
	if err != nil {
		return ports.SCMCIObservation{}, err
	}
	resp, err := hc.doREST(ctx, http.MethodGet, path, nil)
	if err != nil {
		return ports.SCMCIObservation{}, fmt.Errorf("fetch commit statuses: %w", err)
	}
	var statuses []restStatus
	if err := json.Unmarshal(resp.Body, &statuses); err != nil {
		return ports.SCMCIObservation{}, fmt.Errorf("unmarshal commit statuses: %w", err)
	}
	if len(statuses) == 0 {
		return ports.SCMCIObservation{Summary: string(domain.CIUnknown), HeadSHA: headSHA}, nil
	}

	var checks, failed []ports.SCMCheckObservation
	for _, s := range statuses {
		status := statusStateToCheckStatus(s.State)
		check := ports.SCMCheckObservation{
			Name:       s.Context,
			Status:     string(status),
			Conclusion: s.State,
			URL:        firstNonEmpty(s.TargetURL, s.URL),
			ProviderID: strconv.FormatInt(s.ID, 10),
			LogTail:    s.Description,
		}
		checks = append(checks, check)
		if isFailingCheckStatus(status) {
			failed = append(failed, check)
		}
	}

	summary := combinedCIState(statuses)
	return ports.SCMCIObservation{
		Summary:           string(summary),
		HeadSHA:           headSHA,
		FailedFingerprint: forgejoFailedFingerprint(headSHA, failed),
		Checks:            checks,
		FailedChecks:      failed,
	}, nil
}

// combinedCIState derives the aggregate CI state from the status set, using
// the same priority order Forgejo's combined status endpoint uses: error and
// failure dominate, then warning/pending, then success.
func combinedCIState(statuses []restStatus) domain.CIState {
	summary := domain.CIUnknown
	for _, s := range statuses {
		switch domain.CIState(statusToCI(s.State)) {
		case domain.CIFailing:
			summary = domain.CIFailing
		case domain.CIPending:
			if summary != domain.CIFailing {
				summary = domain.CIPending
			}
		case domain.CIPassing:
			if summary == domain.CIUnknown {
				summary = domain.CIPassing
			}
		}
	}
	if summary == domain.CIUnknown {
		summary = domain.CIPending
	}
	return summary
}

func statusToCI(state string) string {
	switch state {
	case "success":
		return string(domain.CIPassing)
	case "error", "failure":
		return string(domain.CIFailing)
	case "pending", "warning":
		return string(domain.CIPending)
	default:
		return string(domain.CIUnknown)
	}
}

func statusStateToCheckStatus(state string) domain.PRCheckStatus {
	switch state {
	case "success":
		return domain.PRCheckPassed
	case "error", "failure":
		return domain.PRCheckFailed
	case "pending":
		return domain.PRCheckInProgress
	default:
		return domain.PRCheckUnknown
	}
}

func isFailingCheckStatus(s domain.PRCheckStatus) bool {
	return s == domain.PRCheckFailed
}

// ---------------------------------------------------------------------------
// FetchFailedCheckLogTail
// ---------------------------------------------------------------------------

// FetchFailedCheckLogTail returns the description of a failed commit status.
// Forgejo statuses carry a short description (and target URL) rather than a
// CI job log, so the stored log tail (set from the status description at fetch
// time) is the most useful tail available. A missing status id is an error so
// the observer surfaces the lookup failure instead of silently dropping it.
func (p *Provider) FetchFailedCheckLogTail(ctx context.Context, repo ports.SCMRepo, check ports.SCMCheckObservation) (string, error) {
	if p == nil || p.client == nil {
		return "", fmt.Errorf("forgejo scm: log tail provider is not configured")
	}
	if check.ProviderID == "" || strings.TrimSpace(repo.Owner) == "" || strings.TrimSpace(repo.Name) == "" {
		return "", fmt.Errorf("forgejo scm: empty status id or repository")
	}
	if check.LogTail != "" {
		return check.LogTail, nil
	}
	return "", nil
}

// ---------------------------------------------------------------------------
// FetchReviewThreads
// ---------------------------------------------------------------------------

// FetchReviewThreads fetches file review threads and review summaries for a
// pull request. Threads are reconstructed from the file list plus issue
// timeline comments (Forgejo has no dedicated thread endpoint); the review
// decision and summaries come from the reviews list.
func (p *Provider) FetchReviewThreads(ctx context.Context, ref ports.SCMPRRef) (ports.SCMReviewObservation, error) {
	repo := ref.Repo
	hc, err := p.clientForRepoErr(repo)
	if err != nil {
		return ports.SCMReviewObservation{}, err
	}

	// 1. File list — the set of files with review comments.
	filesResp, err := hc.doREST(ctx, http.MethodGet, repoPath(repo.Owner, repo.Name, "pulls", strconv.Itoa(ref.Number), "files"), nil)
	if err != nil {
		return ports.SCMReviewObservation{}, err
	}
	var files []restChangedFile
	if err := json.Unmarshal(filesResp.Body, &files); err != nil {
		return ports.SCMReviewObservation{}, fmt.Errorf("forgejo scm: unmarshal files: %w", err)
	}

	// 2. Issue timeline — all comments (including file review comments, which
	//    carry a type of "code"). Grouped by file.
	var timeline []restTimelineComment
	truncated, err := hc.doGETPaginated(ctx, repoPath(repo.Owner, repo.Name, "issues", strconv.Itoa(ref.Number), "timeline"), nil, func(body []byte) error {
		var page []restTimelineComment
		if err := json.Unmarshal(body, &page); err != nil {
			return fmt.Errorf("forgejo scm: unmarshal timeline: %w", err)
		}
		timeline = append(timeline, page...)
		return nil
	})
	if err != nil {
		return ports.SCMReviewObservation{}, err
	}

	// 3. Reviews for the decision + summaries.
	reviews, decision, err := p.fetchReviews(ctx, repo, ref.Number)
	if err != nil {
		return ports.SCMReviewObservation{}, fmt.Errorf("forgejo scm: %w", err)
	}

	commentsByFile := groupCodeCommentsByFile(timeline)
	var threads []ports.SCMReviewThreadObservation
	for _, f := range files {
		comments := commentsByFile[f.Filename]
		if len(comments) == 0 {
			continue
		}
		threads = append(threads, threadFromFileComments(ref, f.Filename, comments))
	}

	return ports.SCMReviewObservation{
		Decision: string(decision),
		Reviews:  reviews,
		Threads:  threads,
		Partial:  truncated,
	}, nil
}

type restChangedFile struct {
	Filename string `json:"filename"`
	Status   string `json:"status"`
}

type restTimelineComment struct {
	ID       int64  `json:"id"`
	Type     string `json:"type"`
	Body     string `json:"body"`
	HTMLURL  string `json:"html_url"`
	ReviewID int64  `json:"review_id"`
	User     struct {
		Login string `json:"login"`
	} `json:"user"`
	Created string `json:"created_at"`
}

type restReview struct {
	ID        int64  `json:"id"`
	State     string `json:"state"`
	Body      string `json:"body"`
	Commit    string `json:"commit_id"`
	Dismissed bool   `json:"dismissed"`
	User      struct {
		Login string `json:"login"`
	} `json:"user"`
	Submitted string `json:"submitted_at"`
}

// fetchReviews returns review summaries + the aggregate review decision for a
// pull request.
func (p *Provider) fetchReviews(ctx context.Context, repo ports.SCMRepo, number int) ([]ports.SCMReviewSummaryObservation, domain.ReviewDecision, error) {
	hc, err := p.clientForRepoErr(repo)
	if err != nil {
		return nil, domain.ReviewNone, err
	}
	var reviews []restReview
	path := repoPath(repo.Owner, repo.Name, "pulls", strconv.Itoa(number), "reviews")
	if _, err := hc.doGETPaginated(ctx, path, nil, func(body []byte) error {
		var page []restReview
		if err := json.Unmarshal(body, &page); err != nil {
			return fmt.Errorf("forgejo scm: unmarshal reviews: %w", err)
		}
		reviews = append(reviews, page...)
		return nil
	}); err != nil {
		return nil, domain.ReviewNone, err
	}

	var summaries []ports.SCMReviewSummaryObservation
	decision := domain.ReviewNone
	for _, r := range reviews {
		if r.Dismissed {
			continue
		}
		switch r.State {
		case "REQUEST_CHANGES":
			decision = domain.ReviewChangesRequest
		case "APPROVED":
			if decision != domain.ReviewChangesRequest {
				decision = domain.ReviewApproved
			}
		}
		if r.State == "APPROVED" || r.State == "REQUEST_CHANGES" {
			summaries = append(summaries, ports.SCMReviewSummaryObservation{
				ID:          strconv.FormatInt(r.ID, 10),
				Author:      r.User.Login,
				State:       string(normalizeReviewState(r.State)),
				URL:         "",
				Body:        r.Body,
				TargetSHA:   r.Commit,
				IsBot:       isBotAuthor(r.User.Login),
				SubmittedAt: parseForgejoTime(r.Submitted),
			})
		}
	}
	return summaries, decision, nil
}

func normalizeReviewState(state string) domain.ReviewDecision {
	switch state {
	case "APPROVED":
		return domain.ReviewApproved
	case "REQUEST_CHANGES":
		return domain.ReviewChangesRequest
	default:
		return domain.ReviewNone
	}
}

// groupCodeCommentsByFile groups timeline comments of type "code" by file.
// The "code" comment type marks file review comments; the file name is
// extracted from the comment's HTML URL tail (Forgejo file comments anchor to
// /pulls/N#issuecomment or carry the path in the timeline; when the file is
// not derivable the comment is skipped).
func groupCodeCommentsByFile(timeline []restTimelineComment) map[string][]restTimelineComment {
	out := make(map[string][]restTimelineComment)
	for _, c := range timeline {
		if c.Type != "code" {
			continue
		}
		file := fileFromCommentHTMLURL(c.HTMLURL)
		if file == "" {
			continue
		}
		out[file] = append(out[file], c)
	}
	return out
}

// fileFromCommentHTMLURL extracts a file path from a review-comment HTML URL.
// Forgejo anchors file comments as .../pulls/N#diff-<base64-ish-file>; the
// exact encoding is instance-version dependent, so this is best effort and
// returns "" when no recognizable file anchor is present.
func fileFromCommentHTMLURL(raw string) string {
	if raw == "" {
		return ""
	}
	if idx := strings.Index(raw, "#diff-"); idx >= 0 {
		return raw[idx+len("#diff-"):]
	}
	return ""
}

func threadFromFileComments(ref ports.SCMPRRef, file string, comments []restTimelineComment) ports.SCMReviewThreadObservation {
	// Deterministic thread id: file + first comment id.
	id := "file:" + file
	if len(comments) > 0 {
		id = strconv.FormatInt(comments[0].ID, 10)
	}
	allBot := true
	var out []ports.SCMReviewCommentObservation
	for i, c := range comments {
		isBot := isBotAuthor(c.User.Login)
		if !isBot {
			allBot = false
		}
		if i < reviewCommentLimitPerThread {
			out = append(out, ports.SCMReviewCommentObservation{
				ID:     strconv.FormatInt(c.ID, 10),
				Author: c.User.Login,
				Body:   c.Body,
				URL:    firstNonEmpty(c.HTMLURL, ref.URL),
				IsBot:  isBot,
			})
		}
	}
	return ports.SCMReviewThreadObservation{
		ID:       id,
		Path:     file,
		Resolved: false,
		IsBot:    allBot,
		Comments: out,
	}
}

func parseForgejoTime(raw string) time.Time {
	if raw == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}
	}
	return t
}

// ---------------------------------------------------------------------------
// Mergeability
// ---------------------------------------------------------------------------

func mergeabilityFromPR(pr *restPull, ciState, reviewDecision string) ports.SCMMergeabilityObservation {
	merged := pr.HasMerged || (pr.Merged != nil && !pr.Merged.IsZero())
	if merged {
		return ports.SCMMergeabilityObservation{
			State:     string(domain.MergeMergeable),
			Mergeable: true,
		}
	}
	if !pr.Mergeable {
		return ports.SCMMergeabilityObservation{
			State:    string(domain.MergeConflicting),
			Conflict: true,
			Blockers: []string{"conflicts"},
		}
	}
	var blockers []string
	mergeable := true
	if ciState == string(domain.CIFailing) {
		blockers = append(blockers, "ci_failing")
		mergeable = false
	}
	if reviewDecision == string(domain.ReviewChangesRequest) {
		blockers = append(blockers, "changes_requested")
		mergeable = false
	}
	if pr.Draft {
		blockers = append(blockers, "draft")
		mergeable = false
	}
	if mergeable {
		return ports.SCMMergeabilityObservation{
			State:     string(domain.MergeMergeable),
			Mergeable: true,
		}
	}
	return ports.SCMMergeabilityObservation{
		State:    string(domain.MergeBlocked),
		Blockers: blockers,
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

var botUsernameRe = regexp.MustCompile(`^project_\d+_bot`)

func isBotAuthor(username string) bool {
	if strings.HasSuffix(username, "[bot]") || strings.HasSuffix(username, "-bot") {
		return true
	}
	switch username {
	case "gitlab-bot", "ghost", "dependabot[bot]", "renovate[bot]", "forgejo-bot":
		return true
	}
	return botUsernameRe.MatchString(username)
}

func forgejoFailedFingerprint(headSHA string, checks []ports.SCMCheckObservation) string {
	if len(checks) == 0 {
		return ""
	}
	parts := make([]string, len(checks))
	for i, c := range checks {
		parts[i] = headSHA + "\x00" + c.Name + "\x00" + c.Status + "\x00" + c.Conclusion + "\x00" + c.URL + "\x00" + c.ProviderID
	}
	sort.Strings(parts)
	h := sha256.Sum256([]byte(strings.Join(parts, "\x1e")))
	return hex.EncodeToString(h[:])
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
