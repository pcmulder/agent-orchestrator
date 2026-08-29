package forgejo

import (
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func validMergeRequest(host, scheme string) ports.SCMMergeRequest {
	return ports.SCMMergeRequest{
		PR: ports.SCMPRRef{
			Repo:   ports.SCMRepo{Provider: "forgejo", Host: host, Scheme: scheme, Owner: "acme", Name: "repo", Repo: "acme/repo"},
			Number: 42,
			URL:    "http://" + host + "/acme/repo/pulls/42",
		},
		ExpectedHeadSHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Method:          ports.SCMMergeSquash,
	}
}

func TestMergePullRequest_UsesSquashAndExpectedHead(t *testing.T) {
	// The merge endpoint returns an empty body; the provider re-fetches the PR
	// to recover merge_commit_sha. A single handler serves both endpoints.
	_, p, host, scheme := testServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/repos/acme/repo/pulls/42/merge":
			if r.Method != http.MethodPost {
				t.Fatalf("merge method = %s, want POST", r.Method)
			}
			var body struct {
				Do           string `json:"do"`
				HeadCommitID string `json:"head_commit_id"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatalf("decode body: %v", err)
			}
			if body.Do != "squash" {
				t.Fatalf("do = %q, want squash", body.Do)
			}
			if body.HeadCommitID != "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" {
				t.Fatalf("head_commit_id = %q", body.HeadCommitID)
			}
			w.WriteHeader(http.StatusOK) // empty body on success
		case "/api/v1/repos/acme/repo/pulls/42":
			_ = json.NewEncoder(w).Encode(map[string]any{"merge_commit_sha": "merged123", "merged": true})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	got, err := p.MergePullRequest(ctx(), validMergeRequest(host, scheme))
	if err != nil {
		t.Fatal(err)
	}
	if got.MergeCommitSHA != "merged123" {
		t.Fatalf("MergeCommitSHA = %q, want merged123", got.MergeCommitSHA)
	}
}

func TestMergePullRequest_MapsForgejoFailures(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		want   error
	}{
		{name: "not found", status: http.StatusNotFound, want: ports.ErrSCMNotFound},
		{name: "head changed (409)", status: http.StatusConflict, want: ports.ErrSCMHeadChanged},
		{name: "not mergeable (405)", status: http.StatusMethodNotAllowed, want: ports.ErrSCMNotMergeable},
		{name: "repo archived (423)", status: http.StatusLocked, want: ports.ErrSCMNotMergeable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, p, host, scheme := testServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte("err"))
			}))
			_, err := p.MergePullRequest(ctx(), validMergeRequest(host, scheme))
			if !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestMergePullRequest_RejectsInvalidArgs(t *testing.T) {
	for _, tc := range []struct {
		name   string
		modify func(r ports.SCMMergeRequest) ports.SCMMergeRequest
	}{
		{name: "non-positive number", modify: func(r ports.SCMMergeRequest) ports.SCMMergeRequest { r.PR.Number = 0; return r }},
		{name: "missing owner", modify: func(r ports.SCMMergeRequest) ports.SCMMergeRequest { r.PR.Repo.Owner = ""; return r }},
		{name: "missing name", modify: func(r ports.SCMMergeRequest) ports.SCMMergeRequest { r.PR.Repo.Name = ""; return r }},
		{name: "invalid sha", modify: func(r ports.SCMMergeRequest) ports.SCMMergeRequest { r.ExpectedHeadSHA = "abc"; return r }},
		{name: "unsupported method", modify: func(r ports.SCMMergeRequest) ports.SCMMergeRequest { r.Method = "merge"; return r }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, p, host, scheme := testServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				t.Fatal("HTTP request should not be made for invalid args")
			}))
			req := tc.modify(validMergeRequest(host, scheme))
			_, err := p.MergePullRequest(ctx(), req)
			if err == nil {
				t.Fatal("expected error for invalid args, got nil")
			}
		})
	}
}

func TestMergePullRequest_NilProvider(t *testing.T) {
	var p *Provider
	_, err := p.MergePullRequest(ctx(), validMergeRequest("h", "https"))
	if err == nil {
		t.Fatal("expected error for nil provider, got nil")
	}
}

// ---------------------------------------------------------------------------
// Review actions
// ---------------------------------------------------------------------------

func validReviewRequest(host, scheme string) ports.SCMReviewRequest {
	return ports.SCMReviewRequest{
		PR: ports.SCMPRRef{
			Repo:   ports.SCMRepo{Provider: "forgejo", Host: host, Scheme: scheme, Owner: "acme", Name: "repo", Repo: "acme/repo"},
			Number: 42,
		},
		Reviewer: "@maya",
	}
}

func TestRequestReview_SendsReviewerLogin(t *testing.T) {
	_, p, host, scheme := testServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/repos/acme/repo/pulls/42/requested_reviewers" || r.Method != http.MethodPost {
			t.Fatalf("got %s %s", r.Method, r.URL.Path)
		}
		var body struct {
			Reviewers []string `json:"reviewers"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if len(body.Reviewers) != 1 || body.Reviewers[0] != "maya" {
			t.Fatalf("reviewers = %v, want [maya]", body.Reviewers)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	if err := p.RequestReview(ctx(), validReviewRequest(host, scheme)); err != nil {
		t.Fatal(err)
	}
}

func TestRequestReview_MapsNotFound(t *testing.T) {
	_, p, host, scheme := testServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	if !errors.Is(p.RequestReview(ctx(), validReviewRequest(host, scheme)), ports.ErrSCMNotFound) {
		t.Fatal("want ErrSCMNotFound")
	}
}

func TestRequestReview_RejectsInvalidArgs(t *testing.T) {
	for _, tc := range []struct {
		name   string
		modify func(r ports.SCMReviewRequest) ports.SCMReviewRequest
	}{
		{name: "non-positive number", modify: func(r ports.SCMReviewRequest) ports.SCMReviewRequest { r.PR.Number = 0; return r }},
		{name: "missing owner", modify: func(r ports.SCMReviewRequest) ports.SCMReviewRequest { r.PR.Repo.Owner = ""; return r }},
		{name: "missing name", modify: func(r ports.SCMReviewRequest) ports.SCMReviewRequest { r.PR.Repo.Name = ""; return r }},
		{name: "missing reviewer", modify: func(r ports.SCMReviewRequest) ports.SCMReviewRequest { r.Reviewer = " "; return r }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, p, host, scheme := testServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				t.Fatal("HTTP request should not be made for invalid args")
			}))
			if err := p.RequestReview(ctx(), tc.modify(validReviewRequest(host, scheme))); err == nil {
				t.Fatal("expected error for invalid args, got nil")
			}
		})
	}
}

func TestResolveReviewThread_Unsupported(t *testing.T) {
	_, p, host, scheme := testServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("HTTP request should not be made (forgejo has no resolve endpoint)")
	}))
	req := ports.SCMReviewResolveRequest{
		PR:       ports.SCMPRRef{Repo: ports.SCMRepo{Provider: "forgejo", Host: host, Scheme: scheme, Owner: "acme", Name: "repo", Repo: "acme/repo"}, Number: 42},
		ThreadID: "abc",
	}
	if !errors.Is(p.ResolveReviewThread(ctx(), req), ports.ErrSCMUnsupported) {
		t.Fatal("want ErrSCMUnsupported")
	}
}
