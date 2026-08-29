package forgejo

import "testing"

// TestRepoPathEncodesNestedOwner locks in the Gitea/Forgejo URL convention:
// nested owner namespaces (org/sub) are percent-encoded into the single
// {username} path segment, while the repo name is a separate segment. This is
// distinct from GitLab (which escapes owner/name as one %2F-joined segment).
func TestRepoPathEncodesNestedOwner(t *testing.T) {
	cases := []struct {
		owner, name string
		parts       []string
		want        string
	}{
		{"acme", "repo", nil, "/repos/acme/repo"},
		{"acme", "repo", []string{"pulls", "7"}, "/repos/acme/repo/pulls/7"},
		{"org/sub", "repo", nil, "/repos/org%2Fsub/repo"},
		{"org/sub", "repo", []string{"pulls", "9", "merge"}, "/repos/org%2Fsub/repo/pulls/9/merge"},
		{"a/b/c", "repo", []string{"commits", "sha1", "statuses"}, "/repos/a%2Fb%2Fc/repo/commits/sha1/statuses"},
	}
	for _, tc := range cases {
		got := repoPath(tc.owner, tc.name, tc.parts...)
		if got != tc.want {
			t.Errorf("repoPath(%q,%q,%v) = %q, want %q", tc.owner, tc.name, tc.parts, got, tc.want)
		}
	}
}
