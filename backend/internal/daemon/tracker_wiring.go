package daemon

import (
	"errors"
	"log/slog"
	"strings"

	scmforgejo "github.com/aoagents/agent-orchestrator/backend/internal/adapters/scm/forgejo"
	scmgitlab "github.com/aoagents/agent-orchestrator/backend/internal/adapters/scm/gitlab"
	trackerforgejo "github.com/aoagents/agent-orchestrator/backend/internal/adapters/tracker/forgejo"
	trackergithub "github.com/aoagents/agent-orchestrator/backend/internal/adapters/tracker/github"
	trackergitlab "github.com/aoagents/agent-orchestrator/backend/internal/adapters/tracker/gitlab"
	trackermulti "github.com/aoagents/agent-orchestrator/backend/internal/adapters/tracker/multi"
	"github.com/aoagents/agent-orchestrator/backend/internal/config"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func newGitHubTracker() (ports.Tracker, error) {
	return trackergithub.New(trackergithub.Options{Token: trackergithub.EnvTokenSource{EnvVars: []string{"AO_GITHUB_TOKEN"}}})
}

// newGitLabTracker constructs a host-aware GitLab tracker. AllowedHosts and
// HostTokens from GitLabConfig are passed through so the tracker can route
// self-managed GitLab issue lookups to the correct host with the correct
// token. This mirrors the SCM provider's wiring in newGitLabSCMProvider.
func newGitLabTracker(gitlabCfg config.GitLabConfig) (ports.Tracker, error) {
	hostTokens := make(map[string]scmgitlab.TokenSource, len(gitlabCfg.HostTokens))
	for host, token := range gitlabCfg.HostTokens {
		host = strings.ToLower(strings.TrimSpace(host))
		if host != "" {
			hostTokens[host] = scmgitlab.StaticTokenSource(token)
		}
	}
	return trackergitlab.New(trackergitlab.Options{
		Token:        trackergitlab.DefaultTokenSource(),
		AllowedHosts: gitlabCfg.AllowedHosts,
		HostTokens:   hostTokens,
	})
}

// newForgejoTracker constructs a host-aware Forgejo tracker. AllowedHosts and
// HostTokens from ForgejoConfig are passed through so the tracker can route
// self-hosted Forgejo issue lookups to the correct host with the correct
// token. This mirrors the SCM provider's wiring in newForgejoSCMProvider.
func newForgejoTracker(forgejoCfg config.ForgejoConfig) (ports.Tracker, error) {
	hostTokens := make(map[string]scmforgejo.TokenSource, len(forgejoCfg.HostTokens))
	for host, token := range forgejoCfg.HostTokens {
		host = strings.ToLower(strings.TrimSpace(host))
		if host != "" {
			hostTokens[host] = scmforgejo.StaticTokenSource(token)
		}
	}
	return trackerforgejo.New(trackerforgejo.Options{
		// DefaultTokenSource resolves AO_FORGEJO_TOKEN → FORGEJO_TOKEN.
		Token:        trackerforgejo.DefaultTokenSource(),
		AllowedHosts: forgejoCfg.AllowedHosts,
		HostTokens:   hostTokens,
	})
}

// newMultiTracker builds a multi-tracker dispatching to the GitHub, Forgejo,
// and GitLab sub-trackers. When one tracker fails to construct (missing
// token), the others still serve issue lookups — the same degrade-gracefully
// pattern used by newMultiSCMProvider. Returns nil when no tracker has usable
// credentials; callers must tolerate a nil ports.Tracker (the session
// service's nil-guard handles this).
func newMultiTracker(gitlabCfg config.GitLabConfig, forgejoCfg config.ForgejoConfig, logger *slog.Logger) ports.Tracker {
	var named []trackermulti.NamedTracker

	if t, err := newGitHubTracker(); err != nil {
		logTrackerDisabled(logger, "github", err)
	} else {
		named = append(named, trackermulti.NamedTracker{Key: "github", Tracker: t})
	}

	if t, err := newForgejoTracker(forgejoCfg); err != nil {
		logTrackerDisabled(logger, "forgejo", err)
	} else {
		named = append(named, trackermulti.NamedTracker{Key: "forgejo", Tracker: t})
	}

	if t, err := newGitLabTracker(gitlabCfg); err != nil {
		logTrackerDisabled(logger, "gitlab", err)
	} else {
		named = append(named, trackermulti.NamedTracker{Key: "gitlab", Tracker: t})
	}

	if len(named) == 0 {
		return nil
	}
	return trackermulti.New(named...)
}

func logTrackerDisabled(logger *slog.Logger, provider string, err error) {
	if errors.Is(err, trackergithub.ErrNoToken) || errors.Is(err, trackergitlab.ErrNoToken) ||
		errors.Is(err, trackerforgejo.ErrNoToken) {
		logger.Warn("tracker disabled: no usable token", "provider", provider, "err", err)
	} else {
		logger.Warn("tracker disabled: setup failed", "provider", provider, "err", err)
	}
}
