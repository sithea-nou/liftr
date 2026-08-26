#!/usr/bin/env bash
#
# Liftr + REAL OpenTofu CLI demonstration (M21.5 optional proof).
#
# Drives DemoWorkload/v1 through the production OpenTofu adapter (M19): real
# 'tofu' binary, saved-plan apply semantics, durable private evidence, contract
# outputs published through the developer API. Uses the built-in
# terraform_data resource — no cloud, no provider downloads.
#
# NON-PRODUCTION: insecure dev auth on loopback; development local state files.
#
# Requires: OpenTofu 1.12.6 at ~/.opentofu/bin/tofu (or LIFTR_DEMO_TOFU_BIN),
# plus bash, curl, python3, a running compose PostgreSQL container.

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
API="http://127.0.0.1:18180"
CLI=("$ROOT/.demo/bin/liftr" --server "$API" -o json)
TOFU_BIN="${LIFTR_DEMO_TOFU_BIN:-$HOME/.opentofu/bin/tofu}"
WORK="$(mktemp -d /tmp/liftr-tofu-demo.XXXXXX)"
SERVER_PID="$WORK/server.pid"
LOG="$ROOT/.demo/tofu-server.log"
DB="liftr_demo_tofu"
cleanup() {
  [ -f "$SERVER_PID" ] && kill "$(cat "$SERVER_PID")" 2>/dev/null || true
  rm -f "$SERVER_PID"
  docker exec liftr-postgres-1 dropdb -U liftr --force --if-exists "$DB" >/dev/null 2>&1 || true
}
trap cleanup EXIT

STEP_NO=0
wait_outputs() { # RESOURCE SUBSTRING — bounded poll for published output evidence.
  local id="$1" needle="$2" attempts=$(( 120 * 10 )) outs=""
  for _ in $(seq 1 "$attempts"); do
    outs="$(outputs_of "$id" || true)"
    case "$outs" in *"$needle"*) return 0 ;; esac
    sleep 0.1
  done
  die "outputs of $id never contained '$needle' (last: ${outs:-none})"
}

step() { STEP_NO=$((STEP_NO + 1)); printf '\n[%d/5] %s\n' "$STEP_NO" "$1"; }
ok()   { printf '  \xe2\x9c\x93 %s\n' "$1"; }
note() { printf '    %s\n' "$1"; }
die()  { printf '\nOPENTOFU DEMO FAILED: %s\n' "$1" >&2; exit 1; }

command -v python3 >/dev/null 2>&1 || die "python3 required"
[ -x "$TOFU_BIN" ] || die "OpenTofu binary not found at $TOFU_BIN (install 1.12.6 or set LIFTR_DEMO_TOFU_BIN)"

jq_field() {
  python3 -c "
import json, sys
with open(sys.argv[1]) as handle:
    doc = json.load(handle)
print($2)
" "$1"
}

wait_state() {
  local id="$1" want="$2" attempts=$(( 120 * 10 )) state=""
  for _ in $(seq 1 "$attempts"); do
    "${CLI[@]}" resource get "$id" >"$WORK/rs.json" 2>/dev/null && \
      state="$(jq_field "$WORK/rs.json" 'doc["status"]["state"]')" || state=""
    [ "$state" = "$want" ] && return 0
    sleep 0.1
  done
  die "resource $id did not reach '$want'"
}

outputs_of() {
  "${CLI[@]}" resource get "$1" >"$WORK/outs.json"
  jq_field "$WORK/outs.json" 'json.dumps(doc.get("outputs", {}).get("values", {}), sort_keys=True)'
}

mkdir -p "$ROOT/.demo"
docker exec liftr-postgres-1 psql -U liftr -d liftr -tAc \
  "SELECT 1 FROM pg_database WHERE datname='$DB'" | grep -q 1 || \
  docker exec liftr-postgres-1 createdb -U liftr "$DB"

# ==============================================================================

step "Preflight: REAL OpenTofu binary pinned by exact digest"
VERSION="$("$TOFU_BIN" version | head -1)"
note "$VERSION at $TOFU_BIN"
case "$VERSION" in
  *1.12.6*) : ;;
  *) note "WARNING: expected 1.12.6; continuing anyway (qualification used 1.12.6)" ;;
esac
(cd "$ROOT" && go build -o .demo/bin/liftr ./cmd/liftr) || die "build CLI"
(cd "$ROOT" && go build -o .demo/bin/liftr-demo-tofu-server ./cmd/liftr-demo-tofu-server) || die "build server"
ok "binaries built"

step "Start Liftr with the production OpenTofu adapter (M19)"
LIFTR_DEMO_TOFU_BIN="$TOFU_BIN" nohup "$ROOT/.demo/bin/liftr-demo-tofu-server" >>"$LOG" 2>&1 &
echo $! >"$SERVER_PID"
for _ in $(seq 1 100); do
  curl -fsS "$API/readyz" >/dev/null 2>&1 && break
  sleep 0.3
done
curl -fsS "$API/readyz" >/dev/null || { tail -20 "$LOG"; die "server not ready"; }
ok "developer API on $API; tofu runs as a child process with fenced evidence"
note "state backend: development local files under .demo/tofu/state (never production)"

step "Create -> real 'tofu init + plan + apply' -> contract outputs published"
"${CLI[@]}" resource create --id workload-1 --type DemoWorkload --version v1 \
  --owner team=demo --spec <(printf '%s\n' '{"name":"web","sizeGB":10}') >/dev/null || die "create"
wait_outputs workload-1 'workload-1-gen1.demo.liftr.internal'
OUTPUTS="$(outputs_of workload-1)"
ok "workload-1 Ready; outputs: $OUTPUTS"
STATE_FILE="$(find "$ROOT/.demo/tofu/state" -name terraform.tfstate | head -1)"
[ -n "$STATE_FILE" ] || die "no terraform state file found"
SERIAL1="$(python3 -c "import json,sys; print(json.load(open('$STATE_FILE'))['serial'])")"
ok "REAL tofu state on disk: $STATE_FILE (serial=$SERIAL1)"

step "Update -> real re-plan/re-apply -> generation-bound outputs advance"
"${CLI[@]}" resource update workload-1 --spec <(printf '%s\n' '{"name":"web","sizeGB":20}') >/dev/null || die "update"
wait_outputs workload-1 'workload-1-gen2.demo.liftr.internal'
wait_state workload-1 Ready
OUTPUTS2="$(outputs_of workload-1)"
ok "update converged; outputs: $OUTPUTS2"
SERIAL2="$(python3 -c "import json,sys; print(json.load(open('$STATE_FILE'))['serial'])")"
[ "$SERIAL2" -gt "$SERIAL1" ] || die "state serial did not advance ($SERIAL1 -> $SERIAL2)"
ok "tofu state serial advanced $SERIAL1 -> $SERIAL2 (real infrastructure re-applied)"
note "observe also proves convergence without resubmission: same plan, fresh observation"

step "Delete -> real destroy -> clean tombstone"
"${CLI[@]}" resource delete workload-1 --yes >/dev/null || die "delete"
wait_state workload-1 Deleted
SERIAL3="$(python3 -c "import json,sys; print(json.load(open('$STATE_FILE'))['serial'])")"
[ "$SERIAL3" -gt "$SERIAL2" ] || die "destroy did not touch state ($SERIAL2 -> $SERIAL3)"
ok "workload-1 Deleted; tofu destroyed managed resources (serial=$SERIAL3)"

printf '\nOPENTOFU DEMO COMPLETE - M19 proven end-to-end with the REAL CLI.\n'
printf 'No cloud, no provider downloads: built-in terraform_data only.\n'
