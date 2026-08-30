package cli

import (
	"testing"
)

func TestCLIParsePRURL(t *testing.T) {
	cases := []struct {
		name       string
		raw        string
		wantHost   string
		wantOwner  string
		wantRepo   string
		wantNum    int
		wantProv   string
		wantScheme string
		wantErr    bool
	}{
		{"github", "https://github.com/owner/repo/pull/42", "github.com", "owner", "repo", 42, "github", "https", false},
		{"gitlab", "https://gitlab.com/castai/ctxd/-/merge_requests/9", "gitlab.com", "castai", "ctxd", 9, "gitlab", "https", false},
		{"gitlab nested namespace", "https://gitlab.com/group/subgroup/repo/-/merge_requests/5", "gitlab.com", "group/subgroup", "repo", 5, "gitlab", "https", false},
		{"gitlab deep nested namespace", "https://gitlab.com/group/sub1/sub2/repo/-/merge_requests/3", "gitlab.com", "group/sub1/sub2", "repo", 3, "gitlab", "https", false},
		{"forgejo https", "https://forgejo.example.com/acme/repo/pulls/9", "forgejo.example.com", "acme", "repo", 9, "forgejo", "https", false},
		{"forgejo http with port", "http://127.0.0.1:3000/acme/repo/pulls/9", "127.0.0.1:3000", "acme", "repo", 9, "forgejo", "http", false},
		{"not a valid scheme", "ftp://github.com/owner/repo/pull/1", "", "", "", 0, "", "", true},
		{"bad number", "https://github.com/owner/repo/pull/abc", "", "", "", 0, "", "", true},
		{"forgejo bad number", "https://forgejo.example.com/acme/repo/pulls/0", "", "", "", 0, "", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			parts, err := cliParsePRURL(tc.raw)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if parts.host != tc.wantHost {
				t.Errorf("host = %q, want %q", parts.host, tc.wantHost)
			}
			if parts.owner != tc.wantOwner {
				t.Errorf("owner = %q, want %q", parts.owner, tc.wantOwner)
			}
			if parts.name != tc.wantRepo {
				t.Errorf("name = %q, want %q", parts.name, tc.wantRepo)
			}
			if parts.number != tc.wantNum {
				t.Errorf("number = %d, want %d", parts.number, tc.wantNum)
			}
			if parts.provider != tc.wantProv {
				t.Errorf("provider = %q, want %q", parts.provider, tc.wantProv)
			}
			if parts.scheme != tc.wantScheme {
				t.Errorf("scheme = %q, want %q", parts.scheme, tc.wantScheme)
			}
		})
	}
}

func TestCLIRepoFromURL(t *testing.T) {
	cases := []struct {
		name       string
		raw        string
		wantHost   string
		wantOwner  string
		wantRepo   string
		wantScheme string
		wantErr    bool
	}{
		{"github https", "https://github.com/owner/repo.git", "github.com", "owner", "repo", "https", false},
		{"github https no suffix", "https://github.com/owner/repo", "github.com", "owner", "repo", "https", false},
		{"gitlab https", "https://gitlab.com/castai/ctxd.git", "gitlab.com", "castai", "ctxd", "https", false},
		{"gitlab nested namespace", "https://gitlab.com/group/subgroup/repo.git", "gitlab.com", "group/subgroup", "repo", "https", false},
		{"gitlab deep nested namespace", "https://gitlab.com/group/sub1/sub2/repo.git", "gitlab.com", "group/sub1/sub2", "repo", "https", false},
		{"gitlab self-managed nested", "https://gitlab.mycompany.com/eng/team/repo.git", "gitlab.mycompany.com", "eng/team", "repo", "https", false},
		{"gitlab self-managed port", "https://gitlab.internal:8443/eng/team/repo.git", "gitlab.internal:8443", "eng/team", "repo", "https", false},
		{"forgejo http with port", "http://127.0.0.1:3000/acme/repo.git", "127.0.0.1:3000", "acme", "repo", "http", false},
		{"ssh remote", "git@gitlab.com:group/subgroup/repo.git", "gitlab.com", "group/subgroup", "repo", "", false},
		{"ssh single group", "git@github.com:owner/repo.git", "github.com", "owner", "repo", "", false},
		{"empty", "", "", "", "", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			host, owner, repo, scheme, err := cliRepoFromURL(tc.raw)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if host != tc.wantHost {
				t.Errorf("host = %q, want %q", host, tc.wantHost)
			}
			if owner != tc.wantOwner {
				t.Errorf("owner = %q, want %q", owner, tc.wantOwner)
			}
			if repo != tc.wantRepo {
				t.Errorf("repo = %q, want %q", repo, tc.wantRepo)
			}
			if scheme != tc.wantScheme {
				t.Errorf("scheme = %q, want %q", scheme, tc.wantScheme)
			}
		})
	}
}

// TestCLIForgejoCanonicalURL verifies a forgejo PR URL round-trips: parse and
// re-canonicalize produce the same /pulls/N URL, preserving host:port and the
// http scheme of a local instance.
func TestCLIForgejoCanonicalURL(t *testing.T) {
	const raw = "http://127.0.0.1:3000/acme/repo/pulls/9"
	parts, err := cliParsePRURL(raw)
	if err != nil {
		t.Fatalf("cliParsePRURL: %v", err)
	}
	want := cliPRURLFromParts(parts.host, parts.owner, parts.name, parts.number, parts.provider, parts.scheme)
	if want != raw {
		t.Fatalf("canonical = %q, want %q", want, raw)
	}
}

// TestNumericClaimNestedNamespace verifies that a numeric claim against a
// project whose repo origin URL uses a nested GitLab namespace constructs the
// correct merge-request path and round-trips through cliParsePRURL. This is the
// regression scenario from reviewer Item 10: before the fix, cliRepoFromURL
// truncated the namespace to the first component, so a numeric claim built
// /group/subgroup/-/merge_requests/N and failed same-repository validation.
func TestNumericClaimNestedNamespace(t *testing.T) {
	const repoOrigin = "https://gitlab.com/group/subgroup/repo.git"
	host, owner, repo, scheme, err := cliRepoFromURL(repoOrigin)
	if err != nil {
		t.Fatalf("cliRepoFromURL: %v", err)
	}
	const num = 7
	mrURL := cliPRURLFromParts(host, owner, repo, num, cliProviderForOrigin(repoOrigin), scheme)
	const wantURL = "https://gitlab.com/group/subgroup/repo/-/merge_requests/7"
	if mrURL != wantURL {
		t.Fatalf("mrURL = %q, want %q", mrURL, wantURL)
	}
	// Round-trip: the constructed MR URL must parse back to the same
	// host/owner/repo/number so same-repository validation passes.
	parts, err := cliParsePRURL(mrURL)
	if err != nil {
		t.Fatalf("cliParsePRURL(%q): %v", mrURL, err)
	}
	if parts.host != host || parts.owner != owner || parts.name != repo || parts.number != num {
		t.Errorf("round-trip mismatch: got host=%q owner=%q repo=%q num=%d, want host=%q owner=%q repo=%q num=%d",
			parts.host, parts.owner, parts.name, parts.number, host, owner, repo, num)
	}
}

// TestCLIProviderForOrigin verifies host-based provider classification for the
// CLI's numeric-ref canonical URL (github.com → github, everything else →
// gitlab; forgejo self-hosted origins are not host-distinguishable and fall to
// gitlab here, which the daemon re-classifies by URL shape when a full PR URL
// is passed).
func TestCLIProviderForOrigin(t *testing.T) {
	cases := []struct {
		origin string
		want   string
	}{
		{"https://github.com/owner/repo", "github"},
		{"https://www.github.com/owner/repo", "github"},
		{"git@github.com:owner/repo.git", "github"},
		{"https://gitlab.com/castai/ctxd", "gitlab"},
		{"https://forgejo.example.com/acme/repo", "gitlab"},
		{"http://127.0.0.1:3000/acme/repo", "gitlab"},
	}
	for _, tc := range cases {
		if got := cliProviderForOrigin(tc.origin); got != tc.want {
			t.Errorf("cliProviderForOrigin(%q) = %q, want %q", tc.origin, got, tc.want)
		}
	}
}
