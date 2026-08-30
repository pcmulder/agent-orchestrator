package scm

// This file tests that the observer plumbs the repo's API scheme into the
// per-provider identity resolution (BUG A). A plain-HTTP Forgejo instance
// must resolve its identity over http://, not the https default — otherwise
// the probe fails and author attribution degrades to branch-only discovery.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// schemeRecordingIdentityResolver implements ports.ScopedIdentityResolver and
// records the (provider, host, scheme) it was called with.
type schemeRecordingIdentityResolver struct {
	identity ports.SCMIdentity
	calls    []string
}

func (r *schemeRecordingIdentityResolver) AuthenticatedIdentityForProvider(_ context.Context, provider, host, scheme string) (ports.SCMIdentity, error) {
	r.calls = append(r.calls, provider+"|"+host+"|"+scheme)
	return r.identity, nil
}

// TestPoll_ForgejoIdentityUsesRemoteScheme verifies that a plain-HTTP Forgejo
// origin resolves its identity with scheme "http" (plumbed from the git
// remote), not an empty/https default.
func TestPoll_ForgejoIdentityUsesRemoteScheme(t *testing.T) {
	store := &fakeStore{
		sessions: []domain.SessionRecord{{ID: "fj-1", ProjectID: "fj-proj", Metadata: domain.SessionMetadata{Branch: "feat"}}},
		projects: map[string]domain.ProjectRecord{"fj-proj": {ID: "fj-proj", RepoOriginURL: "http://127.0.0.1:3000/ao/forgejo-smoke.git"}},
		prs:      map[domain.SessionID][]domain.PullRequest{},
		checks:   map[string][]domain.PullRequestCheck{},
	}
	provider := &forgejoAwareProvider{fakeProvider: &fakeProvider{
		// No tracked PRs; the origin is parsed and the identity is resolved
		// during discoverNewPRs.
		repoGuards: map[string]ports.SCMGuardResult{prKey(fjRepo, 0): {ETag: "fj1"}},
		openPRs:    map[string][]ports.SCMPRObservation{},
	}}
	now := time.Unix(1400, 0).UTC()
	scoped := &schemeRecordingIdentityResolver{identity: ports.SCMIdentity{Login: "ao-admin", Human: true}}
	obs := newTestObserver(store, provider.fakeProvider, &fakeLifecycle{}, now)
	obs.provider = provider
	obs.scopedIdentityResolver = scoped

	if err := obs.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(scoped.calls) != 1 {
		t.Fatalf("identity resolved %d times, want 1: %v", len(scoped.calls), scoped.calls)
	}
	// The scheme must be "http" — the remote's scheme plumbed through, not
	// empty (which would default to https in the Forgejo provider).
	if scoped.calls[0] != "forgejo|127.0.0.1:3000|http" {
		t.Fatalf("identity scheme = %q, want forgejo|127.0.0.1:3000|http (plain-HTTP remote must stay http)", scoped.calls[0])
	}
}

// TestPoll_ForgejoIdentity_SameHostTwoSchemesDistinct verifies that the
// identity cache key is per host+scheme: a plain-HTTP host and an https host
// on the same instance are not conflated.
func TestPoll_ForgejoIdentity_SameHostTwoSchemesDistinct(t *testing.T) {
	httpKey := identityKey("forgejo", "127.0.0.1:3000", "http")
	httpsKey := identityKey("forgejo", "127.0.0.1:3000", "https")
	emptyKey := identityKey("forgejo", "127.0.0.1:3000", "")
	if httpKey == httpsKey {
		t.Fatalf("identity keys must differ by scheme; http=%q https=%q", httpKey, httpsKey)
	}
	if httpKey == emptyKey {
		t.Fatalf("explicit http must not collapse to the empty-scheme key; %q", httpKey)
	}
	// The identity is host-scoped (not provider-only) for forgejo.
	if strings.Contains(httpKey, "127.0.0.1:3000") == false {
		t.Fatalf("identity key must include the host; %q", httpKey)
	}
}
