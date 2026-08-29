#!/usr/bin/env bash
#
# Idempotent bring-up for the local AO Forgejo test instance.
#
#   ./setup.sh            fresh start (or resume an existing instance)
#   ./setup.sh --reset    tear everything down first (docker compose down -v),
#                         then do a full fresh start
#
# What it does:
#   1. docker compose up -d (postgres:17 + forgejo, loopback only)
#   2. wait until /api/healthz reports {"status":"pass"}
#   3. create the admin user (admin CLI) if it does not exist
#   4. create a personal access token with repo/issue/user read+write scopes
#      (reused when a previously issued token is still valid); org/repo
#      scaffolding uses a separate one-off admin token (needs write:organization)
#   5. create the org "ao" and repo "forgejo-smoke" (ao/forgejo-smoke)
#   6. print a ready env snippet (AO_FORGEJO_ALLOWED_HOSTS / AO_FORGEJO_HOST_TOKENS /
#      AO_FORGEJO_TOKEN) matching the AO forgejo provider config contract.
#
# All data and the token file live under ${AO_DATA_DIR:-$HOME/.ao}/forgejo.
# Nothing is ever written into the repository working tree.

set -euo pipefail
cd "$(dirname "$0")"

DATA_ROOT="${AO_FORGEJO_DATA_DIR:-${AO_DATA_DIR:-$HOME/.ao}/forgejo}"
STATE_DIR="$DATA_ROOT/state"
PORT="${AO_FORGEJO_TEST_PORT:-3000}"
BASE_URL="http://127.0.0.1:${PORT}"
HOST_KEY="127.0.0.1:${PORT}"
ADMIN_USER="ao-admin"
ADMIN_EMAIL="admin@localhost"
ORG="ao"
REPO="forgejo-smoke"

# Token scopes: repo (covers pulls) + issues, read+write. Forgejo's scope set
# has no dedicated pull_request scope — pulls ride on repository scopes.
# read:user is required for the standard GET /api/v1/user auth probe.
TOKEN_SCOPES="read:repository,write:repository,read:issue,write:issue,read:user"

compose() { docker compose -f compose.yaml "$@"; }
# The Forgejo admin CLI refuses to run as root; the image runs as root by
# default, so `compose exec` must drop to the app user (uid/gid 1000).
exec_admin() { compose exec -T -u 1000:1000 forgejo forgejo -w /data "$@"; }

log() { printf '[forgejo-test] %s\n' "$*"; }

if [ "${1:-}" = "--reset" ]; then
  log "reset: docker compose down -v"
  compose down -v
fi

fail() { printf '[forgejo-test] ERROR: %s\n' "$1" >&2; exit 1; }

command -v docker >/dev/null 2>&1 || fail "docker is not on PATH"
command -v curl >/dev/null 2>&1 || fail "curl is not on PATH"

mkdir -p "$STATE_DIR"
TOKEN_FILE="$STATE_DIR/token"
ADMIN_PW_FILE="$STATE_DIR/admin-password"

# --- 1. bring up ----------------------------------------------------------
log "docker compose up -d"
compose up -d

# --- 2. wait for healthz ---------------------------------------------------
# Forgejo 16 exposes its health API at /api/healthz (returns {"status":"pass",
# ...}); the v1 API path /api/v1/healthz does not exist in this build and 404s.
# We wait on the real endpoint so "healthy" means actually healthy.
log "waiting for $BASE_URL/api/healthz"
ok=""
for _ in $(seq 1 90); do
  if body="$(curl -fsS --max-time 5 "$BASE_URL/api/healthz" 2>/dev/null)" && printf '%s' "$body" | grep -qE '"status" *: *"pass"'; then
    ok=1
    break
  fi
  sleep 2
done
[ -n "$ok" ] || fail "forgejo did not become healthy; check: compose ps && compose logs forgejo | tail"
log "healthz: $(printf '%s' "$body" | grep '"status"' | head -1)"

# --- 3. admin user ----------------------------------------------------------
admin_exists() {
  exec_admin admin user list 2>/dev/null \
    | grep -qE "(^|[^a-zA-Z0-9_])${ADMIN_USER}([^a-zA-Z0-9_]|$)"
}

if admin_exists; then
  log "admin user '$ADMIN_USER' already exists"
else
  ADMIN_PW="$(openssl rand -hex 16)"
  log "creating admin user '$ADMIN_USER' (password stored in $ADMIN_PW_FILE)"
  exec_admin admin user create \
    --username "$ADMIN_USER" \
    --password "$ADMIN_PW" \
    --email "$ADMIN_EMAIL" \
    --admin >/dev/null
  printf '%s' "$ADMIN_PW" >"$ADMIN_PW_FILE"
  chmod 600 "$ADMIN_PW_FILE"
fi

# --- 4. personal access tokens ---------------------------------------------
api_ok() { curl -fsS --max-time 5 -H "Authorization: token $1" "$BASE_URL/api/v1/user" >/dev/null 2>&1; }

# A short-lived admin token (scopes: all) is used for one thing only: the
# org/repo scaffolding below needs write:organization. It is NOT persisted —
# the provider PAT (next) carries the contract scopes only.
log "issuing one-off admin token for org/repo scaffolding (scopes: all)"
ADMIN_TOKEN="$(exec_admin admin user generate-access-token \
  --username "$ADMIN_USER" \
  --token-name "ao-forgejo-setup-$(date +%s)" \
  --scopes all \
  --raw)"
[ -n "$ADMIN_TOKEN" ] || fail "admin token generation returned no value; check: compose logs forgejo"
api_ok "$ADMIN_TOKEN" || fail "admin token cannot authenticate against $BASE_URL/api/v1/user"

if [ -f "$TOKEN_FILE" ] && TOKEN="$(tr -d '[:space:]' <"$TOKEN_FILE")" && api_ok "$TOKEN"; then
  log "reusing existing provider token from $TOKEN_FILE"
else
  log "issuing new access token (scopes: $TOKEN_SCOPES)"
  # Forgejo token names are unique per user; a unique suffix avoids
  # "name has been used already" when re-issuing on top of a stale token.
  TOKEN="$(exec_admin admin user generate-access-token \
    --username "$ADMIN_USER" \
    --token-name "ao-forgejo-smoke-$(date +%s)" \
    --scopes "$TOKEN_SCOPES" \
    --raw)"
  [ -n "$TOKEN" ] || fail "token generation returned no value; check: compose logs forgejo"
  api_ok "$TOKEN" || fail "newly issued token cannot authenticate against $BASE_URL/api/v1/user"
  printf '%s' "$TOKEN" >"$TOKEN_FILE"
  chmod 600 "$TOKEN_FILE"
fi

# --- 5. org + repo -----------------------------------------------------------
# The provider token has no organization scope, so probe org existence with
# the admin token (the repo probe uses the provider token: repo read/write is
# in its scope set).
if ! curl -fsS -H "Authorization: token $ADMIN_TOKEN" "$BASE_URL/api/v1/orgs/$ORG" >/dev/null 2>&1; then
  log "creating org '$ORG'"
  curl -fsS -X POST \
    -H "Authorization: token $ADMIN_TOKEN" \
    -H "Content-Type: application/json" \
    -d "{\"username\":\"$ORG\",\"description\":\"AO Forgejo smoke-test org\",\"visibility\":\"public\"}" \
    "$BASE_URL/api/v1/orgs" >/dev/null
fi

if ! curl -fsS -H "Authorization: token $TOKEN" "$BASE_URL/api/v1/repos/$ORG/$REPO" >/dev/null 2>&1; then
  log "creating repo '$ORG/$REPO'"
  curl -fsS -X POST \
    -H "Authorization: token $ADMIN_TOKEN" \
    -H "Content-Type: application/json" \
    -d "{\"name\":\"$REPO\",\"auto_init\":true,\"default_branch\":\"main\",\"private\":false,\"description\":\"AO forgejo provider smoke repo\"}" \
    "$BASE_URL/api/v1/orgs/$ORG/repos" >/dev/null
fi

curl -fsS -H "Authorization: token $TOKEN" "$BASE_URL/api/v1/repos/$ORG/$REPO" | grep -q '"full_name":"'"$ORG"/"$REPO"'"' \
  || fail "repo $ORG/$REPO did not come up"
log "org + repo ready: $ORG/$REPO"

# --- 6. ready snippet ----------------------------------------------------------
log "ready."
cat <<EOF

# AO forgejo provider config (export these, or add to your env):
AO_FORGEJO_ALLOWED_HOSTS=$HOST_KEY
AO_FORGEJO_HOST_TOKENS=$HOST_KEY=$TOKEN
AO_FORGEJO_TOKEN=$TOKEN

# Useful extras:
#   server      : $BASE_URL  (api: $BASE_URL/api/v1)
#   git clone   : $BASE_URL/$ORG/$REPO.git
#   token file  : $TOKEN_FILE
#   admin user  : ao-admin (password in $ADMIN_PW_FILE, web login at $BASE_URL)
#   reset       : ./setup.sh --reset   (or: docker compose -f compose.yaml down -v)
EOF
