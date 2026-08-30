package scm

// This file tests the provider-neutral terminal-reconciliation pass for
// Forgejo (BUG B). Forgejo's PR listing used to drop terminal PRs, so a PR
// merged externally between polls was never re-fetched and its row stayed open
// forever. The reconcile pass (reconcileTerminalPRs) is now provider-neutral
// (it skips only GitLab, which lists state=all), so a Forgejo tracked-open PR
// missing from the listing is reconciled and its terminal transition observed.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

var fjRepo = ports.SCMRepo{
	Provider: "forgejo", Host: "127.0.0.1:3000", Scheme: "http",
	Owner: "ao", Name: "forgejo-smoke", Repo: "ao/forgejo-smoke",
}

// forgejoAwareProvider routes the Forgejo origin URL to a Forgejo repo so the
// observer can build a Forgejo subject, while delegating everything else to
// the GitHub-shaped fakeProvider.
type forgejoAwareProvider struct {
	*fakeProvider
}

func (p *forgejoAwareProvider) ParseRepository(remote string) (ports.SCMRepo, bool) {
	if strings.Contains(remote, "127.0.0.1:3000") {
		return fjRepo, true
	}
	return p.fakeProvider.ParseRepository(remote)
}

// TestPoll_ForgejoTerminalReconciliation_MergedPRFlips verifies that a
// Forgejo tracked-open PR that is missing from the current listing (merged
// externally between polls) is reconciled and its row flipped to merged on the
// next poll. Without the provider-neutral reconcile pass, the PR would stay
// open and terminate_on_pr_merge would never fire (BUG B).
func TestPoll_ForgejoTerminalReconciliation_MergedPRFlips(t *testing.T) {
	repoKey := prKey(fjRepo, 0)
	store := &fakeStore{
		sessions: []domain.SessionRecord{{ID: "fj-1", ProjectID: "fj-proj", Metadata: domain.SessionMetadata{Branch: "feat"}}},
		projects: map[string]domain.ProjectRecord{"fj-proj": {ID: "fj-proj", RepoOriginURL: "http://127.0.0.1:3000/ao/forgejo-smoke.git"}},
		prs: map[domain.SessionID][]domain.PullRequest{
			"fj-1": {{
				URL:              "http://127.0.0.1:3000/ao/forgejo-smoke/pulls/6",
				SessionID:        "fj-1",
				Number:           6,
				SourceBranch:     "feat",
				TargetBranch:     "main",
				HeadSHA:          "h6",
				Provider:         "forgejo",
				Host:             "127.0.0.1:3000",
				Repo:             "ao/forgejo-smoke",
				Title:            "PR 6",
				MetadataHash:     "durable-meta",
				CIHash:           "durable-ci",
				ReviewHash:       "durable-review",
				ReviewObservedAt: time.Unix(2, 0).UTC(),
			}},
		},
		checks: map[string][]domain.PullRequestCheck{},
	}
	merged := ports.SCMObservation{
		Fetched: true, Provider: "forgejo", Host: "127.0.0.1:3000", Repo: "ao/forgejo-smoke",
		PR: ports.SCMPRObservation{
			URL:            "http://127.0.0.1:3000/ao/forgejo-smoke/pulls/6",
			Number:         6,
			State:          "merged",
			Merged:         true,
			SourceBranch:   "feat",
			TargetBranch:   "main",
			HeadSHA:        "h6",
			Title:          "PR 6",
			Author:         "ao-admin",
			MergeCommitSHA: "msha",
		},
		CI:           ports.SCMCIObservation{Summary: string(domain.CIPassing), HeadSHA: "h6"},
		Review:       ports.SCMReviewObservation{Decision: string(domain.ReviewNone)},
		Mergeability: ports.SCMMergeabilityObservation{State: string(domain.MergeMergeable), Mergeable: true},
	}
	provider := &forgejoAwareProvider{fakeProvider: &fakeProvider{
		// Non-304 guard (listing changed) so the reconcile pass runs.
		repoGuards: map[string]ports.SCMGuardResult{repoKey: {ETag: "fj2"}},
		// The merged PR is absent from the listing (dropped between polls).
		openPRs:      map[string][]ports.SCMPRObservation{},
		observations: map[string]ports.SCMObservation{prKey(fjRepo, 6): merged},
	}}
	now := time.Unix(1400, 0).UTC()
	lc := &fakeLifecycle{}
	obs := newTestObserver(store, provider.fakeProvider, lc, now)
	// Wire the forgejo-aware provider so ParseRepository resolves the origin.
	obs.provider = provider
	// A prior ETag + cursor so the guard is a genuine non-304 transition.
	obs.Cache.RepoPRListETag[repoKey] = "fj1"
	obs.Cache.LastSyncCursor[repoKey] = now.Add(-time.Hour)

	if err := obs.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}

	// The reconcile pass must have fetched the missing PR.
	if len(provider.fetchBatches) != 1 || len(provider.fetchBatches[0]) != 1 {
		t.Fatalf("reconciliation must fetch the missing Forgejo PR; batches=%#v", provider.fetchBatches)
	}

	// The row must be persisted flipped to merged.
	flipped := false
	for _, w := range store.writes {
		if w.pr.Number == 6 && w.pr.Merged {
			flipped = true
			if w.pr.MergeCommitSHA != "msha" {
				t.Fatalf("persisted MergeCommitSHA = %q, want msha", w.pr.MergeCommitSHA)
			}
		}
	}
	if !flipped {
		t.Fatalf("Forgejo tracked-open PR was not flipped to merged; writes=%#v", store.writes)
	}

	// Lifecycle must have observed the terminal (merged) transition.
	mergedNotified := false
	for _, o := range lc.observed {
		if o.PR.Number == 6 && o.PR.Merged {
			mergedNotified = true
		}
	}
	if !mergedNotified {
		t.Fatalf("lifecycle was not notified of the merged transition; observed=%#v", lc.observed)
	}
}

// TestPoll_ForgejoTerminalReconciliation_StillOpenNoAdvance verifies the
// durable-state rule for the provider-neutral pass: a Forgejo PR that is still
// open (no-op) does NOT advance the repo ETag or sync cursor, so a 304 cannot
// make a later real transition unrecoverable.
func TestPoll_ForgejoTerminalReconciliation_StillOpenNoAdvance(t *testing.T) {
	repoKey := prKey(fjRepo, 0)
	stillOpen := ports.SCMObservation{
		Fetched: true, Provider: "forgejo", Host: "127.0.0.1:3000", Repo: "ao/forgejo-smoke",
		PR: ports.SCMPRObservation{
			URL: "http://127.0.0.1:3000/ao/forgejo-smoke/pulls/6", Number: 6, State: "open",
			SourceBranch: "feat", TargetBranch: "main", HeadSHA: "h6", Title: "PR 6", Author: "ao-admin",
		},
		CI:           ports.SCMCIObservation{Summary: string(domain.CIPassing), HeadSHA: "h6"},
		Review:       ports.SCMReviewObservation{Decision: string(domain.ReviewNone)},
		Mergeability: ports.SCMMergeabilityObservation{State: string(domain.MergeMergeable), Mergeable: true},
	}
	// The tracked row is the pre-poll (still-open) state; derive its durable
	// hashes from the SAME observation the reconcile pass will fetch, so the
	// fetched result is a byte-for-byte no-op against the stored hashes.
	localPR, _, _, _, _ := domainFromObservation("fj-1", domain.SessionRecord{}, stillOpen, domain.PullRequest{}, persistenceOptions{}, time.Unix(1, 0).UTC())
	localPR.ReviewObservedAt = time.Unix(2, 0).UTC()
	store := &fakeStore{
		sessions: []domain.SessionRecord{{ID: "fj-1", ProjectID: "fj-proj", Metadata: domain.SessionMetadata{Branch: "feat"}}},
		projects: map[string]domain.ProjectRecord{"fj-proj": {ID: "fj-proj", RepoOriginURL: "http://127.0.0.1:3000/ao/forgejo-smoke.git"}},
		prs:      map[domain.SessionID][]domain.PullRequest{"fj-1": {localPR}},
		checks:   map[string][]domain.PullRequestCheck{},
	}
	provider := &forgejoAwareProvider{fakeProvider: &fakeProvider{
		repoGuards:   map[string]ports.SCMGuardResult{repoKey: {ETag: "fj2"}},
		openPRs:      map[string][]ports.SCMPRObservation{},
		observations: map[string]ports.SCMObservation{prKey(fjRepo, 6): stillOpen},
	}}
	now := time.Unix(1400, 0).UTC()
	obs := newTestObserver(store, provider.fakeProvider, &fakeLifecycle{}, now)
	obs.provider = provider
	obs.Cache.RepoPRListETag[repoKey] = "fj1"
	cursorBefore := now.Add(-time.Hour)
	obs.Cache.LastSyncCursor[repoKey] = cursorBefore

	if err := obs.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}

	// A still-open no-op must NOT advance the repo ETag.
	if got := obs.Cache.RepoPRListETag[repoKey]; got == "fj2" {
		t.Fatalf("repo ETag advanced to %q after a still-open no-op; durable state must not advance", got)
	}
	// ...and must NOT advance the sync cursor.
	if got := obs.Cache.LastSyncCursor[repoKey]; !got.Equal(cursorBefore) {
		t.Fatalf("sync cursor advanced after a still-open no-op: got %v, want %v", got, cursorBefore)
	}
}
