package forgejo

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// testServer returns an httptest server plus a provider whose default client
// is pointed at it. The httptest server is plain HTTP at 127.0.0.1:PORT, so
// the provider's allowlist includes that host and the test repos carry
// Scheme="http" — exercising the scheme-respecting API base (the core
// requirement for local/plain-HTTP Forgejo instances).
func testServer(t *testing.T, handler http.Handler) (*httptest.Server, *Provider, string, string) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	host := u.Host // e.g. "127.0.0.1:54321"
	c := NewClient(ClientOptions{
		Token:   StaticTokenSource("test-token"),
		APIBase: srv.URL + "/api/v1",
	})
	p, err := NewProvider(ProviderOptions{Client: c, AllowedHosts: []string{host}})
	if err != nil {
		t.Fatal(err)
	}
	return srv, p, host, "http"
}

func testRepo(host, scheme string) ports.SCMRepo {
	return ports.SCMRepo{Provider: "forgejo", Host: host, Scheme: scheme, Owner: "acme", Name: "repo", Repo: "acme/repo"}
}

func ctx() context.Context { return context.Background() }

// ---------------------------------------------------------------------------
// ParseRepository
// ---------------------------------------------------------------------------

func TestParseRepository(t *testing.T) {
	// ParseRepository needs no HTTP, so build a provider with the hosts the
	// table cases reference allowlisted (the httptest-host case uses a
	// dedicated server below).
	p, err := NewProvider(ProviderOptions{
		Token:              StaticTokenSource("tok"),
		SkipTokenPreflight: true,
		AllowedHosts:       []string{"forgejo.example.com", "forgejo.example.com:22", "127.0.0.1:3000"},
	})
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name   string
		remote string
		want   ports.SCMRepo
		ok     bool
	}{
		{
			name:   "https remote",
			remote: "https://forgejo.example.com/acme/repo.git",
			want:   ports.SCMRepo{Provider: "forgejo", Host: "forgejo.example.com", Scheme: "https", Owner: "acme", Name: "repo", Repo: "acme/repo"},
			ok:     true,
		},
		{
			name:   "http remote with port preserves scheme",
			remote: "http://127.0.0.1:3000/acme/repo.git",
			want:   ports.SCMRepo{Provider: "forgejo", Host: "127.0.0.1:3000", Scheme: "http", Owner: "acme", Name: "repo", Repo: "acme/repo"},
			ok:     true,
		},
		{
			name:   "nested namespace",
			remote: "https://forgejo.example.com/org/team/repo.git",
			want:   ports.SCMRepo{Provider: "forgejo", Host: "forgejo.example.com", Scheme: "https", Owner: "org/team", Name: "repo", Repo: "org/team/repo"},
			ok:     true,
		},
		{
			name:   "ssh scp-like remote defaults to https",
			remote: "git@forgejo.example.com:acme/repo.git",
			want:   ports.SCMRepo{Provider: "forgejo", Host: "forgejo.example.com", Scheme: "https", Owner: "acme", Name: "repo", Repo: "acme/repo"},
			ok:     true,
		},
		{
			name:   "ssh url remote defaults to https",
			remote: "ssh://git@forgejo.example.com:22/acme/repo.git",
			want:   ports.SCMRepo{Provider: "forgejo", Host: "forgejo.example.com:22", Scheme: "https", Owner: "acme", Name: "repo", Repo: "acme/repo"},
			ok:     true,
		},
		{
			name:   "non-allowlisted host rejected",
			remote: "https://forgejo.attacker.example/acme/repo.git",
			ok:     false,
		},
		{
			name:   "host with port not in allowlist rejected",
			remote: "http://127.0.0.1:3001/acme/repo.git",
			ok:     false,
		},
		{
			name:   "single segment path rejected",
			remote: "https://forgejo.example.com/repo.git",
			ok:     false,
		},
		{
			name:   "empty remote rejected",
			remote: "",
			ok:     false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Ensure the allowlisted hosts used in the happy cases are
			// actually allowed by the provider.
			got, ok := p.ParseRepository(tc.remote)
			if ok != tc.ok {
				t.Fatalf("ok = %v, want %v (repo=%+v)", ok, tc.ok, got)
			}
			if !ok {
				return
			}
			if got != tc.want {
				t.Fatalf("got = %+v, want %+v", got, tc.want)
			}
		})
	}
	// The httptest host is allowlisted in a dedicated server provider, so a
	// plain-HTTP remote to it must parse with Scheme="http".
	httpHost := "127.0.0.1:3000"
	httpProvider, err := NewProvider(ProviderOptions{
		Token:              StaticTokenSource("tok"),
		SkipTokenPreflight: true,
		AllowedHosts:       []string{httpHost},
	})
	if err != nil {
		t.Fatal(err)
	}
	got, ok := httpProvider.ParseRepository("http://" + httpHost + "/acme/repo.git")
	if !ok {
		t.Fatalf("httptest host not parsed")
	}
	if got.Scheme != "http" {
		t.Fatalf("scheme = %q, want http", got.Scheme)
	}
	if got.Host != httpHost {
		t.Fatalf("host = %q, want %q", got.Host, httpHost)
	}
}

func TestParseRepositoryAllowsOnlyListedHosts(t *testing.T) {
	p, err := NewProvider(ProviderOptions{
		Token:              StaticTokenSource("tok"),
		SkipTokenPreflight: true,
		AllowedHosts:       []string{"forgejo.a.example", "127.0.0.1:3000"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := p.ParseRepository("https://forgejo.a.example/acme/repo.git"); !ok {
		t.Fatal("allowed host rejected")
	}
	if _, ok := p.ParseRepository("http://127.0.0.1:3000/acme/repo.git"); !ok {
		t.Fatal("allowed host:port rejected")
	}
	if _, ok := p.ParseRepository("https://forgejo.b.example/acme/repo.git"); ok {
		t.Fatal("non-allowlisted host accepted")
	}
}

// ---------------------------------------------------------------------------
// Host allowlist + client selection
// ---------------------------------------------------------------------------

func TestClientForHostRejectsNonAllowlisted(t *testing.T) {
	p, err := NewProvider(ProviderOptions{
		Token:              StaticTokenSource("tok"),
		SkipTokenPreflight: true,
		AllowedHosts:       []string{"forgejo.example.com"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if p.clientForHost("evil.example", "https") != nil {
		t.Fatal("non-allowlisted host returned a client")
	}
	if _, err := p.clientForRepoErr(ports.SCMRepo{Host: "evil.example"}); !errors.Is(err, ErrHostNotAllowed) {
		t.Fatalf("err = %v, want ErrHostNotAllowed", err)
	}
}

func TestClientForHostUsesSchemeForAPIBase(t *testing.T) {
	p, err := NewProvider(ProviderOptions{
		Token:              StaticTokenSource("tok"),
		SkipTokenPreflight: true,
		AllowedHosts:       []string{"127.0.0.1:3000"},
	})
	if err != nil {
		t.Fatal(err)
	}
	// http scheme → http://.../api/v1 (the local-instance requirement).
	c := p.clientForHost("127.0.0.1:3000", "http")
	if c == nil {
		t.Fatal("client is nil for allowlisted host")
	}
	if got := c.apiBaseURL(); got != "http://127.0.0.1:3000/api/v1" {
		t.Fatalf("APIBase = %q, want http://127.0.0.1:3000/api/v1", got)
	}
	// https scheme on the same host → a distinct client with the https base.
	c2 := p.clientForHost("127.0.0.1:3000", "https")
	if c2 == nil || c2 == c {
		t.Fatalf("expected a distinct https client, got %v", c2)
	}
	if got := c2.apiBaseURL(); got != "https://127.0.0.1:3000/api/v1" {
		t.Fatalf("https APIBase = %q", got)
	}
	// Empty scheme defaults to https.
	c3 := p.clientForHost("127.0.0.1:3000", "")
	if got := c3.apiBaseURL(); got != "https://127.0.0.1:3000/api/v1" {
		t.Fatalf("default APIBase = %q, want https", got)
	}
}

func TestClientForHostHostTokenOverride(t *testing.T) {
	p, err := NewProvider(ProviderOptions{
		Token:              StaticTokenSource("default"),
		SkipTokenPreflight: true,
		AllowedHosts:       []string{"a.example", "b.example"},
		HostTokens:         map[string]TokenSource{"b.example": StaticTokenSource("b-token")},
	})
	if err != nil {
		t.Fatal(err)
	}
	ca := p.clientForHost("a.example", "https")
	cb := p.clientForHost("b.example", "https")
	tokA, _ := ca.tokens.Token(ctx())
	tokB, _ := cb.tokens.Token(ctx())
	if tokA != "default" {
		t.Fatalf("host a token = %q, want default", tokA)
	}
	if tokB != "b-token" {
		t.Fatalf("host b token = %q, want b-token", tokB)
	}
}

// ---------------------------------------------------------------------------
// RepoPRListGuard / CommitChecksGuard
// ---------------------------------------------------------------------------

func TestRepoPRListGuardFingerprint(t *testing.T) {
	var body []byte
	_, p, host, scheme := testServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/repos/acme/repo/pulls" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		_, _ = w.Write(body)
	}))
	repo := testRepo(host, scheme)
	body = []byte(`[{"id":1,"number":7,"state":"open","draft":false,"html_url":"u","user":{"login":"alice"},"head":{"ref":"b"},"base":{"ref":"main"}}]`)
	res1, err := p.RepoPRListGuard(ctx(), repo, "")
	if err != nil {
		t.Fatal(err)
	}
	if res1.ETag == "" {
		t.Fatal("guard returned no fingerprint")
	}
	if res1.NotModified {
		t.Fatal("first guard (empty prev etag) must not report NotModified")
	}
	// Unchanged listing -> same fingerprint -> NotModified.
	res2, _ := p.RepoPRListGuard(ctx(), repo, res1.ETag)
	if !res2.NotModified {
		t.Fatal("unchanged listing should report NotModified")
	}
	// Changed listing -> new fingerprint -> change.
	body = []byte(`[]`)
	res3, _ := p.RepoPRListGuard(ctx(), repo, res1.ETag)
	if res3.NotModified || res3.ETag == res1.ETag {
		t.Fatalf("changed listing should flip the guard (etag=%q notmod=%v)", res3.ETag, res3.NotModified)
	}
}

func TestCommitChecksGuardFingerprintChangesWithStatuses(t *testing.T) {
	var body []byte
	_, p, host, scheme := testServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/statuses") {
			t.Fatalf("path = %s", r.URL.Path)
		}
		_, _ = w.Write(body)
	}))
	repo := testRepo(host, scheme)
	body = []byte(`[{"id":1,"status":"pending","context":"ci"}]`)
	res1, err := p.CommitChecksGuard(ctx(), repo, "abc123", "")
	if err != nil {
		t.Fatal(err)
	}
	if res1.ETag == "" {
		t.Fatal("first guard returned no fingerprint")
	}
	// Unchanged body → same fingerprint → NotModified.
	res2, _ := p.CommitChecksGuard(ctx(), repo, "abc123", res1.ETag)
	if !res2.NotModified {
		t.Fatal("unchanged statuses should report NotModified")
	}
	// Changed body → new fingerprint → change.
	body = []byte(`[{"id":1,"status":"success","context":"ci"}]`)
	res3, _ := p.CommitChecksGuard(ctx(), repo, "abc123", res1.ETag)
	if res3.NotModified || res3.ETag == res1.ETag {
		t.Fatalf("changed statuses should flip the guard (etag=%q notmod=%v)", res3.ETag, res3.NotModified)
	}
}

func TestCommitChecksGuardRejectsEmptySHA(t *testing.T) {
	_, p, host, scheme := testServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("no request expected for empty sha")
	}))
	_, err := p.CommitChecksGuard(ctx(), testRepo(host, scheme), "", "")
	if !errors.Is(err, ports.ErrSCMNotFound) {
		t.Fatalf("err = %v, want ErrSCMNotFound", err)
	}
}

// ---------------------------------------------------------------------------
// ListPRsByRepo
// ---------------------------------------------------------------------------

func TestListPRsByRepo(t *testing.T) {
	_, p, host, scheme := testServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/repos/acme/repo/pulls" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{
				"id": 101, "number": 7, "title": "Add X", "state": "open", "draft": false,
				"html_url": "http://" + r.Host + "/acme/repo/pulls/7", "mergeable": true, "merged": false,
				"head": map[string]any{"ref": "feat/x", "sha": "h1", "repo": map[string]any{"full_name": "acme/repo"}},
				"base": map[string]any{"ref": "main", "sha": "b1", "repo": map[string]any{"full_name": "acme/repo"}},
				"user": map[string]any{"login": "alice"}, "merge_base": "base1",
			},
		})
	}))
	prs, err := p.ListPRsByRepo(ctx(), testRepo(host, scheme), time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(prs) != 1 {
		t.Fatalf("len = %d, want 1", len(prs))
	}
	pr := prs[0]
	if pr.Number != 7 || pr.State != string(domain.PRStateOpen) || pr.SourceBranch != "feat/x" ||
		pr.TargetBranch != "main" || pr.HeadSHA != "h1" || pr.Author != "alice" || pr.HeadRepo != "acme/repo" {
		t.Fatalf("pr = %+v", pr)
	}
	if pr.ProviderID != "101" {
		t.Fatalf("providerID = %q, want 101", pr.ProviderID)
	}
}

// TestListPRsByRepoSurfacesMergedPR verifies that a PR that has been merged
// externally (state=closed, merged=true, merge_commit_sha set) is surfaced by
// ListPRsByRepo as merged. Forgejo lists with state=all (matching GitLab) so
// terminal PRs are observed by the normal refresh path instead of being
// dropped (BUG B).
func TestListPRsByRepoSurfacesMergedPR(t *testing.T) {
	_, p, host, scheme := testServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/repos/acme/repo/pulls" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		// The listing must request all states so merged PRs are included.
		if r.URL.Query().Get("state") != "all" {
			t.Errorf("list state = %q, want all", r.URL.Query().Get("state"))
		}
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{
				"id": 103, "number": 9, "title": "Merged PR", "state": "closed", "draft": false,
				"html_url": "http://" + r.Host + "/acme/repo/pulls/9", "mergeable": false, "merged": true,
				"merge_commit_sha": "msha",
				"head":             map[string]any{"ref": "feat/m", "sha": "h3", "repo": map[string]any{"full_name": "acme/repo"}},
				"base":             map[string]any{"ref": "main", "sha": "b2", "repo": map[string]any{"full_name": "acme/repo"}},
				"user":             map[string]any{"login": "alice"},
				"merged_at":        "2026-08-30T07:01:20Z",
			},
		})
	}))
	prs, err := p.ListPRsByRepo(ctx(), testRepo(host, scheme), time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(prs) != 1 {
		t.Fatalf("len = %d, want 1 (merged PR must be listed under state=all)", len(prs))
	}
	pr := prs[0]
	if !pr.Merged || pr.State != string(domain.PRStateMerged) {
		t.Fatalf("merged PR not surfaced as merged: %+v", pr)
	}
	if pr.MergeCommitSHA != "msha" {
		t.Fatalf("MergeCommitSHA = %q, want msha", pr.MergeCommitSHA)
	}
}

func TestListPRsByRepoDraftAndForkHeadRepo(t *testing.T) {
	_, p, host, scheme := testServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{
				"id": 102, "number": 8, "title": "Draft PR", "state": "open", "draft": true,
				"html_url": "http://" + r.Host + "/acme/repo/pulls/8", "mergeable": false, "merged": false,
				"head": map[string]any{"ref": "feat/d", "sha": "h2", "repo": map[string]any{"full_name": "forkuser/repo"}},
				"base": map[string]any{"ref": "main", "repo": map[string]any{"full_name": "acme/repo"}},
				"user": map[string]any{"login": "bob"},
			},
		})
	}))
	prs, err := p.ListPRsByRepo(ctx(), testRepo(host, scheme), time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	pr := prs[0]
	if pr.State != string(domain.PRStateDraft) || !pr.Draft {
		t.Fatalf("draft state not mapped: %+v", pr)
	}
	if pr.HeadRepo != "forkuser/repo" {
		t.Fatalf("fork HeadRepo = %q, want forkuser/repo", pr.HeadRepo)
	}
}

// ---------------------------------------------------------------------------
// FetchPullRequests
// ---------------------------------------------------------------------------

func TestFetchPullRequestsFull(t *testing.T) {
	repo := testRepo("placeholder", "https")
	srv, p, host, scheme := testServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/repos/acme/repo/pulls/7":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": 101, "number": 7, "title": "Add X", "state": "open", "draft": false,
				"html_url": "http://" + r.Host + "/acme/repo/pulls/7", "mergeable": true, "merged": false,
				"head": map[string]any{"ref": "feat/x", "sha": "head1", "repo": map[string]any{"full_name": "acme/repo"}},
				"base": map[string]any{"ref": "main", "sha": "base1"},
				"user": map[string]any{"login": "alice"},
			})
		case "/api/v1/repos/acme/repo/commits/head1/statuses":
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"id": 5, "status": "success", "context": "build", "target_url": "http://ci/build"},
				{"id": 6, "status": "success", "context": "test"},
			})
		case "/api/v1/repos/acme/repo/pulls/7/reviews":
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"id": 20, "state": "APPROVED", "user": map[string]any{"login": "reviewer"}, "commit_id": "head1", "submitted_at": "2026-01-01T00:00:00Z"},
			})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	_ = srv
	r := testRepo(host, scheme)
	obs, err := p.FetchPullRequests(ctx(), []ports.SCMPRRef{{Repo: r, Number: 7, URL: "http://" + host + "/acme/repo/pulls/7"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(obs) != 1 || !obs[0].Fetched {
		t.Fatalf("obs = %+v", obs)
	}
	o := obs[0]
	if o.Provider != "forgejo" || o.Host != host {
		t.Fatalf("provider/host = %s/%s", o.Provider, o.Host)
	}
	if o.PR.Number != 7 || o.PR.State != string(domain.PRStateOpen) || o.PR.HeadSHA != "head1" || o.PR.Author != "alice" {
		t.Fatalf("PR = %+v", o.PR)
	}
	if o.PR.BaseSHA != "base1" {
		t.Fatalf("BaseSHA = %q, want base1", o.PR.BaseSHA)
	}
	// CI: two successes → passing, two checks.
	if o.CI.Summary != string(domain.CIPassing) || len(o.CI.Checks) != 2 {
		t.Fatalf("CI = %+v", o.CI)
	}
	// Review: one approval → approved.
	if o.Review.Decision != string(domain.ReviewApproved) || len(o.Review.Reviews) != 1 {
		t.Fatalf("Review = %+v", o.Review)
	}
	// Mergeability: mergeable + passing + approved → mergeable.
	if o.Mergeability.State != string(domain.MergeMergeable) || !o.Mergeability.Mergeable {
		t.Fatalf("Mergeability = %+v", o.Mergeability)
	}
	_ = repo
}

func TestFetchPullRequestsMergedCarriesMergeCommitSHA(t *testing.T) {
	_, p, host, scheme := testServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/repos/acme/repo/pulls/11":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": 111, "number": 11, "state": "closed", "draft": false,
				"html_url": "http://" + r.Host + "/acme/repo/pulls/11",
				"merged":   true, "merge_commit_sha": "5de6ef5e0123456789abcdef0123456789abcdef",
				"head": map[string]any{"ref": "feat/z", "sha": "head9", "repo": map[string]any{"full_name": "acme/repo"}},
				"base": map[string]any{"ref": "main"}, "user": map[string]any{"login": "dave"},
			})
		case "/api/v1/repos/acme/repo/commits/head9/statuses":
			_ = json.NewEncoder(w).Encode([]map[string]any{})
		case "/api/v1/repos/acme/repo/pulls/11/reviews":
			_ = json.NewEncoder(w).Encode([]map[string]any{})
		}
	}))
	r := testRepo(host, scheme)
	obs, err := p.FetchPullRequests(ctx(), []ports.SCMPRRef{{Repo: r, Number: 11}})
	if err != nil {
		t.Fatal(err)
	}
	o := obs[0]
	if !o.PR.Merged || o.PR.State != string(domain.PRStateMerged) {
		t.Fatalf("merged state not mapped: %+v", o.PR)
	}
	if o.PR.MergeCommitSHA != "5de6ef5e0123456789abcdef0123456789abcdef" {
		t.Fatalf("MergeCommitSHA = %q, want the payload value", o.PR.MergeCommitSHA)
	}
}

func TestFetchPullRequestsFailingCIAndChangesRequested(t *testing.T) {
	_, p, host, scheme := testServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/repos/acme/repo/pulls/9":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": 109, "number": 9, "state": "open", "draft": false, "mergeable": true, "merged": false,
				"head": map[string]any{"ref": "feat/y", "sha": "head2", "repo": map[string]any{"full_name": "acme/repo"}},
				"base": map[string]any{"ref": "main"}, "user": map[string]any{"login": "carol"},
			})
		case "/api/v1/repos/acme/repo/commits/head2/statuses":
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"id": 7, "status": "failure", "context": "test", "description": "2 tests failed"},
			})
		case "/api/v1/repos/acme/repo/pulls/9/reviews":
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"id": 21, "state": "REQUEST_CHANGES", "user": map[string]any{"login": "reviewer"}, "commit_id": "head2"},
			})
		}
	}))
	r := testRepo(host, scheme)
	obs, err := p.FetchPullRequests(ctx(), []ports.SCMPRRef{{Repo: r, Number: 9}})
	if err != nil {
		t.Fatal(err)
	}
	o := obs[0]
	if o.CI.Summary != string(domain.CIFailing) || len(o.CI.FailedChecks) != 1 {
		t.Fatalf("CI = %+v", o.CI)
	}
	if o.Review.Decision != string(domain.ReviewChangesRequest) {
		t.Fatalf("Review = %+v", o.Review)
	}
	if o.Mergeability.State != string(domain.MergeBlocked) {
		t.Fatalf("Mergeability = %+v", o.Mergeability)
	}
	found := false
	for _, b := range o.Mergeability.Blockers {
		if b == "ci_failing" || b == "changes_requested" {
			found = true
		}
	}
	if !found {
		t.Fatalf("blockers = %v", o.Mergeability.Blockers)
	}
}

func TestFetchPullRequestsTransientsLeaveFetchedFalsePlaceholder(t *testing.T) {
	_, p, host, scheme := testServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("boom"))
	}))
	r := testRepo(host, scheme)
	obs, err := p.FetchPullRequests(ctx(), []ports.SCMPRRef{{Repo: r, Number: 4}})
	if err == nil {
		t.Fatal("expected an error on a 5xx detail fetch")
	}
	if len(obs) != 1 || obs[0].Fetched {
		t.Fatalf("placeholder must be Fetched=false: %+v", obs)
	}
}

func TestFetchPullRequestsBatchLimit(t *testing.T) {
	_, p, host, scheme := testServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	r := testRepo(host, scheme)
	refs := make([]ports.SCMPRRef, 26)
	for i := range refs {
		refs[i] = ports.SCMPRRef{Repo: r, Number: i + 1}
	}
	if _, err := p.FetchPullRequests(ctx(), refs); err == nil {
		t.Fatal("expected batch-size error for 26 refs")
	}
}

// ---------------------------------------------------------------------------
// FetchReviewThreads
// ---------------------------------------------------------------------------

func TestFetchReviewThreads(t *testing.T) {
	_, p, host, scheme := testServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/repos/acme/repo/pulls/7/files":
			_ = json.NewEncoder(w).Encode([]map[string]any{{"filename": "main.go", "status": "modified"}})
		case "/api/v1/repos/acme/repo/issues/7/timeline":
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"id": 30, "type": "code", "body": "nit: rename", "html_url": "http://" + r.Host + "/acme/repo/pulls/7#diff-main.go", "user": map[string]any{"login": "reviewer"}},
				{"id": 31, "type": "comment", "body": "a general comment", "user": map[string]any{"login": "bob"}},
			})
		case "/api/v1/repos/acme/repo/pulls/7/reviews":
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"id": 20, "state": "APPROVED", "user": map[string]any{"login": "reviewer"}, "commit_id": "head1"},
			})
		}
	}))
	ref := ports.SCMPRRef{Repo: testRepo(host, scheme), Number: 7, URL: "http://" + host + "/acme/repo/pulls/7"}
	res, err := p.FetchReviewThreads(ctx(), ref)
	if err != nil {
		t.Fatal(err)
	}
	if res.Decision != string(domain.ReviewApproved) {
		t.Fatalf("decision = %q", res.Decision)
	}
	// One file with a code comment → one thread; the general "comment" type is
	// not a review thread.
	if len(res.Threads) != 1 {
		t.Fatalf("threads = %+v", res.Threads)
	}
	th := res.Threads[0]
	if th.Path != "main.go" || len(th.Comments) != 1 || th.Comments[0].Author != "reviewer" {
		t.Fatalf("thread = %+v", th)
	}
}

// ---------------------------------------------------------------------------
// Identity / credentials
// ---------------------------------------------------------------------------

// TestAuthenticatedIdentityForHost verifies that AuthenticatedIdentityForHost
// resolves the identity for an allowlisted host. The test server is plain
// HTTP at 127.0.0.1, and the probe only succeeds when the client's API base
// uses the remote's http scheme — with the https default the request would
// fail with "server gave HTTP response to HTTPS client" (BUG A).
func TestAuthenticatedIdentityForHost(t *testing.T) {
	_, p, host, scheme := testServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/user" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"login": "octo", "is_admin": false})
	}))
	ident, err := p.AuthenticatedIdentityForHost(ctx(), host, scheme)
	if err != nil {
		t.Fatalf("identity over plain HTTP (scheme=%q) = %v; the probe must stay http", scheme, err)
	}
	if ident.Login != "octo" || !ident.Human {
		t.Fatalf("identity = %+v", ident)
	}
	// Second call is cached (no extra assertion; just ensure no error).
	if _, err := p.AuthenticatedIdentityForHost(ctx(), host, scheme); err != nil {
		t.Fatal(err)
	}
}

// TestAuthenticatedIdentityForHost_PlainHTTPRemoteStaysHTTP proves the scheme
// is actually plumbed into client selection (not just accepted): a provider
// with no pre-matched default client must lazily create an http:// client for
// a plain-HTTP remote and fail on an https:// client — the exact BUG A
// symptom observed against a live plain-HTTP Forgejo.
func TestAuthenticatedIdentityForHost_PlainHTTPRemoteStaysHTTP(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"login": "ao-admin", "is_admin": true})
	}))
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	host := u.Host
	// No default client: per-host clients are derived purely from host+scheme.
	p, err := NewProvider(ProviderOptions{
		Token:              StaticTokenSource("test-token"),
		SkipTokenPreflight: true,
		AllowedHosts:       []string{host},
	})
	if err != nil {
		t.Fatal(err)
	}

	// http scheme (the plain-HTTP remote): the probe reaches the server and
	// resolves the login.
	ident, err := p.AuthenticatedIdentityForHost(ctx(), host, "http")
	if err != nil {
		t.Fatalf("http-scheme identity = %v; the plain-HTTP remote must be probed over http", err)
	}
	if ident.Login != "ao-admin" {
		t.Fatalf("login = %q, want ao-admin", ident.Login)
	}

	// https scheme for the same host is a DISTINCT cache entry and must FAIL
	// against the plain-HTTP server with the BUG A signature — proving the
	// scheme (not just the host) drives client selection and caching.
	if _, err := p.AuthenticatedIdentityForHost(ctx(), host, "https"); err == nil {
		t.Fatal("https-scheme identity succeeded against a plain-HTTP server; scheme must select the API base")
	} else if !strings.Contains(err.Error(), "server gave HTTP response to HTTPS client") {
		t.Fatalf("https-scheme identity err = %v, want the plain-HTTP/https mismatch signature", err)
	}
}

// TestAuthenticatedIdentityForHost_SchemeSelectsClient verifies that the
// identity probe's client selection honors the remote's scheme (http stays
// http, https stays https) instead of always defaulting to https — the
// scheme is part of both the client key and the identity cache key, so a
// plain-HTTP instance is never probed over https (BUG A).
func TestAuthenticatedIdentityForHost_SchemeSelectsClient(t *testing.T) {
	_, p, host, _ := testServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"login": "octo"})
	}))
	httpClient := p.clientForHost(host, "http")
	if httpClient == nil {
		t.Fatal("http-scheme host not in allowlist")
	}
	if !strings.HasPrefix(httpClient.apiBaseURL(), "http://") {
		t.Fatalf("http-scheme API base = %q, want http:// prefix (plain-HTTP instance must stay http)", httpClient.apiBaseURL())
	}
	httpsClient := p.clientForHost(host, "https")
	if httpsClient == nil {
		t.Fatal("https-scheme host not in allowlist")
	}
	if !strings.HasPrefix(httpsClient.apiBaseURL(), "https://") {
		t.Fatalf("https-scheme API base = %q, want https:// prefix", httpsClient.apiBaseURL())
	}
	// The two clients must be distinct: the scheme is part of the client key,
	// so the identity cache (keyed by host+scheme) cannot conflate the two.
	if httpClient == httpsClient {
		t.Fatal("http and https clients for the same host must be distinct")
	}
}

func TestSCMCredentialsAvailable(t *testing.T) {
	// Default token present → available.
	p, err := NewProvider(ProviderOptions{Token: StaticTokenSource("tok"), SkipTokenPreflight: true, AllowedHosts: []string{"a.example"}})
	if err != nil {
		t.Fatal(err)
	}
	ok, _ := p.SCMCredentialsAvailable(ctx())
	if !ok {
		t.Fatal("expected credentials available with a default token")
	}

	// No default token but a per-host token → still available.
	p2, err := NewProvider(ProviderOptions{SkipTokenPreflight: true, AllowedHosts: []string{"b.example"}, HostTokens: map[string]TokenSource{"b.example": StaticTokenSource("host-tok")}})
	if err != nil {
		t.Fatal(err)
	}
	ok2, _ := p2.SCMCredentialsAvailable(ctx())
	if !ok2 {
		t.Fatal("expected credentials available via a per-host token")
	}

	// No tokens at all → not available.
	p3, err := NewProvider(ProviderOptions{SkipTokenPreflight: true, AllowedHosts: []string{"c.example"}})
	if err != nil {
		t.Fatal(err)
	}
	ok3, _ := p3.SCMCredentialsAvailable(ctx())
	if ok3 {
		t.Fatal("expected no credentials when no token is configured")
	}
}

func TestRateLimitError(t *testing.T) {
	_, p, host, scheme := testServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte("rate limited"))
	}))
	r := testRepo(host, scheme)
	_, err := p.RepoPRListGuard(ctx(), r, "")
	if err == nil {
		t.Fatal("expected a rate-limit error")
	}
	var rl *RateLimitError
	if !errors.As(err, &rl) {
		t.Fatalf("err = %v, want *RateLimitError", err)
	}
	if !errors.Is(err, ErrRateLimited) {
		t.Fatal("should match ErrRateLimited")
	}
	if rl.GetRetryAfter() != 30*time.Second {
		t.Fatalf("RetryAfter = %v, want 30s", rl.GetRetryAfter())
	}
}
