package forgejo

import "strings"

// NormalizeHost lowercases and trims a host string. This is the canonical
// normalization used by both the SCM provider and the tracker for host
// comparison and allowlist lookup.
func NormalizeHost(host string) string {
	return strings.ToLower(strings.TrimSpace(host))
}
