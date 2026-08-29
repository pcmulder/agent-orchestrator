# Local Forgejo test instance

Docker-based [Forgejo](https://forgejo.org/) instance for developing and
testing AO's new `forgejo` provider (SCM + tracker). Brings up the official
image plus a `postgres:17` backing store and creates the fixtures the smoke
tests need: an admin user, an org/repo (`ao/forgejo-smoke`), and a personal
access token with repo/issue read+write scopes (pulls ride on the repository
scopes; plus `read:user` for the auth probe — see "Known limitations").

Conventions mirror `cloud/compose.yaml`: fixed compose project name,
loopback-only port binding, postgres:17, healthchecks, init SQL under
`test/forgejo/`, and **all** data under `${AO_DATA_DIR:-$HOME/.ao}` (see
`AGENTS.md` — all app state lives under `~/.ao`; nothing is written
elsewhere).

## Files

| Path | Purpose |
| --- | --- |
| `compose.yaml` | `postgres:17` + `codeberg.org/forgejo/forgejo:16.0.3` (latest stable as of 2026-08-29). Project name `ao-forgejo-local`. Ports bound to `127.0.0.1` only. |
| `postgres/init.sql` | Runs once on a fresh volume: creates the non-privileged `ao_forgejo` owner role, sets ownership, locks down the bootstrap role. |
| `setup.sh` | Idempotent bring-up. Waits on `healthz`, creates admin + token + `ao/forgejo-smoke`, prints the ready env snippet. `--reset` wipes state first. |
| `README.md` | This file. |

> The compose file is the source of truth for the image tag. To pin a newer
> stable release, update the `image:` line in `compose.yaml` and confirm it
> pulls (`docker pull codeberg.org/forgejo/forgejo:<tag>`).

## Quickstart

```bash
cd test/forgejo

# Fresh start (or resume). Waits for healthz, then prints the env snippet.
./setup.sh

# Wipe all state (containers + volumes + token) and start over.
./setup.sh --reset
```

The compose file is validated with:

```bash
docker compose -f test/forgejo/compose.yaml config -q
```

### Verify

> **Health endpoint note:** Forgejo 16 serves its health API at
> `GET /api/healthz` (returns `{"status":"pass", ...}`). The v1 API path
> `/api/v1/healthz` does **not** exist in this build and 404s — use
> `/api/healthz` for readiness checks.

```bash
PORT="${AO_FORGEJO_TEST_PORT:-3000}"
DATA_ROOT="${AO_FORGEJO_DATA_DIR:-${AO_DATA_DIR:-$HOME/.ao}/forgejo}"
TOKEN="$(tr -d '[:space:]' < "$DATA_ROOT/state/token")"  # or the ready-snippet value

# healthz
curl -fsS "http://127.0.0.1:${PORT}/api/healthz" | grep '"status"'

# authenticated user
curl -fsS -H "Authorization: token ${TOKEN}" \
  "http://127.0.0.1:${PORT}/api/v1/user"

# list repos
curl -fsS -H "Authorization: token ${TOKEN}" \
  "http://127.0.0.1:${PORT}/api/v1/user/repos"

# the smoke repo
curl -fsS -H "Authorization: token ${TOKEN}" \
  "http://127.0.0.1:${PORT}/api/v1/repos/ao/forgejo-smoke"
```

### Reset

```bash
./setup.sh --reset
# or, equivalent low-level:
docker compose -f test/forgejo/compose.yaml down -v
```

`down -v` removes the named volumes; the token and admin-password files under
`$DATA_ROOT/state/` are re-created on the next `setup.sh` run.

## Configuration

| Variable | Default | Meaning |
| --- | --- | --- |
| `AO_FORGEJO_TEST_PORT` | `3000` | HTTP port for the Forgejo server. |
| `AO_FORGEJO_TEST_DB_PORT` | `54330` | Postgres port, loopback, for diagnostics only (e.g. `psql -h 127.0.0.1 -p 54330 -U ao_forgejo -d forgejo`). |
| `AO_DATA_DIR` | `~/.ao` | Root for all data. |
| `AO_FORGEJO_DATA_DIR` | `$AO_DATA_DIR/forgejo` | Overrides the Forgejo data root entirely (postgres volume + app dir + state). |

Server URL: `http://127.0.0.1:3000` (overridable via `AO_FORGEJO_TEST_PORT`).

Data locations (default data root: `${AO_DATA_DIR:-$HOME/.ao}` — in this
environment `AO_DATA_DIR` is set, so it resolves there):

- Data root: `${AO_FORGEJO_DATA_DIR:-${AO_DATA_DIR:-$HOME/.ao}/forgejo}`
- Postgres data: `$DATA_ROOT/postgres`
- Forgejo app (config + repo data): `$DATA_ROOT/app`
- Token + admin password: `$DATA_ROOT/state/`

## API surface

Base URL: `http://127.0.0.1:3000/api/v1` (Forgejo 16 / Gitea 1.22 API).
Authenticate with `Authorization: token <PAT>`. The full route list is
available at `GET /swagger.v1.json` (the HTML swagger UI is at
`/api/v1/swagger`).

Endpoints the AO `forgejo` provider adapter is expected to hit — shapes
below were verified live against this instance on 2026-08-29 (see the
"Shape traps" list for the places that differ from GitHub):

| Area | Endpoint | Notes |
| --- | --- | --- |
| Health | `GET /api/healthz` | **Outside** `/api/v1`. Returns `{"status":"pass", ...}`. The v1 path `/api/v1/healthz` 404s in this build. |
| Identity | `GET /user` | Requires `read:user` scope. Returns the token's owner. |
| Repos | `GET /user/repos`, `GET /repos/{owner}/{repo}` | `default_branch`, `clone_url` (note: `http://…`), `html_url`. |
| Branches | `GET /repos/{owner}/{repo}/branches` | Item: `{name, commit:{id, message, author:{...}, committer, url}}`. **`commit.id` is the SHA** — no `commit.sha` field. |
| Pulls (list) | `GET /repos/{owner}/{repo}/pulls` | **`/pulls` is plural.** Do not port the GitHub `/pull/{index}` URL shape. |
| Single PR | `GET /repos/{owner}/{repo}/pulls/{index}` | `index` is the PR number. State: `open` / `closed`; `merged` (bool) + `merged_at` distinguish merged from closed; `mergeable` present. |
| PR commits | `GET /repos/{owner}/{repo}/pulls/{index}/commits` | Verified. |
| Create PR | `POST /repos/{owner}/{repo}/pulls` | Body `{head, base, title, body}`. Verified (PR #1 created this way). |
| Merge PR | `POST /repos/{owner}/{repo}/pulls/{index}/merge` | Body `{"Do":"merge"}` — **capital `D`**; enum: `merge`, `rebase`, `rebase-merge`, `squash`, `fast-forward-only`, `manually-merged`. GitHub's `do_merge` 422s. Verified. |
| Issues | `GET /repos/{owner}/{repo}/issues` | **Includes PRs** (PR #1 appeared in the list). Filter on `pull_request == null` for pure issues. |
| Issue | `GET /repos/{owner}/{repo}/issues/{index}` | Body, labels, state. |
| Comments | `GET/POST /repos/{owner}/{repo}/issues/{index}/comments` | Works for issues **and** PRs (PRs are issues; both verified). |
| Status (post) | `POST /repos/{owner}/{repo}/statuses/{sha}` | Body `{state, context, description}` — `state` on *input*. |
| Status (list) | `GET /repos/{owner}/{repo}/statuses/{sha}` | Array; **each item uses field `status` (not `state`)**: `{status, context, description}`. |
| Status (aggregate) | `GET /repos/{owner}/{repo}/commits/{sha}/status` | GitHub-like: `{state, sha, total_count, statuses:[...]}` — `state` on *output*. `GET …/commits/{sha}/statuses` returns the raw list. Verified. |
| Commits (list) | `GET /repos/{owner}/{repo}/commits?sha={branch}` | Item: `{sha, commit:{message, author:{...}}}`. |
| Commit (raw) | `GET /repos/{owner}/{repo}/git/commits/{sha}` | **There is no `GET /commits/{sha}`** (404). Raw object: `{sha, parents, author, committer, commit, ...}`; message/author nested under `commit`. |
| PR by refs | `GET /repos/{owner}/{repo}/pulls/{base}/{head}` | Resolves the open PR for a ref pair. |

### Shape traps vs GitHub (the adapter must not assume GitHub field names)

1. Status objects serialize as `status`, not `state` (the aggregate
   `/commits/{sha}/status` uses `state` — so prefer the aggregate, or handle
   both field names).
2. Branch/commit objects use `commit.id` for the SHA.
3. There is no `GET /commits/{sha}`; use `/git/commits/{sha}` (raw) or
   `/commits` (list with `?sha=`).
4. Merge body is `Do` (capital D) with a different enum than GitHub's
   `merge_method`.
5. Health endpoint is `/api/healthz`, not `/api/v1/healthz`.
6. PR URLs are `/pulls/{index}` (plural) — a discriminator from GitHub
   (`/pull/`) and GitLab (`/-/merge_requests/`).

## How AO tests consume this

The provider config contract (owned by the sibling worker; do not deviate):

- provider key: `forgejo`
- `AO_FORGEJO_TOKEN` — a single personal access token (used when one host).
- `AO_FORGEJO_ALLOWED_HOSTS` — comma-separated `host[:port]` allowlist.
- `AO_FORGEJO_HOST_TOKENS` — `host=token,host=token` per-host map.

`setup.sh` prints exactly this snippet on success:

```bash
AO_FORGEJO_ALLOWED_HOSTS=127.0.0.1:3000
AO_FORGEJO_HOST_TOKENS=127.0.0.1:3000=<token>
AO_FORGEJO_TOKEN=<token>
```

Tests point the `forgejo` provider at `http://127.0.0.1:3000` with that token
and exercise the same endpoints listed above against `ao/forgejo-smoke`.

## Known limitations

- **Plain HTTP, no TLS.** The server is reachable only at
  `http://127.0.0.1:3000` by default. The provider must respect the remote's
  *scheme* — do not hard-code `https://`. `ParseRepository`/`clientForHost`
  should build the base URL from the remote's actual scheme (this instance is
  `http`). The port is overridable via `AO_FORGEJO_TEST_PORT`, so any
  consumer should take the full URL, not a hard-coded one.
- **Single-user / no CI.** `auto_init` seeds the repo; there are no status
  checkers running, so `GET …/statuses/{sha}` returns an empty list unless a
  test posts a status explicitly. The web admin account is `ao-admin`
  (username `admin` is reserved by Forgejo).
- **Loopback only.** Nothing is exposed on the network.
- **Token scope mapping.** Forgejo has no dedicated pull-request scope —
  PR read/write is covered by `read:repository`/`write:repository` (plus
  `read:issue`/`write:issue` and `read:user` for the `GET /user` auth probe).
  The setup token is issued with exactly that scope set.
- **No secrets committed.** The token and admin password live only under
  `$DATA_ROOT/state/` and are regenerated on `--reset`. The compose file
  ships fixed local-only credentials (they guard loopback-only, throwaway
  data, matching `cloud/compose.yaml`).
- **Pinned image.** Bumping the Forgejo tag is a deliberate change (see the
  note under "Files"); verify the pull before relying on it.

## Security

- All ports bind to `127.0.0.1`.
- Registration is disabled; the admin account is created by the CLI.
- The issued token is the one the provider uses; keep it out of the repo.
- `setup.sh` stores the token at `0600` under `$DATA_ROOT/state/token`.
  (`$DATA_ROOT` is `${AO_FORGEJO_DATA_DIR:-${AO_DATA_DIR:-$HOME/.ao}/forgejo}`.)
