// Package forgejo observes Forgejo (Gitea-compatible) pull requests for AO's
// SCM integrations.
//
// It implements the provider-neutral scm.Provider interface against the
// Forgejo REST API v1 (https://<host>/api/v1). Forgejo's API is Gitea's v1
// surface: owner/repo pull requests at /repos/{owner}/{repo}/pulls, commit
// statuses at /commits/{sha}/statuses, and pull reviews at /pulls/{index}/reviews.
//
// Unlike GitLab, every Forgejo instance is self-hosted: there is no
// "default" public host, so ALL hosts must appear in the provider's
// allowlist (ProviderOptions.AllowedHosts) before any credential is attached.
// The API base is <scheme>://<host>/api/v1, where the scheme is taken from
// the git remote URL (a local test instance served over plain HTTP is
// addressed with http://) and defaults to https.
//
// # State mapping
//
//   - PR state: from pull_request.state (open/closed) + the draft and merged
//     booleans.
//     | state  | merged | draft | domain.PRState |
//     |--------|--------|-------|----------------|
//     | open   | false  | true  | draft          |
//     | open   | false  | false | open           |
//     | *      | true   | *     | merged         |
//     | closed | false  | *     | closed         |
//
//   - CI: derived from the commit statuses of the head SHA.
//     | Combined state       | domain.CIState |
//     |----------------------|----------------|
//     | success              | passing        |
//     | error, failure       | failing        |
//     | pending, warning     | pending        |
//     | no statuses / other  | unknown        |
//
//   - Review: derived from the pull's submitted reviews.
//     | Condition                              | domain.ReviewDecision   |
//     |----------------------------------------|-------------------------|
//     | any non-dismissed REQUEST_CHANGES      | changes_requested       |
//     | any non-dismissed APPROVED             | approved                |
//     | otherwise                              | none                    |
//
//   - Mergeability: from pull_request.mergeable (a plain bool on Forgejo)
//     plus the draft flag. CI/review blockers are layered on by the shared
//     mergeabilityFromPR helper (see gitlab's for the full table).
//
// # Incremental discovery
//
// The Forgejo API has no updated_after filter and no ETag revalidation on
// list endpoints. RepoPRListGuard therefore fingerprints the all-states PR
// listing (state=all, sha256 of the response) and reports NotModified when the
// fingerprint is unchanged, so steady-state repos skip the per-PR detail
// fetches; a PR transitioning to merged/closed changes the fingerprint, so the
// transition is detected on the next poll (matching GitLab's state=all guard).
// ListPRsByRepo lists state=all (matching GitLab) so closed/merged PRs are
// observed by the normal refresh path, and ignores its updatedAfter cursor
// (there is no equivalent filter). CommitChecksGuard fingerprints the
// commit-status set the same way. The observer's DefaultPRMaxAge still bounds
// staleness for the head-SHA-unchanged case.
package forgejo
