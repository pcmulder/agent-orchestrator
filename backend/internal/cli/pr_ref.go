package cli

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

func (c *commandContext) resolvePRRef(ctx context.Context, ref string, project projectDetails) (string, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return "", usageError{errors.New("PR reference must be a PR/MR URL or a number")}
	}
	if isNumericPRRef(ref) {
		repo := strings.TrimSpace(project.Repo)
		if repo == "" {
			// The daemon must not shell out to external CLIs from its loopback API;
			// when the durable project record lacks repo_origin_url, the thin CLI
			// does the one-off gh lookup from the registered project checkout and
			// sends the daemon a normalized URL.
			out, err := c.deps.CommandOutputInDir(ctx, project.Path, "gh", "repo", "view", "--json", "url", "-q", ".url")
			if err != nil || strings.TrimSpace(string(out)) == "" {
				return "", usageError{errors.New("gh not available; pass the full PR URL")}
			}
			repo = strings.TrimSpace(string(out))
		}
		host, owner, name, scheme, err := cliRepoFromURL(repo)
		if err != nil {
			return "", usageError{errors.New("PR reference must be a PR/MR URL or a number")}
		}
		n, _ := strconv.Atoi(strings.TrimPrefix(ref, "#"))
		// For a numeric ref the provider is unknown to the CLI (a forgejo
		// origin looks like any self-hosted git remote). It defaults to gitlab
		// here, matching the daemon's host-based default; the daemon re-resolves
		// numeric refs against its configured providers, so pass the full PR URL
		// (rather than a number) for a forgejo project to be routed correctly.
		return cliPRURLFromParts(host, owner, name, n, cliProviderForOrigin(repo), scheme), nil
	}
	parts, err := cliParsePRURL(ref)
	if err != nil || parts.host == "" || parts.owner == "" || parts.name == "" || parts.number <= 0 {
		return "", usageError{errors.New("PR reference must be a PR/MR URL or a number")}
	}
	return cliPRURLFromParts(parts.host, parts.owner, parts.name, parts.number, parts.provider, parts.scheme), nil
}

func isNumericPRRef(ref string) bool {
	ref = strings.TrimPrefix(strings.TrimSpace(ref), "#")
	n, err := strconv.Atoi(ref)
	return err == nil && n > 0
}

// cliPRURLFromParts constructs the canonical PR/MR URL for a provider.
// GitHub uses /pull/N; GitLab uses /-/merge_requests/N; Forgejo uses /pulls/N.
// The scheme is preserved (so a plain-HTTP local instance keeps http://);
// an empty scheme defaults to https.
func cliPRURLFromParts(host, owner, repo string, number int, provider, scheme string) string {
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

// cliProviderForOrigin classifies the provider of a repo origin URL by host.
// github.com hosts are github; everything else defaults to gitlab (the CLI has
// no configured-provider context to distinguish forgejo from gitlab on a
// self-hosted host).
func cliProviderForOrigin(repoOrigin string) string {
	if isCLIGitHubHost(hostFromOrigin(repoOrigin)) {
		return "github"
	}
	return "gitlab"
}

// cliPRURLParts is the parsed form of a PR/MR URL for the CLI.
type cliPRURLParts struct {
	host     string
	owner    string
	name     string
	number   int
	provider string
	scheme   string
}

func cliParsePRURL(raw string) (cliPRURLParts, error) {
	var out cliPRURLParts
	u, err := url.Parse(raw)
	if err != nil {
		return out, err
	}
	if !strings.EqualFold(u.Scheme, "https") && !strings.EqualFold(u.Scheme, "http") {
		return out, errors.New("not http(s)")
	}
	out.scheme = strings.ToLower(u.Scheme)
	// u.Host preserves any port (e.g. 127.0.0.1:3000); a forgejo local
	// instance needs it for allowlist matching and the API base.
	out.host = u.Host
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")

	// Forgejo: /owner/repo/pulls/N → 4 parts, parts[2] == "pulls" (PLURAL —
	// checked BEFORE the GitHub /pull/ form so the two never collide).
	if len(parts) == 4 && parts[2] == "pulls" {
		n, parseErr := strconv.Atoi(parts[3])
		if parseErr != nil || n <= 0 {
			return out, errors.New("bad number")
		}
		out.owner, out.name, out.number, out.provider = parts[0], strings.TrimSuffix(parts[1], ".git"), n, "forgejo"
		return out, nil
	}

	// GitHub: /owner/repo/pull/N → 4 parts, parts[2] == "pull"
	if len(parts) == 4 && parts[2] == "pull" {
		n, parseErr := strconv.Atoi(parts[3])
		if parseErr != nil || n <= 0 {
			return out, errors.New("bad number")
		}
		out.owner, out.name, out.number, out.provider = parts[0], strings.TrimSuffix(parts[1], ".git"), n, "github"
		return out, nil
	}

	// GitLab: /owner/repo/-/merge_requests/N
	// Supports nested groups: /group/subgroup/repo/-/merge_requests/N
	if len(parts) >= 5 && parts[len(parts)-2] == "merge_requests" && parts[len(parts)-3] == "-" {
		n, parseErr := strconv.Atoi(parts[len(parts)-1])
		if parseErr != nil || n <= 0 {
			return out, errors.New("bad number")
		}
		repoParts := parts[:len(parts)-3]
		if len(repoParts) < 2 {
			return out, errors.New("bad repo path")
		}
		out.owner = strings.Join(repoParts[:len(repoParts)-1], "/")
		out.name = strings.TrimSuffix(repoParts[len(repoParts)-1], ".git")
		out.number, out.provider = n, "gitlab"
		return out, nil
	}

	return out, errors.New("not a PR/MR URL")
}

func cliRepoFromURL(raw string) (host, owner, name, scheme string, err error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", "", "", "", errors.New("empty repo")
	}
	if strings.HasPrefix(raw, "git@") {
		rest := strings.TrimPrefix(raw, "git@")
		colonIdx := strings.Index(rest, ":")
		if colonIdx < 0 {
			return "", "", "", "", errors.New("bad ssh remote")
		}
		host = rest[:colonIdx]
		path := rest[colonIdx+1:]
		parts := strings.Split(strings.TrimSuffix(path, ".git"), "/")
		if len(parts) < 2 {
			return "", "", "", "", errors.New("bad repo")
		}
		for _, seg := range parts {
			if seg == "" {
				return "", "", "", "", errors.New("bad repo")
			}
		}
		name = parts[len(parts)-1]
		owner = strings.Join(parts[:len(parts)-1], "/")
		return host, owner, name, "", nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", "", "", "", err
	}
	// u.Host preserves any port (e.g. 127.0.0.1:3000). The scheme is carried
	// through so a plain-HTTP self-hosted instance keeps its API scheme.
	host = u.Host
	if u.Scheme == "http" || u.Scheme == "https" {
		scheme = u.Scheme
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) < 2 {
		return "", "", "", "", errors.New("bad repo")
	}
	for _, seg := range parts {
		if seg == "" {
			return "", "", "", "", errors.New("bad repo")
		}
	}
	name = strings.TrimSuffix(parts[len(parts)-1], ".git")
	owner = strings.Join(parts[:len(parts)-1], "/")
	return host, owner, name, scheme, nil
}

// hostFromOrigin extracts the host from a git origin URL (https/ssh).
func hostFromOrigin(raw string) string {
	raw = strings.TrimSpace(raw)
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

func isCLIGitHubHost(host string) bool {
	host = strings.ToLower(host)
	return host == "github.com" || host == "www.github.com" || host == "api.github.com" ||
		strings.HasSuffix(host, ".github.com") || strings.HasSuffix(host, ".ghe.io")
}
