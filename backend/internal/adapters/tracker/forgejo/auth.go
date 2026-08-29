package forgejo

import (
	scmforgejo "github.com/aoagents/agent-orchestrator/backend/internal/adapters/scm/forgejo"
)

// ErrNoToken re-exports the SCM provider's canonical sentinel so the
// tracker and SCM adapter share one error identity. Callers that need to
// distinguish "no token" from other failures should use
// errors.Is(err, scmforgejo.ErrNoToken) regardless of whether the failure
// originated in the tracker or the SCM provider.
var ErrNoToken = scmforgejo.ErrNoToken

// DefaultTokenSource returns the standard Forgejo token source chain used
// by the tracker: AO_FORGEJO_TOKEN → FORGEJO_TOKEN. This mirrors the SCM
// provider's chain so both adapters honor the same precedence.
func DefaultTokenSource() scmforgejo.TokenSource {
	return scmforgejo.EnvTokenSource{EnvVars: []string{"AO_FORGEJO_TOKEN", "FORGEJO_TOKEN"}}
}
