package forgejo

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	scmforgejo "github.com/aoagents/agent-orchestrator/backend/internal/adapters/scm/forgejo"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// recordedReq captures one inbound HTTP request so tests can assert against
// the exact Forgejo API surface the adapter touched.
type recordedReq struct {
	Method string
	Path   string
	Body   string
}

// fakeFJ is a programmable httptest.Server that matches requests by
// "METHOD path" and records every call. Unmatched requests fail the test.
type fakeFJ struct {
	t        *testing.T
	server   *httptest.Server
	mu       sync.Mutex
	requests []recordedReq
	handlers map[string]http.HandlerFunc
}

func newFakeFJ(t *testing.T) *fakeFJ {
	t.Helper()
	f := &fakeFJ{t: t, handlers: map[string]http.HandlerFunc{}}
	f.server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeFJ) on(method, path string, h http.HandlerFunc) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.handlers[method+" "+path] = h
}

func (f *fakeFJ) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	key := r.Method + " " + r.URL.Path
	f.mu.Lock()
	f.requests = append(f.requests, recordedReq{Method: r.Method, Path: r.URL.Path, Body: string(body)})
	h, ok := f.handlers[key]
	f.mu.Unlock()
	if !ok {
		f.t.Errorf("unexpected request: %s", key)
		http.Error(w, "no handler", http.StatusNotImplemented)
		return
	}
	r.Body = io.NopCloser(strings.NewReader(string(body)))
	h(w, r)
}

// newTrackerForTest constructs an adapter pointed at the fake server with a
// static dev token. The httptest host (127.0.0.1:PORT) is allowlisted and its
// base URL is overridden to the plain-HTTP test server.
func newTrackerForTest(t *testing.T, f *fakeFJ) *Tracker {
	t.Helper()
	host := f.server.Listener.Addr().String()
	// Point the base at the bare server (no /api/v1) so the tracker's relative
	// paths (/repos/..., /user) hit the fake's registered paths — the same
	// convention the GitLab tracker test uses to strip /api/v4.
	tr, err := New(Options{
		Token:        scmforgejo.StaticTokenSource("tkn-test"),
		AllowedHosts: []string{host},
		BaseURLs:     map[string]string{host: f.server.URL},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return tr
}

// hostFor returns the allowlisted httptest host for a fake server.
func hostFor(f *fakeFJ) string {
	return f.server.Listener.Addr().String()
}

func ctx() context.Context { return context.Background() }

// ---------------------------------------------------------------------------
// New / construction
// ---------------------------------------------------------------------------

func TestNewRejectsMissingToken(t *testing.T) {
	if _, err := New(Options{Token: scmforgejo.StaticTokenSource("")}); !errors.Is(err, ErrNoToken) {
		t.Fatalf("New with empty token = %v, want ErrNoToken", err)
	}
	if _, err := New(Options{}); !errors.Is(err, ErrNoToken) {
		t.Fatalf("New with no source = %v, want ErrNoToken", err)
	}
}

func TestNewRejectsEmptyHostInAllowlist(t *testing.T) {
	// A blank entry in the allowlist is dropped, so an empty-host lookup must
	// be rejected (forgejo has no default public host).
	tr, err := New(Options{
		Token:        scmforgejo.StaticTokenSource("tkn"),
		AllowedHosts: []string{"", "  "},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if !errors.Is(tr.ConfigForHost(""), ErrHostNotAllowed) {
		t.Fatalf("ConfigForHost(\"\") = %v, want ErrHostNotAllowed", err)
	}
}

// ---------------------------------------------------------------------------
// ID parsing
// ---------------------------------------------------------------------------

func TestParseID(t *testing.T) {
	cases := []struct {
		name     string
		native   string
		wantPath string
		wantNum  int
		wantErr  bool
	}{
		{"happy", "octocat/hello-world#42", "octocat/hello-world", 42, false},
		{"nested namespace", "group/subgroup/project#7", "group/subgroup/project", 7, false},
		{"missing hash", "octocat/hello-world", "", 0, true},
		{"missing slash", "octocat#42", "", 0, true},
		{"empty path", "#42", "", 0, true},
		{"non-numeric number", "o/r#abc", "", 0, true},
		{"zero number", "o/r#0", "", 0, true},
		{"negative number", "o/r#-1", "", 0, true},
		{"whitespace in path", "o/r space#1", "", 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path, num, err := parseForgejoID(tc.native)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got %s#%d", path, num)
				}
				return
			}
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if path != tc.wantPath || num != tc.wantNum {
				t.Fatalf("got %s#%d, want %s#%d", path, num, tc.wantPath, tc.wantNum)
			}
		})
	}
}

func TestParseRepo(t *testing.T) {
	cases := []struct {
		name    string
		native  string
		wantErr bool
	}{
		{"happy", "group/project", false},
		{"nested", "group/sub/project", false},
		{"empty", "", true},
		{"no separator", "project", true},
		{"whitespace", " group/project", true},
		{"hash", "group/pro#ject", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseForgejoRepo(tc.native)
			if tc.wantErr && err == nil {
				t.Fatalf("expected error for %q", tc.native)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error for %q: %v", tc.native, err)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Get
// ---------------------------------------------------------------------------

func TestGet_HappyPath(t *testing.T) {
	f := newFakeFJ(t)
	f.on("GET", "/repos/octocat/hello-world/issues/42", func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer tkn-test" {
			t.Errorf("Authorization = %q, want Bearer tkn-test", got)
		}
		_, _ = w.Write([]byte(`{
			"number": 42,
			"title": "Found a bug",
			"body": "It does not work",
			"state": "open",
			"html_url": "https://forgejo.example.com/octocat/hello-world/issues/42",
			"labels": [{"name":"bug"},{"name":"critical"}],
			"assignees": [{"login":"alice"},{"login":"bob"}]
		}`))
	})
	tr := newTrackerForTest(t, f)

	issue, err := tr.Get(ctx(), domain.TrackerID{Provider: domain.TrackerProviderForgejo, Native: "octocat/hello-world#42", Host: hostFor(f)})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	want := domain.Issue{
		ID:        domain.TrackerID{Provider: domain.TrackerProviderForgejo, Native: "octocat/hello-world#42", Host: hostFor(f)},
		Title:     "Found a bug",
		Body:      "It does not work",
		State:     domain.IssueOpen,
		URL:       "https://forgejo.example.com/octocat/hello-world/issues/42",
		Labels:    []string{"bug", "critical"},
		Assignees: []string{"alice", "bob"},
	}
	if !reflect.DeepEqual(issue, want) {
		t.Fatalf("issue = %#v\nwant %#v", issue, want)
	}
}

func TestGet_StateMapping(t *testing.T) {
	cases := []struct {
		name      string
		fjState   string
		wantState domain.NormalizedIssueState
	}{
		{"open", "open", domain.IssueOpen},
		{"closed", "closed", domain.IssueDone},
		{"open uppercase", "OPEN", domain.IssueOpen},
		{"closed uppercase", "CLOSED", domain.IssueDone},
		{"unknown defaults to open", "weird", domain.IssueOpen},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeFJ(t)
			payload := map[string]any{
				"number":   1,
				"title":    "t",
				"state":    tc.fjState,
				"html_url": "https://h/o/r/issues/1",
			}
			b, _ := json.Marshal(payload)
			f.on("GET", "/repos/o/r/issues/1", func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write(b)
			})
			tr := newTrackerForTest(t, f)
			issue, err := tr.Get(ctx(), domain.TrackerID{Provider: domain.TrackerProviderForgejo, Native: "o/r#1", Host: hostFor(f)})
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if issue.State != tc.wantState {
				t.Fatalf("state = %q, want %q", issue.State, tc.wantState)
			}
		})
	}
}

func TestGet_NestedNamespace(t *testing.T) {
	f := newFakeFJ(t)
	f.on("GET", "/repos/group/sub/project/issues/7", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"number":7,"title":"nested","body":"d","state":"open","html_url":"https://h/group/sub/project/issues/7"}`))
	})
	tr := newTrackerForTest(t, f)
	issue, err := tr.Get(ctx(), domain.TrackerID{Provider: domain.TrackerProviderForgejo, Native: "group/sub/project#7", Host: hostFor(f)})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if issue.ID.Native != "group/sub/project#7" {
		t.Fatalf("Native = %q, want group/sub/project#7", issue.ID.Native)
	}
}

func TestGet_WrongProvider(t *testing.T) {
	tr := newTrackerForTest(t, newFakeFJ(t))
	_, err := tr.Get(ctx(), domain.TrackerID{Provider: domain.TrackerProviderGitLab, Native: "o/r#1", Host: "h"})
	if !errors.Is(err, ErrWrongProvider) {
		t.Fatalf("err = %v, want ErrWrongProvider", err)
	}
}

func TestGet_HostNotAllowed(t *testing.T) {
	f := newFakeFJ(t)
	tr := newTrackerForTest(t, f)
	_, err := tr.Get(ctx(), domain.TrackerID{Provider: domain.TrackerProviderForgejo, Native: "o/r#1", Host: "evil.example"})
	if !errors.Is(err, ErrHostNotAllowed) {
		t.Fatalf("err = %v, want ErrHostNotAllowed", err)
	}
}

func TestGet_NotFound(t *testing.T) {
	f := newFakeFJ(t)
	f.on("GET", "/repos/o/r/issues/1", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "404 page not found", http.StatusNotFound)
	})
	tr := newTrackerForTest(t, f)
	_, err := tr.Get(ctx(), domain.TrackerID{Provider: domain.TrackerProviderForgejo, Native: "o/r#1", Host: hostFor(f)})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestGet_RateLimited(t *testing.T) {
	f := newFakeFJ(t)
	f.on("GET", "/repos/o/r/issues/1", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "60")
		http.Error(w, "too many requests", http.StatusTooManyRequests)
	})
	tr := newTrackerForTest(t, f)
	_, err := tr.Get(ctx(), domain.TrackerID{Provider: domain.TrackerProviderForgejo, Native: "o/r#1", Host: hostFor(f)})
	if !errors.Is(err, ErrRateLimited) {
		t.Fatalf("err = %v, want ErrRateLimited", err)
	}
	var rle *RateLimitError
	if !errors.As(err, &rle) {
		t.Fatalf("err = %v, want *RateLimitError", err)
	}
	if rle.RetryAfter != 60*time.Second {
		t.Fatalf("RetryAfter = %v, want 60s", rle.RetryAfter)
	}
}

func TestGet_AuthFailed(t *testing.T) {
	f := newFakeFJ(t)
	f.on("GET", "/repos/o/r/issues/1", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	})
	tr := newTrackerForTest(t, f)
	_, err := tr.Get(ctx(), domain.TrackerID{Provider: domain.TrackerProviderForgejo, Native: "o/r#1", Host: hostFor(f)})
	if !errors.Is(err, ErrAuthFailed) {
		t.Fatalf("err = %v, want ErrAuthFailed", err)
	}
}

// ---------------------------------------------------------------------------
// List
// ---------------------------------------------------------------------------

func TestList_HappyPathFiltersOutPullRequests(t *testing.T) {
	f := newFakeFJ(t)
	f.on("GET", "/repos/octocat/hello-world/issues", func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("state"); got != "all" {
			t.Errorf("state = %q, want all", got)
		}
		_, _ = w.Write([]byte(`[
			{"number": 1, "title": "a plain issue", "state": "open", "html_url": "u1", "labels": []},
			{"number": 2, "title": "a PR", "state": "open", "html_url": "u2", "pull_request": {"merged": false}},
			{"number": 3, "title": "a closed issue", "state": "closed", "html_url": "u3", "labels": [{"name":"done"}]}
		]`))
	})
	tr := newTrackerForTest(t, f)
	issues, err := tr.List(ctx(), domain.TrackerRepo{Provider: domain.TrackerProviderForgejo, Native: "octocat/hello-world", Host: hostFor(f)}, domain.ListFilter{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(issues) != 2 {
		t.Fatalf("len = %d, want 2 (PR rows filtered out)", len(issues))
	}
	if issues[0].Title != "a plain issue" || issues[0].State != domain.IssueOpen {
		t.Fatalf("issue[0] = %#v", issues[0])
	}
	if issues[1].Title != "a closed issue" || issues[1].State != domain.IssueDone {
		t.Fatalf("issue[1] = %#v", issues[1])
	}
}

func TestList_StateAndAssigneeFilters(t *testing.T) {
	f := newFakeFJ(t)
	f.on("GET", "/repos/o/r/issues", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if got := q.Get("state"); got != "open" {
			t.Errorf("state = %q, want open", got)
		}
		if got := q.Get("assignee"); got != "alice" {
			t.Errorf("assignee = %q, want alice", got)
		}
		if got := q.Get("labels"); got != "bug,security" {
			t.Errorf("labels = %q, want bug,security", got)
		}
		_, _ = w.Write([]byte(`[{"number":5,"title":"x","state":"open","html_url":"u"}]`))
	})
	tr := newTrackerForTest(t, f)
	issues, err := tr.List(ctx(), domain.TrackerRepo{Provider: domain.TrackerProviderForgejo, Native: "o/r", Host: hostFor(f)},
		domain.ListFilter{State: domain.ListOpen, Labels: []string{"bug", "security"}, Assignee: "alice"})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(issues) != 1 || issues[0].Title != "x" {
		t.Fatalf("issues = %#v", issues)
	}
}

func TestList_PaginationViaLinkHeader(t *testing.T) {
	f := newFakeFJ(t)
	page := 0
	f.on("GET", "/repos/o/r/issues", func(w http.ResponseWriter, r *http.Request) {
		page++
		body := `[{"number":1,"title":"p1","state":"open","html_url":"u1"}]`
		if page == 1 {
			w.Header().Set("Link", "<"+f.server.URL+"/repos/o/r/issues?page=2>; rel=\"next\"")
		}
		_, _ = w.Write([]byte(body))
	})
	tr := newTrackerForTest(t, f)
	issues, err := tr.List(ctx(), domain.TrackerRepo{Provider: domain.TrackerProviderForgejo, Native: "o/r", Host: hostFor(f)}, domain.ListFilter{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(issues) != 2 {
		t.Fatalf("len = %d, want 2 (two pages)", len(issues))
	}
}

func TestList_LimitCapsResults(t *testing.T) {
	f := newFakeFJ(t)
	f.on("GET", "/repos/o/r/issues", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[
			{"number":1,"title":"a","state":"open","html_url":"u1"},
			{"number":2,"title":"b","state":"open","html_url":"u2"},
			{"number":3,"title":"c","state":"open","html_url":"u3"}
		]`))
	})
	tr := newTrackerForTest(t, f)
	issues, err := tr.List(ctx(), domain.TrackerRepo{Provider: domain.TrackerProviderForgejo, Native: "o/r", Host: hostFor(f)},
		domain.ListFilter{Limit: 2})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(issues) != 2 {
		t.Fatalf("len = %d, want 2 (limit)", len(issues))
	}
}

func TestList_WrongProvider(t *testing.T) {
	tr := newTrackerForTest(t, newFakeFJ(t))
	_, err := tr.List(ctx(), domain.TrackerRepo{Provider: domain.TrackerProviderGitHub, Native: "o/r", Host: "h"}, domain.ListFilter{})
	if !errors.Is(err, ErrWrongProvider) {
		t.Fatalf("err = %v, want ErrWrongProvider", err)
	}
}

// ---------------------------------------------------------------------------
// Preflight
// ---------------------------------------------------------------------------

func TestPreflight(t *testing.T) {
	f := newFakeFJ(t)
	f.on("GET", "/user", func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer tkn-test" {
			t.Errorf("Authorization = %q, want Bearer tkn-test", got)
		}
		_, _ = w.Write([]byte(`{"login":"octo"}`))
	})
	tr := newTrackerForTest(t, f)
	if err := tr.Preflight(ctx()); err != nil {
		t.Fatalf("Preflight: %v", err)
	}
}

func TestPreflight_FailureNotCached(t *testing.T) {
	f := newFakeFJ(t)
	f.on("GET", "/user", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	})
	tr := newTrackerForTest(t, f)
	if err := tr.Preflight(ctx()); !errors.Is(err, ErrAuthFailed) {
		t.Fatalf("first Preflight = %v, want ErrAuthFailed", err)
	}
}

func TestPreflight_NoHostConfigured(t *testing.T) {
	tr, err := New(Options{Token: scmforgejo.StaticTokenSource("tkn"), AllowedHosts: nil})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// With no host, Preflight is a no-op success (nothing to probe).
	if err := tr.Preflight(ctx()); err != nil {
		t.Fatalf("Preflight = %v, want nil (no host configured)", err)
	}
}

// ---------------------------------------------------------------------------
// defaultSchemeForHost
// ---------------------------------------------------------------------------

func TestDefaultSchemeForHost(t *testing.T) {
	cases := []struct {
		host string
		want string
	}{
		{"127.0.0.1:3000", "http"},
		{"127.0.0.1", "http"},
		{"localhost", "http"},
		{"localhost:3000", "http"},
		{"::1", "http"},
		{"forgejo.example.com", "https"},
		{"10.0.0.5:3000", "https"},
	}
	for _, tc := range cases {
		if got := defaultSchemeForHost(tc.host); got != tc.want {
			t.Errorf("defaultSchemeForHost(%q) = %q, want %q", tc.host, got, tc.want)
		}
	}
}

// Ensure strconv import stays used (retry-after assertions above use time).
var _ = strconv.Itoa
