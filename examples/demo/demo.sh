#!/usr/bin/env bash
#
# Liftr end-to-end demonstration (M21.5).
#
# Drives the REAL control plane — HTTP API, application orchestration,
# PostgreSQL durability, transactional outbox, worker loop, deterministic demo
# provisioner, operator admin plane — through the dependency-aware lifecycle
# story. Non-production: the demo server registers demo-only ResourceTypes and
# runs insecure development authentication on loopback listeners only.
#
# Prerequisites: make demo-up  (needs bash, curl, python3; no jq).

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
API="http://127.0.0.1:18080"
ADMIN="http://127.0.0.1:18090"
CONTROL="http://127.0.0.1:18099"
CLI=("$ROOT/.demo/bin/liftr" --server "$API" -o json)
WORK="$(mktemp -d /tmp/liftr-demo.XXXXXX)"
trap 'rm -rf "$WORK"' EXIT

STEP_NO=0
step() { STEP_NO=$((STEP_NO + 1)); printf '\n[%d/8] %s\n' "$STEP_NO" "$1"; }
ok()   { printf '  \xe2\x9c\x93 %s\n' "$1"; }
note() { printf '    %s\n' "$1"; }

die() {
  printf '\nDEMO FAILED: %s\n\n' "$1" >&2
  "${CLI[@]}" resource list --include-deleted --limit 100 >&2 2>/dev/null || true
  exit 1
}

command -v curl >/dev/null    2>&1 || die "curl is required"
command -v python3 >/dev/null 2>&1 || die "python3 is required"

# jq_field FILE PYEXPR — evaluate a Python expression over one JSON document.
jq_field() {
  python3 -c "
import json, sys
with open(sys.argv[1]) as handle:
    doc = json.load(handle)
print($2)
" "$1"
}

cli_json() { # cli_json TAG ARGS... — store CLI JSON output under WORK/TAG.json
  local tag="$1"; shift
  "${CLI[@]}" "$@" >"$WORK/$tag.json" || die "liftr $* failed"
}

resource_state() {
  cli_json rs-"$1" resource get "$1"
  jq_field "$WORK/rs-$1.json" 'doc["status"]["state"]'
}

wait_state() { # RESOURCE WANT [SECONDS] — bounded public-API state polling.
  local id="$1" want="$2" seconds="${3:-60}" attempts=$(( (${3:-60}) * 10 )) state=""
  for _ in $(seq 1 "$attempts"); do
    state="$(resource_state "$id" || true)"
    [ "$state" = "$want" ] && return 0
    sleep 0.1
  done
  die "resource $id did not reach '$want' within ${seconds}s (last: ${state:-unknown})"
}

resource_generation() {
  cli_json gen-$1 resource get "$1"
  jq_field "$WORK/gen-$1.json" 'doc["generation"]'
}

wait_update_succeeded() { # RESOURCE NEW_GENERATION — waits for THAT operation.
  local id="$1" want_gen="$2" attempts=$(( 60 * 10 )) gen="" opstate=""
  for _ in $(seq 1 "$attempts"); do
    cli_json wus-$id resource get "$id"
    gen="$(jq_field "$WORK/wus-$id.json" 'doc["latestOperation"]["targetGeneration"]')"
    opstate="$(jq_field "$WORK/wus-$id.json" 'doc["latestOperation"]["state"]')"
    if [ "$gen" = "$want_gen" ] && [ "$opstate" = "Succeeded" ]; then return 0; fi
    sleep 0.1
  done
  die "update of $id to generation $want_gen did not succeed in time (last: gen=$gen state=$opstate)"
}

latest_operation_id() {
  cli_json op-$1 resource get "$1"
  jq_field "$WORK/op-$1.json" 'doc["latestOperation"]["id"]'
}

operation_state() {
  cli_json ops-$1 operation get "$1"
  jq_field "$WORK/ops-$1.json" 'doc["state"]'
}

wait_operation_state() {
  local id="$1" want="$2" seconds="${3:-60}" attempts=$(( (${3:-60}) * 10 )) state=""
  for _ in $(seq 1 "$attempts"); do
    state="$(operation_state "$id" || true)"
    [ "$state" = "$want" ] && return 0
    sleep 0.1
  done
  die "operation $id did not reach '$want' within ${seconds}s (last: ${state:-unknown})"
}

dependency_condition() { # RESOURCE FIELD — DependenciesReady condition field.
  cli_json cond-$1-$2 resource get "$1"
  jq_field "$WORK/cond-$1-$2.json" \
    'next((c["'"$2"'"] for c in doc["status"].get("conditions", []) if c["type"] == "DependenciesReady"), None)'
}

wait_gate_blocked() { # RESOURCE — bounded poll until the dependency gate reports.
  local id="$1" attempts=$(( 60 * 10 )) ready="" reason=""
  for _ in $(seq 1 "$attempts"); do
    ready="$(dependency_condition "$id" status || true)"
    reason="$(dependency_condition "$id" reason || true)"
    if [ "$ready" = "False" ] && [ "$reason" = "WaitingForDependencies" ]; then return 0; fi
    sleep 0.1
  done
  die "resource $id never reported DependenciesReady=False/WaitingForDependencies"
}

admin_get() { curl -fsS "$ADMIN$1"; }

# expect_problem METHOD PATH BODY STATUS CODE LABEL EXTRA_HEADER... verifies an
# EXPECTED refusal through its exact HTTP status and Problem code. Mutations
# require Idempotency-Key, so a deterministic per-call key is always included.
PROBLEM_CALL=0
expect_problem() {
  local method="$1" path="$2" body="$3" status="$4" code="$5" label="$6"
  shift 6
  PROBLEM_CALL=$((PROBLEM_CALL + 1))
  local args=(-sS -o "$WORK/problem.json" -w '%{http_code}' -X "$method" "$API$path")
  args+=(-H "Idempotency-Key: demo-expected-failure-$PROBLEM_CALL")
  [ -n "$body" ] && args+=(-H 'Content-Type: application/json' -d "$body")
  local extra
  for extra in "$@"; do args+=(-H "$extra"); done
  local http_status problem_code
  http_status=$(curl "${args[@]}") || die "$label: request transport failed"
  problem_code=$(jq_field "$WORK/problem.json" 'doc.get("code", "<missing>")')
  if [ "$http_status" != "$status" ] || [ "$problem_code" != "$code" ]; then
    die "$label expected HTTP $status $code, got $http_status $problem_code ($(cat "$WORK/problem.json"))"
  fi
  ok "$label -> HTTP $http_status $problem_code (exactly as designed)"
}

spec_file() { printf '%s\n' "$2" >"$WORK/$1"; }

# ==============================================================================

"${CLI[@]}" version >/dev/null 2>&1 || die "liftr CLI not found; run 'make demo-up' first"

step "Discover ResourceTypes"
note "developers request Liftr Resources - never Pulumi/Terraform/Crossplane primitives."
cli_json types resource-type list
TYPES=$(jq_field "$WORK/types.json" '[i["name"] + "/" + i["version"] for i in doc["items"]]')
echo "  $TYPES"
case "$TYPES" in
  *DemoDatabase/v1*) : ;;
  *) die "demo ResourceTypes missing from discovery: $TYPES" ;;
esac
cli_json appcontract resource-type get DemoApp v1
SLOT=$(jq_field "$WORK/appcontract.json" 'doc["referenceContract"]["slots"][0]["name"]')
TARGET=$(jq_field "$WORK/appcontract.json" 'doc["referenceContract"]["slots"][0]["allowedTargetTypes"][0]["name"]')
[ "$SLOT" = "database" ] && [ "$TARGET" = "DemoDatabase" ] ||
  die "DemoApp reference contract unexpected: slot=$SLOT target=$TARGET"
ok "DemoApp/v1 declares hard-dependency slot 'database' -> DemoDatabase/v1"

step "Create the dependency (held Pending by its own deterministic spec)"
spec_file db-a.json '{"engine":"demo-postgres","sizeGB":20,"hold":true}'
"${CLI[@]}" resource create --id db-a --type DemoDatabase --version v1 \
  --owner team=demo --spec "$WORK/db-a.json" >/dev/null || die "create db-a"
DB_OP=$(latest_operation_id db-a)
wait_operation_state "$DB_OP" Running
STATE=$(resource_state db-a)
[ "$STATE" = "Pending" ] || die "db-a state = $STATE, want Pending while held"
ok "Resource db-a admitted; provider engaged, spec.hold keeps it Pending"
note "$(cli_json dba resource get db-a; jq_field "$WORK/dba.json" '"state=%s generation=%s" % (doc["status"]["state"], doc["generation"])')"

step "Create the dependent (blocked BEFORE any infrastructure submission)"
spec_file app.json '{"image":"demo:v1","hold":false}'
"${CLI[@]}" resource create --id app-1 --type DemoApp --version v1 \
  --owner team=demo --spec "$WORK/app.json" --reference database=db-a >/dev/null || die "create app-1"
APP_CREATE_OP=$(latest_operation_id app-1)
wait_gate_blocked app-1
STATE=$(resource_state app-1)
[ "$STATE" = "Pending" ] || die "app-1 state = $STATE, want Pending while gated"
admin_get "/admin/v1/operations/$APP_CREATE_OP/diagnostics" >"$WORK/diag-app.json"
SUBMITS=$(jq_field "$WORK/diag-app.json" 'doc["attemptCount"]')
[ "$SUBMITS" = "0" ] || die "dependent submitted $SUBMITS time(s) while gated, want 0"
ok "admitted; DependenciesReady=False (WaitingForDependencies); provider submissions so far: $SUBMITS"
note "$(cli_json app1 resource get app-1; jq_field "$WORK/app1.json" '"references=%s conditions=%s" % (doc.get("references"), [(c["type"], c["status"], c["reason"]) for c in doc["status"].get("conditions", [])])')"

step "Release the dependency -> Ready -> WakeDependents -> dependent converges"
note "the simulated backend finishes converging db-a (demo control plane); Liftr observes the new facts"
curl -fsS -X POST "$CONTROL/release/db-a" >/dev/null || die "demo control release of db-a"
wait_state db-a Ready
ok "db-a converged Ready through ordinary observation - no forced success primitive exists or was needed"
wait_state app-1 Ready
ok "app-1 woken, submitted exactly once, converged Ready"
note "wake chain: db-a terminal transition -> WakeDependents work item -> fresh app-1 Drive -> gate passes -> Submit -> Ready"

step "Delete protection (relationships govern lifecycle, not just metadata)"
expect_problem DELETE /v1/resources/db-a "" 409 RESOURCE_IN_USE "DELETE db-a while referenced" \
  "If-Liftr-Generation: $(resource_generation db-a)"

step "Desired vs applied references protect BOTH targets mid-update"
# db-b is created HELD: the app-1 re-point registers its desired reference and
# then waits durably on db-b (zero submissions), leaving both references live.
spec_file db-b.json '{"engine":"demo-postgres","sizeGB":40,"hold":true}'
"${CLI[@]}" resource create --id db-b --type DemoDatabase --version v1 \
  --owner team=demo --spec "$WORK/db-b.json" >/dev/null || die "create db-b"
spec_file app-hold.json '{"image":"demo:v2","hold":false}'
"${CLI[@]}" resource update app-1 --spec "$WORK/app-hold.json" --reference database=db-b >/dev/null ||
  die "update app-1 references to db-b"
APP_UPDATE_OP=$(latest_operation_id app-1)
wait_gate_blocked app-1
curl -fsS "$ADMIN/admin/v1/operations/$APP_UPDATE_OP/diagnostics" >"$WORK/diag-appupd.json"
UPD_SUBMITS=$(jq_field "$WORK/diag-appupd.json" 'doc["attemptCount"]')
[ "$UPD_SUBMITS" = "0" ] || die "gated update submitted $UPD_SUBMITS time(s), want 0"
expect_problem DELETE /v1/resources/db-a "" 409 RESOURCE_IN_USE "DELETE db-a (still applied)" \
  "If-Liftr-Generation: $(resource_generation db-a)"
expect_problem DELETE /v1/resources/db-b "" 409 RESOURCE_IN_USE "DELETE db-b (now desired)" \
  "If-Liftr-Generation: $(resource_generation db-b)"
ok "both anchors protected while the dependent update waits: applied evidence AND desired intent"
curl -fsS -X POST "$CONTROL/release/db-b" >/dev/null || die "demo control release of db-b"
wait_state db-b Ready
wait_state app-1 Ready
ok "db-b converged; wake resumed app-1; protective evidence advanced from db-a to db-b"
expect_problem DELETE /v1/resources/db-b "" 409 RESOURCE_IN_USE "DELETE db-b after convergence (still protected)" \
  "If-Liftr-Generation: $(resource_generation db-b)"
"${CLI[@]}" resource delete db-a --yes >/dev/null || die "delete released db-a"
wait_state db-a Deleted
ok "released db-a deleted normally once nothing references it"
# Idempotent replay: repeating the exact same admitted request is inert.
GENERATION=$(cli_json genapp resource get app-1; jq_field "$WORK/genapp.json" 'doc["generation"]')
REPLAY_BODY='{"spec":{"image":"demo:v2","hold":false}}'
STATUS1=$(curl -sS -o "$WORK/replay1.json" -w '%{http_code}' -X PUT "$API/v1/resources/app-1" \
  -H 'Content-Type: application/json' -H 'Idempotency-Key: demo-replay-key' \
  -H "If-Liftr-Generation: $GENERATION" -d "$REPLAY_BODY")
STATUS2=$(curl -sS -o "$WORK/replay2.json" -D "$WORK/replay2.headers" -w '%{http_code}' -X PUT "$API/v1/resources/app-1" \
  -H 'Content-Type: application/json' -H 'Idempotency-Key: demo-replay-key' \
  -H "If-Liftr-Generation: $GENERATION" -d "$REPLAY_BODY")
REPLAYED=$(grep -i '^Idempotency-Replayed:' "$WORK/replay2.headers" | tr -d '\r' | awk '{print $2}')
[ "$STATUS1" = "202" ] && [ "$STATUS2" = "202" ] && [ "$REPLAYED" = "true" ] ||
  die "idempotent replay failed ($STATUS1/$STATUS2 replayed=${REPLAYED:-unset})"
ok "identical retry with same Idempotency-Key replayed the original result (no duplicate Operation)"

step "Operator diagnostics and safe recovery (admin plane)"
note "conclusive deterministic failure, then explicit M13 retry:"
spec_file fault-fail.json '{"scenario":"failure"}'
"${CLI[@]}" resource create --id fault-fail --type DemoFault --version v1 \
  --owner team=demo --spec "$WORK/fault-fail.json" >/dev/null || die "create fault-fail"
FAIL_OP=$(latest_operation_id fault-fail)
wait_operation_state "$FAIL_OP" Failed
FAIL_REASON=$(cli_json failop operation get "$FAIL_OP"; jq_field "$WORK/failop.json" 'doc["failure"]["reason"]')
[ "$FAIL_REASON" = "DeterministicFailure" ] || die "failure reason = $FAIL_REASON"
ok "fault-fail create failed conclusively (DeterministicFailure)"
"${CLI[@]}" operation retry "$FAIL_OP" >/dev/null || die "retry admission"
RETRY_OP=$(cli_json retryops operation list --resource fault-fail; jq_field "$WORK/retryops.json" 'max(doc["items"], key=lambda i: i["requestedAt"])["id"]')
[ "$RETRY_OP" != "$FAIL_OP" ] || die "retry did not mint a new Operation"
wait_operation_state "$RETRY_OP" Failed
ok "explicit retry admitted a NEW Operation ($RETRY_OP); deterministic backend fails again, history preserved"
note "ambiguous submission resolved by observation, never resubmitted:"
spec_file fault-ambig.json '{"scenario":"ambiguous"}'
"${CLI[@]}" resource create --id fault-ambig --type DemoFault --version v1 \
  --owner team=demo --spec "$WORK/fault-ambig.json" >/dev/null || die "create fault-ambig"
wait_state fault-ambig Ready
AMBIG_OP=$(latest_operation_id fault-ambig)
admin_get "/admin/v1/operations/$AMBIG_OP/diagnostics" >"$WORK/diag-ambig.json"
ATTEMPTS=$(jq_field "$WORK/diag-ambig.json" 'doc["attemptCount"]')
OPSTATE=$(jq_field "$WORK/diag-ambig.json" 'doc["state"]')
[ "$OPSTATE" = "Succeeded" ] && [ "$ATTEMPTS" = "1" ] ||
  die "ambiguous recovery wrong: state=$OPSTATE attempts=$ATTEMPTS, want Succeeded/1"
ok "submission outcome Unknown -> Observe recovered the truth WITHOUT resubmission (attemptCount=$ATTEMPTS)"
FORCE_STATUS=$(curl -sS -o "$WORK/force.json" -w '%{http_code}' -X POST \
  "$ADMIN/admin/v1/operations/$AMBIG_OP/observe" -H 'Idempotency-Key: demo-force-observe' -d '')
FORCE_CODE=$(jq_field "$WORK/force.json" 'doc.get("code", "<none>")')
[ "$FORCE_STATUS" = "409" ] && [ "$FORCE_CODE" = "ACTION_NOT_APPLICABLE" ] ||
  die "observe-on-terminal expected 409 ACTION_NOT_APPLICABLE, got $FORCE_STATUS $FORCE_CODE"
ok "operator Observe on a terminal Operation refused: operators re-evaluate evidence, they cannot force success"
PASSIVE1=$(curl -sS -o "$WORK/passive1.json" -D "$WORK/passive1.h" -w '%{http_code}' -X POST \
  "$ADMIN/admin/v1/resources/app-1/observe" -H 'Idempotency-Key: demo-passive-1' -d '')
PASSIVE2=$(curl -sS -o "$WORK/passive2.json" -D "$WORK/passive2.h" -w '%{http_code}' -X POST \
  "$ADMIN/admin/v1/resources/app-1/observe" -H 'Idempotency-Key: demo-passive-1' -d '')
ACTION1=$(jq_field "$WORK/passive1.json" 'doc["operatorActionId"]')
ACTION2=$(jq_field "$WORK/passive2.json" 'doc["operatorActionId"]')
REPLAYED2=$(grep -i '^Idempotency-Replayed:' "$WORK/passive2.h" | tr -d '\r' | awk '{print $2}')
[ "$PASSIVE1" = "202" ] && [ "$PASSIVE2" = "202" ] || die "passive observe statuses $PASSIVE1/$PASSIVE2"
[ "$ACTION1" = "$ACTION2" ] && [ -n "$ACTION1" ] && [ "$REPLAYED2" = "true" ] ||
  die "passive observe replay mismatch ($ACTION1 vs $ACTION2, replayed=${REPLAYED2:-unset})"
ok "safe passive Observe triggered once; identical Idempotency-Key replayed the SAME audited action"
note "platform policy and quota (M18):"
FAULT_GEN=$(cli_json fgen resource get fault-ambig; jq_field "$WORK/fgen.json" 'doc["generation"]')
spec_file fault-clean.json '{"scenario":"clean"}'
expect_problem PUT /v1/resources/fault-ambig '{"spec":{"scenario":"clean"}}' 403 POLICY_DENIED \
  "UPDATE DemoFault denied" "If-Liftr-Generation: $FAULT_GEN"
QUOTA_BODY='{"id":"quota-bait","type":{"name":"DemoFault","version":"v1"},"owner":{"kind":"team","id":"demo"},"spec":{"scenario":"clean"}}'
expect_problem POST /v1/resources "$QUOTA_BODY" 409 QUOTA_EXCEEDED \
  "fifth live Resource exceeds owner quota of four"

step "Cleanup: references stay protective until Deleted, then everything releases"
# Hold only app-1's destruction so the update completes before the deterministic
# Deleting window opens.
spec_file app-hold-final.json '{"image":"demo:v2","hold":false,"holdDelete":true}'
NEXT_GEN=$(( $(resource_generation app-1) + 1 ))
"${CLI[@]}" resource update app-1 --spec "$WORK/app-hold-final.json" >/dev/null || die "configure app-1 delete hold"
wait_update_succeeded app-1 "$NEXT_GEN"
"${CLI[@]}" resource delete app-1 --yes >/dev/null || die "delete app-1"
expect_problem DELETE /v1/resources/db-b "" 409 RESOURCE_IN_USE \
  "DELETE db-b while dependent still Deleting (protective edge alive)" \
  "If-Liftr-Generation: $(resource_generation db-b)"
curl -fsS -X POST "$CONTROL/release/app-1" >/dev/null || die "release app-1 destruction"
wait_state app-1 Deleted
ok "app-1 reached Deleted; protective edges released atomically"
"${CLI[@]}" resource delete db-b --yes >/dev/null || die "delete db-b"
"${CLI[@]}" resource delete fault-fail --yes >/dev/null || die "delete fault-fail"
"${CLI[@]}" resource delete fault-ambig --yes >/dev/null || die "delete fault-ambig"
wait_state db-b Deleted
wait_state fault-fail Deleted
wait_state fault-ambig Deleted
LIVE=$(cli_json final resource list --limit 100; jq_field "$WORK/final.json" 'len([i for i in doc["items"] if i["status"]["state"] != "Deleted"])')
[ "$LIVE" = "0" ] || die "cleanup left $LIVE live Resource(s)"
ok "final inventory holds only retained tombstones - clean lifecycle end to end"

printf '\nDEMO COMPLETE - every step exercised the real control plane.\n'
printf 'The server keeps running; run make demo-down to stop it and drop the demo database.\n'
