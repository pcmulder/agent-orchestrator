package forgejo

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	scmforgejo "github.com/aoagents/agent-orchestrator/backend/internal/adapters/scm/forgejo"
	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/tracker/httpkit"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

const (
	defaultUserAgent = "ao-agent-orchestrator/tracker-forgejo"

	// Gitea/Forgejo page size; ListFilter.Limit is an optional
	// total-result cap, so page size stays at a reasonable value.
	listPageSize = 50
	// Guard against a pathological Link cycle.
	maxListPages = 50
)

// Sentinel errors. Adapter-level callers should match on these via
// errors.Is; the orchestrator's lifecycle code is intentionally insulated
// from raw HTTP status codes.
var (
	ErrNotFound       = errors.New("forgejo tracker: issue not found")
	ErrRateLimited    = errors.New("forgejo tracker: rate limited")
	ErrAuthFailed     = errors.New("forgejo tracker: authentication failed")
	ErrWrongProvider  = errors.New("forgejo tracker: id is not a forgejo tracker id")
	ErrBadID          = errors.New("forgejo tracker: malformed native id")
	ErrHostNotAllowed = errors.New("forgejo tracker: host not in allowlist")
)

// RateLimitError is an alias for httpkit.RateLimitError so existing callers
// that use errors.As with *RateLimitError continue to work.
type RateLimitError = httpkit.RateLimitError

// Options configures a Tracker. All fields except Token are optional —
// production code typically sets Token + AllowedHosts; tests inject
// HTTPClient and BaseURLs to point at an httptest fake.
//
// AllowedHosts is the list of self-hosted Forgejo hosts the tracker is
// permitted to talk to. Unlike GitLab, every host must appear here (there is
// no default public host); a host not in this list is rejected before any
// credential is attached.
//
// HostTokens maps a host to a token override. Hosts in AllowedHosts without
// an explicit entry fall back to the default Token.
type Options struct {
	Token        scmforgejo.TokenSource
	HTTPClient   *http.Client
	UserAgent    string
	AllowedHosts []string
	HostTokens   map[string]scmforgejo.TokenSource
	// BaseURLs overrides the per-host API base URL. When a host has an entry
	// here (keyed by normalized host), that base is used instead of the
	// derived <scheme>://<host>/api/v1. Tests use this to point at an
	// httptest server.
	BaseURLs map[string]string
}

// hostEntry holds the per-host base URL and token source.
type hostEntry struct {
	baseURL string
	tokens  scmforgejo.TokenSource
}

// Tracker implements ports.Tracker against the Forgejo REST API v1.
//
// Construction performs a fail-fast token presence check (no network call).
// The first Preflight call validates the token against the configured host;
// a successful preflight is cached for the lifetime of the Tracker.
//
// The tracker is host-aware: it maintains a per-host base URL + token map.
// Unlike GitLab, there is no default public host — every host must be in
// AllowedHosts. Unconfigured hosts are rejected before any credential is
// attached.
type Tracker struct {
	http      *http.Client
	userAgent string

	// hosts maps each allowed self-hosted host to its config (base URL +
	// token).
	hosts map[string]hostEntry

	// hostOrder preserves the AllowedHosts order so Preflight can pick a
	// deterministic host when none is specified.
	hostOrder []string

	preflight httpkit.PreflightCache
}

// defaultSchemeForHost derives the API URL scheme for a host. Loopback hosts
// (127.0.0.1, localhost, ::1) default to http so a local test instance is
// reachable without TLS; all other hosts default to https. Callers can
// override per-host via Options.BaseURLs.
func defaultSchemeForHost(host string) string {
	h := scmforgejo.NormalizeHost(host)
	// SplitHostPort returns (host, "") with an error for hostnames without a
	// port; the host part is all we need here.
	name, _, _ := net.SplitHostPort(h)
	if name == "" {
		name = h
	}
	if name == "localhost" {
		return "http"
	}
	if ip := net.ParseIP(name); ip != nil && ip.IsLoopback() {
		return "http"
	}
	return "https"
}

// New returns a Tracker. It fails fast when no token can be obtained so
// daemons crash at startup rather than at first issue lookup.
func New(opts Options) (*Tracker, error) {
	src := opts.Token
	if src == nil {
		return nil, ErrNoToken
	}
	if _, err := src.Token(context.Background()); err != nil {
		return nil, err
	}
	ua := opts.UserAgent
	if ua == "" {
		ua = defaultUserAgent
	}

	// Build per-host config for every allowlisted host.
	hosts := make(map[string]hostEntry, len(opts.AllowedHosts))
	hostOrder := make([]string, 0, len(opts.AllowedHosts))
	for _, raw := range opts.AllowedHosts {
		h := scmforgejo.NormalizeHost(raw)
		if h == "" {
			continue
		}
		he := hostEntry{
			baseURL: defaultSchemeForHost(h) + "://" + h + "/api/v1",
			tokens:  src, // fall back to default token
		}
		if base, ok := opts.BaseURLs[h]; ok && base != "" {
			he.baseURL = strings.TrimRight(base, "/")
		}
		if ts, ok := opts.HostTokens[h]; ok && ts != nil {
			he.tokens = ts
		}
		hosts[h] = he
		hostOrder = append(hostOrder, h)
	}

	t := &Tracker{
		http:      opts.HTTPClient,
		userAgent: ua,
		hosts:     hosts,
		hostOrder: hostOrder,
	}
	if t.http == nil {
		t.http = &http.Client{Timeout: 30 * time.Second}
	}
	return t, nil
}

// configForHost returns the per-host config (base URL + token) for the given
// host. Forgejo has no default public host, so an empty host is an error.
// Self-hosted hosts must be in the allowlist; unconfigured hosts return an
// error so callers fail closed before any credential is attached.
func (t *Tracker) configForHost(host string) (hostEntry, error) {
	host = scmforgejo.NormalizeHost(host)
	if host == "" {
		return hostEntry{}, fmt.Errorf("forgejo tracker: host is required (no default host): %w", ErrHostNotAllowed)
	}
	if he, ok := t.hosts[host]; ok {
		return he, nil
	}
	return hostEntry{}, fmt.Errorf("forgejo tracker: host %q not in allowlist: %w", host, ErrHostNotAllowed)
}

// firstHost returns the first allowlisted host in AllowedHosts order, or ""
// when none is configured. Used by Preflight to pick a host to probe.
func (t *Tracker) firstHost() string {
	if len(t.hostOrder) > 0 {
		return t.hostOrder[0]
	}
	return ""
}

// ConfigForHost returns nil if the given host is allowed by the tracker, or
// ErrHostNotAllowed otherwise. No network call is made — this is safe for use
// in wiring tests that need to assert host routing without DNS resolution.
func (t *Tracker) ConfigForHost(host string) error {
	_, err := t.configForHost(host)
	return err
}

// Statically assert Tracker satisfies the port. If this stops compiling, the
// port shape changed and the adapter needs to follow.
var _ ports.Tracker = (*Tracker)(nil)

// ---------------------------------------------------------------------------
// Get
// ---------------------------------------------------------------------------

// fIssue is the subset of fields we read off the Forgejo issue payload.
// The Forgejo issues endpoint returns both issues and pull requests; PR rows
// carry a non-nil pull_request field and are excluded by the caller.
type fIssue struct {
	IID         int        `json:"number"`
	ID          int64      `json:"id"`
	Title       string     `json:"title"`
	Body        string     `json:"body"`
	State       string     `json:"state"`
	HTMLURL     string     `json:"html_url"`
	Labels      []fLabel   `json:"labels"`
	Assignees   []fUser    `json:"assignees"`
	PullRequest *fPullMeta `json:"pull_request"`
}

type fLabel struct {
	Name string `json:"name"`
}

type fUser struct {
	Login string `json:"login"`
}

type fPullMeta struct {
	HasMerged bool `json:"merged"`
}

// Get fetches a single issue by id and maps it onto the normalized domain.Issue.
func (t *Tracker) Get(ctx context.Context, id domain.TrackerID) (domain.Issue, error) {
	repoPath, number, err := t.parseID(id)
	if err != nil {
		return domain.Issue{}, err
	}
	he, err := t.configForHost(id.Host)
	if err != nil {
		return domain.Issue{}, err
	}
	path := fmt.Sprintf("/repos/%s/issues/%d", url.PathEscape(repoPath), number)

	resp, err := t.do(ctx, he, http.MethodGet, path, nil)
	if err != nil {
		return domain.Issue{}, err
	}
	var raw fIssue
	if err := json.Unmarshal(resp, &raw); err != nil {
		return domain.Issue{}, fmt.Errorf("forgejo tracker: decode issue: %w", err)
	}
	issue := issueFromForgejo(repoPath, raw)
	issue.ID.Host = id.Host // preserve host for round-trip
	return issue, nil
}

// issueFromForgejo projects a raw Forgejo issue payload into the normalized
// domain.Issue. repoPath is passed in because the TrackerID.Native shape is
// "owner/repo#number" and we want the returned ID to round-trip through the
// same adapter.
func issueFromForgejo(repoPath string, raw fIssue) domain.Issue {
	labels := make([]string, 0, len(raw.Labels))
	for _, l := range raw.Labels {
		labels = append(labels, l.Name)
	}
	assignees := make([]string, 0, len(raw.Assignees))
	for _, a := range raw.Assignees {
		assignees = append(assignees, a.Login)
	}
	out := domain.Issue{
		ID: domain.TrackerID{
			Provider: domain.TrackerProviderForgejo,
			Native:   fmt.Sprintf("%s#%d", repoPath, raw.IID),
		},
		Title:     raw.Title,
		Body:      raw.Body,
		State:     mapStateFromForgejo(raw.State),
		URL:       raw.HTMLURL,
		Labels:    labels,
		Assignees: assignees,
	}
	if len(out.Labels) == 0 {
		out.Labels = nil
	}
	if len(out.Assignees) == 0 {
		out.Assignees = nil
	}
	return out
}

// mapStateFromForgejo projects Forgejo's open/closed state onto the normalized
// state vocabulary.
func mapStateFromForgejo(state string) domain.NormalizedIssueState {
	if strings.EqualFold(state, "closed") {
		return domain.IssueDone
	}
	return domain.IssueOpen
}

// ---------------------------------------------------------------------------
// List
// ---------------------------------------------------------------------------

// List returns issues for a repository, filtered by state/labels/assignee.
// Pagination is followed via the Link header (rel="next") until no next link
// remains; ListFilter.Limit, when set, caps the total accumulated issue count.
// Pull-request rows are filtered out (the Forgejo issues endpoint returns
// both issues and PRs).
func (t *Tracker) List(ctx context.Context, repo domain.TrackerRepo, filter domain.ListFilter) ([]domain.Issue, error) {
	if repo.Provider != domain.TrackerProviderForgejo {
		return nil, fmt.Errorf("%w: provider=%q", ErrWrongProvider, repo.Provider)
	}
	repoPath, err := parseForgejoRepo(repo.Native)
	if err != nil {
		return nil, err
	}
	he, err := t.configForHost(repo.Host)
	if err != nil {
		return nil, err
	}

	q := url.Values{}
	switch filter.State {
	case domain.ListOpen:
		q.Set("state", "open")
	case domain.ListClosed:
		q.Set("state", "closed")
	default:
		q.Set("state", "all")
	}
	if len(filter.Labels) > 0 {
		q.Set("labels", strings.Join(filter.Labels, ","))
	}
	if filter.Assignee != "" {
		q.Set("assignee", filter.Assignee)
	}
	q.Set("limit", strconv.Itoa(listPageSize))

	base := "/repos/" + url.PathEscape(repoPath) + "/issues"
	path := base + "?" + q.Encode()
	out := make([]domain.Issue, 0)
	if filter.Limit > 0 {
		out = make([]domain.Issue, 0, filter.Limit)
	}
	for page := 0; path != ""; page++ {
		if page >= maxListPages {
			return nil, fmt.Errorf("forgejo tracker: list pagination exceeded %d pages", maxListPages)
		}
		respBody, nextPath, err := t.roundTrip(ctx, he, http.MethodGet, path, nil)
		if err != nil {
			return nil, err
		}
		var raw []fIssue
		if err := json.Unmarshal(respBody, &raw); err != nil {
			return nil, fmt.Errorf("forgejo tracker: decode list: %w", err)
		}
		pageIssues := make([]domain.Issue, 0, len(raw))
		for _, r := range raw {
			// Skip pull-request rows; they are not issues.
			if r.PullRequest != nil {
				continue
			}
			issue := issueFromForgejo(repoPath, r)
			issue.ID.Host = repo.Host // preserve host for round-trip
			pageIssues = append(pageIssues, issue)
		}
		var done bool
		out, done = httpkit.AppendIssuesWithLimit(out, pageIssues, filter.Limit)
		if done {
			break
		}
		path = nextPath
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Preflight
// ---------------------------------------------------------------------------

// Preflight verifies the configured token is currently accepted by Forgejo
// (one GET /user against the first allowlisted host). It does NOT prove the
// token has the scope or visibility needed for any specific Get/List call.
//
// Successful checks are cached via httpkit.PreflightCache (double-checked
// atomic+mutex). Failures are intentionally NOT cached.
func (t *Tracker) Preflight(ctx context.Context) error {
	if len(t.hostOrder) == 0 {
		// No host configured — nothing to preflight against.
		return nil
	}
	host := t.firstHost()
	return t.preflight.Run(ctx, func(ctx context.Context) error {
		he, err := t.configForHost(host)
		if err != nil {
			return err
		}
		_, err = t.do(ctx, he, http.MethodGet, "/user", nil)
		return err
	})
}

// ---------------------------------------------------------------------------
// HTTP plumbing
// ---------------------------------------------------------------------------

func (t *Tracker) do(ctx context.Context, he hostEntry, method, path string, body any) ([]byte, error) {
	respBody, _, err := t.roundTrip(ctx, he, method, path, body)
	return respBody, err
}

func (t *Tracker) roundTrip(ctx context.Context, he hostEntry, method, path string, body any) ([]byte, string, error) {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, "", fmt.Errorf("forgejo tracker: encode body: %w", err)
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, he.baseURL+path, rdr)
	if err != nil {
		return nil, "", fmt.Errorf("forgejo tracker: build request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", t.userAgent)
	tok, err := he.tokens.Token(ctx)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("Authorization", "Bearer "+tok)

	resp, err := t.http.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("forgejo tracker: %s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	nextPath := httpkit.ParseLinkNext(resp.Header.Get("Link"), he.baseURL)
	respBody, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		return nil, "", fmt.Errorf("forgejo tracker: read response body: %w", readErr)
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return respBody, nextPath, nil
	}
	return respBody, nextPath, classifyError(resp, respBody)
}

func classifyError(resp *http.Response, body []byte) error {
	msg := httpkit.Message(body)
	switch resp.StatusCode {
	case http.StatusNotFound:
		return fmt.Errorf("%w: %s", ErrNotFound, msg)
	case http.StatusTooManyRequests:
		return httpkit.BuildRateLimitError(resp, msg, ErrRateLimited)
	case http.StatusUnauthorized, http.StatusForbidden:
		return fmt.Errorf("%w: %s", ErrAuthFailed, msg)
	}
	return fmt.Errorf("forgejo tracker: %d %s", resp.StatusCode, msg)
}

// ---------------------------------------------------------------------------
// ID parsing
// ---------------------------------------------------------------------------

func (t *Tracker) parseID(id domain.TrackerID) (repoPath string, number int, err error) {
	if id.Provider != domain.TrackerProviderForgejo {
		return "", 0, fmt.Errorf("%w: provider=%q", ErrWrongProvider, id.Provider)
	}
	return parseForgejoID(id.Native)
}

// parseForgejoID accepts "owner/repo#number" and returns the repo path and
// issue number. The repo path may contain multiple segments for nested
// namespaces (e.g. "group/subgroup/repo").
func parseForgejoID(native string) (repoPath string, number int, err error) {
	hash := strings.IndexByte(native, '#')
	if hash < 0 {
		return "", 0, fmt.Errorf("%w: missing #number", ErrBadID)
	}
	repoPath = native[:hash]
	numStr := native[hash+1:]
	if err := validateRepoPath(repoPath); err != nil {
		return "", 0, err
	}
	n, parseErr := strconv.Atoi(numStr)
	if parseErr != nil || n <= 0 {
		return "", 0, fmt.Errorf("%w: bad number %q", ErrBadID, numStr)
	}
	return repoPath, n, nil
}

// parseForgejoRepo accepts "owner/repo" and rejects empty strings, paths
// without a slash, and paths containing whitespace or "#".
func parseForgejoRepo(native string) (string, error) {
	if native == "" {
		return "", fmt.Errorf("%w: empty repo", ErrBadID)
	}
	if err := validateRepoPath(native); err != nil {
		return "", err
	}
	return native, nil
}

// validateRepoPath checks that a repo path is non-empty, contains at least
// one "/" separator, and has no whitespace or "#" characters.
func validateRepoPath(path string) error {
	if path == "" {
		return fmt.Errorf("%w: empty repo path", ErrBadID)
	}
	if !strings.Contains(path, "/") {
		return fmt.Errorf("%w: missing repo path separator", ErrBadID)
	}
	if strings.ContainsAny(path, " \t\n\r#") {
		return fmt.Errorf("%w: invalid repo path %q", ErrBadID, path)
	}
	return nil
}
