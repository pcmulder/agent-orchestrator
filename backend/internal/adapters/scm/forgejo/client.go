package forgejo

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

var (
	// ErrNotFound is returned when a Forgejo API resource does not exist.
	ErrNotFound = ports.ErrSCMNotFound
	// ErrRateLimited is returned when Forgejo responds with HTTP 429.
	ErrRateLimited = fmt.Errorf("forgejo scm: rate limited")
)

// RateLimitError carries the structured backoff hints from a Forgejo 429
// response. Forgejo (like Gitea) sends a Retry-After header on 429 responses;
// the observer applies a provider-level cooldown so it does not keep polling
// every 30s while rate-limited. Callers that only need the category use
// errors.Is(err, ErrRateLimited); callers needing the exact backoff use
// errors.As.
type RateLimitError struct {
	RetryAfter time.Duration
	Message    string
}

// Error formats the rate-limit error for logs.
func (e *RateLimitError) Error() string {
	if e == nil {
		return ErrRateLimited.Error()
	}
	if e.Message != "" {
		return "forgejo scm: rate limited: " + e.Message
	}
	return ErrRateLimited.Error()
}

// Is lets errors.Is match a *RateLimitError against ErrRateLimited.
func (e *RateLimitError) Is(target error) bool { return target == ErrRateLimited }

// GetRetryAfter exposes the Retry-After hint for the provider-neutral
// observer's rateLimitCooldown helper.
func (e *RateLimitError) GetRetryAfter() time.Duration {
	if e == nil {
		return 0
	}
	return e.RetryAfter
}

// GetResetAt is required by the observer's rateLimitedError capability.
// Forgejo only sends Retry-After, so this is always zero.
func (e *RateLimitError) GetResetAt() time.Time { return time.Time{} }

const (
	defaultUserAgent   = "ao-forgejo-scm/1"
	defaultHTTPTimeout = 30 * time.Second
)

// RESTResponse is the normalised result of a Forgejo REST call.
type RESTResponse struct {
	StatusCode int
	Body       []byte
}

// ClientOptions configures the Forgejo HTTP client.
type ClientOptions struct {
	HTTPClient *http.Client
	Token      TokenSource
	// APIBase is the full REST base URL (e.g. "https://forgejo.example.com/api/v1"
	// or "http://127.0.0.1:3000/api/v1" for a local instance). The scheme is
	// caller-supplied so self-hosted instances reachable over plain HTTP work.
	APIBase   string
	UserAgent string
}

// Client wraps the Forgejo REST API v1. It handles auth and error
// classification. The Forgejo API has no ETag revalidation, so unlike the
// GitLab/GitHub clients this one does not cache responses conditionally.
type Client struct {
	http      *http.Client
	tokens    TokenSource
	apiBase   string
	userAgent string
}

// NewClient creates a new Forgejo REST client. apiBase must be a full base
// URL including the /api/v1 path.
func NewClient(opts ClientOptions) *Client {
	hc := opts.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: defaultHTTPTimeout}
	}
	ua := opts.UserAgent
	if ua == "" {
		ua = defaultUserAgent
	}
	return &Client{
		http:      hc,
		tokens:    opts.Token,
		apiBase:   strings.TrimRight(opts.APIBase, "/"),
		userAgent: ua,
	}
}

// apiBase returns the configured REST base URL.
func (c *Client) apiBaseURL() string { return c.apiBase }

// doREST performs a request of the given method with an optional JSON body,
// returning the response body. Error statuses are classified into the
// sentinel errors. (Query-parameter GETs go through doGETPaginated, which
// builds the URL itself.)
func (c *Client) doREST(ctx context.Context, method, path string, body any) (RESTResponse, error) {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return RESTResponse{}, fmt.Errorf("forgejo scm: encode %s body: %w", path, err)
		}
		rdr = bytes.NewReader(b)
	}

	u := c.apiBase + path
	req, err := http.NewRequestWithContext(ctx, method, u, rdr)
	if err != nil {
		return RESTResponse{}, fmt.Errorf("forgejo scm: build %s request: %w", path, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("User-Agent", c.userAgent)
	if err := c.authorize(ctx, req); err != nil {
		return RESTResponse{}, err
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return RESTResponse{}, fmt.Errorf("forgejo scm: %s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return RESTResponse{}, fmt.Errorf("forgejo scm: read %s response: %w", path, err)
	}
	if resp.StatusCode >= 400 {
		return RESTResponse{StatusCode: resp.StatusCode, Body: b}, classifyError(resp, b)
	}
	return RESTResponse{StatusCode: resp.StatusCode, Body: b}, nil
}

// doGETPaginated performs a GET and follows the Forgejo/Gitea Link header
// (rel="next") to fetch all pages, calling handler for each page's body. It
// caps at maxPaginationPages to prevent runaway pagination.
func (c *Client) doGETPaginated(ctx context.Context, path string, q url.Values, handler func(body []byte) error) (bool, error) {
	if q == nil {
		q = url.Values{}
	}
	nextURL := c.apiBase + path
	if len(q) > 0 {
		nextURL += "?" + q.Encode()
	}
	for page := 0; page < maxPaginationPages; page++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, nextURL, http.NoBody)
		if err != nil {
			return false, err
		}
		req.Header.Set("User-Agent", c.userAgent)
		if err := c.authorize(ctx, req); err != nil {
			return false, err
		}
		resp, err := c.http.Do(req)
		if err != nil {
			return false, err
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode >= 400 {
			return false, classifyError(resp, body)
		}
		if err := handler(body); err != nil {
			return false, err
		}
		next := parseNextLink(resp.Header.Get("Link"))
		if next == "" {
			return false, nil
		}
		// Validate the next-page URL against the configured API base before
		// following it, so a hostile or misconfigured server cannot redirect
		// the bearer token to a different host.
		if err := c.validatePaginationURL(next); err != nil {
			return false, err
		}
		nextURL = next
	}
	return true, nil
}

// parseNextLink extracts the next-page URL from a Link header like:
//
//	<https://host/api/v1/...?page=2>; rel="next", <...>; rel="first"
func parseNextLink(linkHeader string) string {
	if linkHeader == "" {
		return ""
	}
	for _, part := range strings.Split(linkHeader, ",") {
		part = strings.TrimSpace(part)
		if !strings.Contains(part, `rel="next"`) {
			continue
		}
		lt := strings.Index(part, "<")
		gt := strings.Index(part, ">")
		if lt < 0 || gt < 0 || gt <= lt {
			continue
		}
		return part[lt+1 : gt]
	}
	return ""
}

// ErrPaginationURLRejected is returned when a Link: rel="next" pagination URL
// fails validation against the configured API base. The URL is not followed,
// so the Forgejo token is never attached to a different host.
var ErrPaginationURLRejected = fmt.Errorf("forgejo scm: pagination URL rejected")

// validatePaginationURL validates a Link: rel="next" URL against the client's
// configured API base before the URL is followed. It requires the same
// scheme, host, and port as apiBase, and rejects HTTPS-to-HTTP downgrades.
func (c *Client) validatePaginationURL(next string) error {
	nextURL, err := url.Parse(next)
	if err != nil {
		return fmt.Errorf("forgejo scm: parse pagination URL: %w", ErrPaginationURLRejected)
	}
	if nextURL.Host == "" {
		// Relative next URLs resolve against the trusted API base.
		return nil
	}
	base, err := url.Parse(c.apiBase)
	if err != nil {
		return fmt.Errorf("forgejo scm: parse API base: %w", ErrPaginationURLRejected)
	}
	if base.Scheme == "https" && nextURL.Scheme == "http" {
		return fmt.Errorf("forgejo scm: pagination URL downgrades https to http: %w", ErrPaginationURLRejected)
	}
	if !strings.EqualFold(nextURL.Scheme, base.Scheme) {
		return fmt.Errorf("forgejo scm: pagination URL scheme %q != base %q: %w", nextURL.Scheme, base.Scheme, ErrPaginationURLRejected)
	}
	if !strings.EqualFold(nextURL.Hostname(), base.Hostname()) {
		return fmt.Errorf("forgejo scm: pagination URL host %q != base %q: %w", nextURL.Hostname(), base.Hostname(), ErrPaginationURLRejected)
	}
	if nextURL.Port() != base.Port() {
		return fmt.Errorf("forgejo scm: pagination URL port %q != base %q: %w", nextURL.Port(), base.Port(), ErrPaginationURLRejected)
	}
	if !strings.HasPrefix(nextURL.Path, base.Path) {
		return fmt.Errorf("forgejo scm: pagination URL path %q does not start with API base %q: %w", nextURL.Path, base.Path, ErrPaginationURLRejected)
	}
	return nil
}

const maxPaginationPages = 50

func (c *Client) authorize(ctx context.Context, req *http.Request) error {
	if c.tokens == nil {
		return nil
	}
	tok, err := c.tokens.Token(ctx)
	if err != nil {
		return err
	}
	// Forgejo accepts both the "token <t>" and "Bearer <t>" Authorization
	// forms for personal access tokens; Bearer is the more widely compatible
	// form.
	req.Header.Set("Authorization", "Bearer "+tok)
	return nil
}

func classifyError(resp *http.Response, body []byte) error {
	switch resp.StatusCode {
	case http.StatusNotFound:
		return ErrNotFound
	case http.StatusUnauthorized, http.StatusForbidden:
		return ErrAuthFailed
	case http.StatusTooManyRequests:
		return forgejoRateLimited(resp, body)
	default:
		msg := forgejoMessage(body)
		if msg == "" {
			msg = resp.Status
		}
		return fmt.Errorf("forgejo scm: %s", msg)
	}
}

// forgejoRateLimited builds a *RateLimitError from a 429 response, parsing the
// Retry-After header so the observer can apply a provider-level cooldown.
func forgejoRateLimited(resp *http.Response, body []byte) error {
	e := &RateLimitError{Message: forgejoMessage(body)}
	if ra := resp.Header.Get("Retry-After"); ra != "" {
		if sec, err := strconv.Atoi(ra); err == nil && sec >= 0 {
			e.RetryAfter = time.Duration(sec) * time.Second
		}
	}
	return e
}

// forgejoMessage extracts a short message from a Forgejo error body. Gitea
// v1 error responses are plain text, so return the trimmed body when it is
// short and not JSON; otherwise fall back to a JSON message/error field.
func forgejoMessage(body []byte) string {
	trimmed := strings.TrimSpace(string(body))
	if trimmed == "" {
		return ""
	}
	var v struct {
		Message string `json:"message"`
		Error   string `json:"error"`
	}
	if json.Unmarshal(body, &v) == nil {
		if v.Message != "" {
			return v.Message
		}
		return v.Error
	}
	// Cap the returned message so a large HTML error page is not surfaced.
	if len(trimmed) > 200 {
		return trimmed[:200]
	}
	return trimmed
}

// repoPath encodes owner/name for Forgejo API URL path segments. The owner may
// contain "/" for nested namespaces; those must remain literal so the API
// route /repos/{owner}/{repo}/... resolves correctly (only the final repo
// segment and path specials are escaped).
func repoPath(owner, name string, parts ...string) string {
	path := "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(name)
	for _, p := range parts {
		path += "/" + p
	}
	return path
}
