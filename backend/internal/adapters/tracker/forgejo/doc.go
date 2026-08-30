// Package forgejo implements the ports.Tracker outbound port for Forgejo
// (Gitea-compatible) issues. v1 is read-only:
//
//   - Get returns a normalized snapshot of one issue (spawn-bootstrap
//     reads it to hydrate the agent prompt).
//   - List returns a filtered slice of issues in a repository, paginated via
//     the Link-header with state/labels/assignee filters. Issues are
//     excluded from the listing (the Forgejo issues endpoint returns both
//     issues and pull requests; only the plain-issue rows are kept).
//   - Preflight performs a single GET /user to verify the token is accepted;
//     success is cached for the lifetime of the Tracker, failures are not.
//
// The adapter reuses the SCM forgejo TokenSource chain
// (AO_FORGEJO_TOKEN / FORGEJO_TOKEN).
//
// # State mapping
//
// Forgejo issues have two native states: open and closed. They map onto the
// normalized state vocabulary as follows:
//
//   - open -> open
//   - closed -> done
//
// # Host handling
//
// Every Forgejo instance is self-hosted, so there is no default public host.
// All hosts must be in AllowedHosts; unconfigured hosts are rejected before
// any credential is attached.
package forgejo
