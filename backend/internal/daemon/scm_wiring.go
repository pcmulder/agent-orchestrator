package daemon

import (
	"context"
	"errors"
	"log/slog"

	scmforgejo "github.com/aoagents/agent-orchestrator/backend/internal/adapters/scm/forgejo"
	scmgithub "github.com/aoagents/agent-orchestrator/backend/internal/adapters/scm/github"
	scmgitlab "github.com/aoagents/agent-orchestrator/backend/internal/adapters/scm/gitlab"
	scmmulti "github.com/aoagents/agent-orchestrator/backend/internal/adapters/scm/multi"
	"github.com/aoagents/agent-orchestrator/backend/internal/config"
	"github.com/aoagents/agent-orchestrator/backend/internal/lifecycle"
	scmobserve "github.com/aoagents/agent-orchestrator/backend/internal/observe/scm"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
)

// startSCMObserver wires the provider-neutral SCM observer with GitHub,
// Forgejo, and GitLab providers via a multi Provider dispatcher. Missing
// credentials for one provider do not prevent the others from starting; the
// observer is disabled only when no provider has usable credentials.
//
// Registration order is GitHub, Forgejo, GitLab: each sub-provider only
// accepts hosts in its own allowlist, so the order only matters when a host is
// allowlisted for multiple providers — Forgejo precedes GitLab so a shared
// host is classified by the forgejo adapter (whose /pulls/N URL shape is
// distinct).
func startSCMObserver(ctx context.Context, store *sqlite.Store, lcm *lifecycle.Manager, gitlabCfg config.GitLabConfig, forgejoCfg config.ForgejoConfig, logger *slog.Logger) <-chan struct{} {
	var named []scmmulti.NamedProvider

	ghProvider, ghErr := newGitHubSCMProvider(logger)
	if ghErr != nil {
		logSCMProviderDisabled(logger, "github", ghErr)
	} else {
		named = append(named, scmmulti.NamedProvider{Key: "github", Provider: ghProvider})
	}

	fjProvider, fjErr := newForgejoSCMProvider(forgejoCfg, logger)
	if fjErr != nil {
		logSCMProviderDisabled(logger, "forgejo", fjErr)
	} else {
		named = append(named, scmmulti.NamedProvider{Key: "forgejo", Provider: fjProvider})
	}

	glProvider, glErr := newGitLabSCMProvider(gitlabCfg, logger)
	if glErr != nil {
		logSCMProviderDisabled(logger, "gitlab", glErr)
	} else {
		named = append(named, scmmulti.NamedProvider{Key: "gitlab", Provider: glProvider})
	}

	if len(named) == 0 {
		logger.Warn("scm observer disabled: no usable SCM provider")
		return closedDone()
	}
	provider := scmmulti.New(named...)
	observer := scmobserve.New(provider, store, lcm, scmobserve.Config{Logger: logger, ScopedIdentityResolver: provider})
	return observer.Start(ctx)
}

func newGitHubSCMProvider(logger *slog.Logger) (*scmgithub.Provider, error) {
	tokens := scmgithub.FallbackTokenSource{
		scmgithub.EnvTokenSource{EnvVars: []string{"AO_GITHUB_TOKEN"}},
		&scmgithub.GHTokenSource{},
	}
	return scmgithub.NewProvider(scmgithub.ProviderOptions{Token: tokens, SkipTokenPreflight: true, Logger: logger})
}

func newForgejoSCMProvider(forgejoCfg config.ForgejoConfig, logger *slog.Logger) (*scmforgejo.Provider, error) {
	tokens := scmforgejo.FallbackTokenSource{
		scmforgejo.EnvTokenSource{EnvVars: []string{"AO_FORGEJO_TOKEN", "FORGEJO_TOKEN"}},
	}
	hostTokens := make(map[string]scmforgejo.TokenSource, len(forgejoCfg.HostTokens))
	for host, token := range forgejoCfg.HostTokens {
		hostTokens[host] = scmforgejo.StaticTokenSource(token)
	}
	return scmforgejo.NewProvider(scmforgejo.ProviderOptions{
		Token:              tokens,
		SkipTokenPreflight: true,
		Logger:             logger,
		AllowedHosts:       forgejoCfg.AllowedHosts,
		HostTokens:         hostTokens,
	})
}

func newGitLabSCMProvider(gitlabCfg config.GitLabConfig, logger *slog.Logger) (*scmgitlab.Provider, error) {
	tokens := scmgitlab.FallbackTokenSource{
		scmgitlab.EnvTokenSource{EnvVars: []string{"AO_GITLAB_TOKEN"}},
		&scmgitlab.GLabTokenSource{},
	}
	hostTokens := make(map[string]scmgitlab.TokenSource, len(gitlabCfg.HostTokens))
	for host, token := range gitlabCfg.HostTokens {
		hostTokens[host] = scmgitlab.StaticTokenSource(token)
	}
	return scmgitlab.NewProvider(scmgitlab.ProviderOptions{
		Token:              tokens,
		SkipTokenPreflight: true,
		Logger:             logger,
		AllowedHosts:       gitlabCfg.AllowedHosts,
		HostTokens:         hostTokens,
	})
}

func logSCMProviderDisabled(logger *slog.Logger, provider string, err error) {
	if errors.Is(err, scmgithub.ErrNoToken) || errors.Is(err, scmgithub.ErrAuthFailed) ||
		errors.Is(err, scmgitlab.ErrNoToken) || errors.Is(err, scmgitlab.ErrAuthFailed) ||
		errors.Is(err, scmforgejo.ErrNoToken) || errors.Is(err, scmforgejo.ErrAuthFailed) {
		logger.Warn("scm provider disabled: no usable token", "provider", provider, "err", err)
	} else {
		logger.Warn("scm provider disabled: setup failed", "provider", provider, "err", err)
	}
}

// newMultiSCMProvider builds a multi-provider for use outside the polling
// observer (e.g. session service PR claiming). Registration order is GitHub,
// Forgejo, GitLab (see startSCMObserver). Returns nil when no provider has
// usable credentials — callers must tolerate a nil SCM.
func newMultiSCMProvider(gitlabCfg config.GitLabConfig, forgejoCfg config.ForgejoConfig, logger *slog.Logger) *scmmulti.Provider {
	var named []scmmulti.NamedProvider
	if gh, err := newGitHubSCMProvider(logger); err == nil {
		named = append(named, scmmulti.NamedProvider{Key: "github", Provider: gh})
	}
	if fj, err := newForgejoSCMProvider(forgejoCfg, logger); err == nil {
		named = append(named, scmmulti.NamedProvider{Key: "forgejo", Provider: fj})
	}
	if gl, err := newGitLabSCMProvider(gitlabCfg, logger); err == nil {
		named = append(named, scmmulti.NamedProvider{Key: "gitlab", Provider: gl})
	}
	if len(named) == 0 {
		return nil
	}
	return scmmulti.New(named...)
}

// newMultiSCMMerger builds a multi-merger for PR merge actions, registering
// the GitHub, Forgejo, and GitLab providers. When one provider is unavailable
// (missing token), the multi-merger still routes to the healthy one — same
// degrade-gracefully pattern as newMultiSCMProvider. Returns nil when no
// provider has usable credentials.
func newMultiSCMMerger(gitlabCfg config.GitLabConfig, forgejoCfg config.ForgejoConfig, logger *slog.Logger) *scmmulti.Merger {
	var named []scmmulti.NamedMerger
	if gh, err := newGitHubSCMProvider(logger); err == nil {
		named = append(named, scmmulti.NamedMerger{Key: "github", Merger: gh})
	}
	if fj, err := newForgejoSCMProvider(forgejoCfg, logger); err == nil {
		named = append(named, scmmulti.NamedMerger{Key: "forgejo", Merger: fj})
	}
	if gl, err := newGitLabSCMProvider(gitlabCfg, logger); err == nil {
		named = append(named, scmmulti.NamedMerger{Key: "gitlab", Merger: gl})
	}
	if len(named) == 0 {
		return nil
	}
	return scmmulti.NewMerger(named...)
}

func closedDone() <-chan struct{} {
	done := make(chan struct{})
	close(done)
	return done
}
