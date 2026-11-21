#!/usr/bin/env bash
#
# End-to-end test of cpa-quota inside a real CLIProxyAPI v8.
#
# Usage: scripts/e2e_cpa_quota.sh [TARGET ...]
#
#   TARGET   a CLIProxyAPI release version such as 8.0.79 (downloaded from
#            GitHub), or the path of a cli-proxy-api executable.
#            Default: 8.0.79.
#
# PostgreSQL: set CPA_QUOTA_E2E_DSN to a postgres:// URL the suite may create
# a schema in; otherwise a postgres:16 container is started with Docker.
#
# The suite builds the plugin, starts scripts/dummy_upstream.py as a Claude
# API upstream that reports unified quota headers, runs CLIProxyAPI with the
# plugin and sends streaming and non-streaming requests, two of them refused
# for policy reasons. It then checks the rows the plugin wrote and the quota
# API's answers. CPA_E2E_KEEP=1 keeps the run directory.
set -euo pipefail

readonly default_target="8.0.79"
readonly instance="e2e"
readonly api_token="e2e-read-token"
readonly admin_token="e2e-admin-token"
readonly downstream_key="e2e-downstream-key"
readonly model="claude-e2e-model"
export PGOPTIONS="${PGOPTIONS:---client-min-messages=warning}"

for command_name in curl jq go python3 psql; do
  if ! command -v "$command_name" >/dev/null 2>&1; then
    echo "missing command: $command_name" >&2
    exit 1
  fi
done

case "$(uname -s)" in
  Linux) platform="linux"; extension="so" ;;
  Darwin) platform="darwin"; extension="dylib" ;;
  *) echo "unsupported OS: $(uname -s)" >&2; exit 1 ;;
esac
case "$(uname -m)" in
  x86_64|amd64) arch="amd64" ;;
  arm64|aarch64) arch="arm64" ;;
  *) echo "unsupported architecture: $(uname -m)" >&2; exit 1 ;;
esac

script_dir="$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)"
repo_dir="$(CDPATH= cd -- "$script_dir/.." && pwd)"
cache_dir="${CPA_E2E_CACHE_DIR:-${TMPDIR:-/tmp}/cpa-quota-e2e-cache}"
run_dir="$(mktemp -d "${TMPDIR:-/tmp}/cpa-quota-e2e.XXXXXX")"
schema="cpa_quota_e2e_$(date +%s)_$$"
pids=()
container=""
dsn="${CPA_QUOTA_E2E_DSN:-}"

cleanup() {
  local status=$?
  trap - EXIT INT TERM
  for pid in "${pids[@]}"; do
    kill "$pid" >/dev/null 2>&1 || true
    wait "$pid" >/dev/null 2>&1 || true
  done
  if [[ -n "$dsn" && -z "$container" && "${CPA_E2E_KEEP:-0}" != "1" ]]; then
    psql "$dsn" -qAtc "DROP SCHEMA IF EXISTS \"$schema\" CASCADE" >/dev/null 2>&1 || true
  fi
  if [[ -n "$container" ]]; then
    docker rm -f "$container" >/dev/null 2>&1 || true
  fi
  if [[ "${CPA_E2E_KEEP:-0}" == "1" ]]; then
    echo "run directory kept: $run_dir"
  else
    rm -rf "$run_dir"
  fi
  exit "$status"
}
trap cleanup EXIT INT TERM

stage() { printf '\n==> %s\n' "$*"; }
fail() { echo "FAIL: $*" >&2; exit 1; }
free_port() { python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1])'; }
sql() { psql "$dsn" -qAtc "SET search_path TO \"$schema\"; $1" | tail -n 1; }

wait_for() {
  local description="$1" attempts="$2"
  shift 2
  for _ in $(seq "$attempts"); do
    if "$@" >/dev/null 2>&1; then
      return 0
    fi
    sleep 1
  done
  fail "timed out waiting for $description"
}

stage "Building the plugin"
mkdir -p "$run_dir/plugins"
(cd "$repo_dir" && CGO_ENABLED=1 go build -tags cshared -buildmode=c-shared -o "$run_dir/plugins/cpa-quota.$extension" ./cmd/cpa-quota)
rm -f "$run_dir/plugins/cpa-quota.h"

if [[ -z "$dsn" ]]; then
  stage "Starting PostgreSQL"
  command -v docker >/dev/null 2>&1 || fail "set CPA_QUOTA_E2E_DSN or install Docker"
  pg_port="$(free_port)"
  container="$(docker run -d --rm -e POSTGRES_PASSWORD=e2e -p "127.0.0.1:$pg_port:5432" postgres:16)"
  dsn="postgres://postgres:e2e@127.0.0.1:$pg_port/postgres?sslmode=disable"
  wait_for "PostgreSQL" 60 psql "$dsn" -qAtc "SELECT 1"
fi

stage "Starting the dummy upstream"
python3 "$script_dir/dummy_upstream.py" --port 0 >"$run_dir/upstream.port" 2>"$run_dir/upstream.log" &
pids+=($!)
wait_for "the dummy upstream" 20 test -s "$run_dir/upstream.port"
upstream="http://127.0.0.1:$(head -n 1 "$run_dir/upstream.port")"

resolve_target() {
  local target="$1"
  if [[ -x "$target" ]]; then
    echo "$target"
    return
  fi
  local version="${target#v}"
  local dir="$cache_dir/$version"
  if [[ ! -x "$dir/cli-proxy-api" ]]; then
    mkdir -p "$dir"
    curl -fsSL -o "$dir/cpa.tar.gz" \
      "https://github.com/router-for-me/CLIProxyAPI/releases/download/v$version/CLIProxyAPI_${version}_${platform}_${arch}.tar.gz"
    tar -xzf "$dir/cpa.tar.gz" -C "$dir" cli-proxy-api
  fi
  echo "$dir/cli-proxy-api"
}

run_target() {
  local target="$1"
  local binary port base config
  binary="$(resolve_target "$target")"
  port="$(free_port)"
  base="http://127.0.0.1:$port"
  config="$run_dir/config.yaml"
  stage "CLIProxyAPI $("$binary" --help 2>&1 | head -n 1 | sed 's/^CLIProxyAPI //')"
  cat >"$config" <<EOF
config-version: 8
server:
  host: "127.0.0.1"
  port: $port
management:
  allow-remote: false
  secret-key: "e2e-management-key"
  disable-control-panel: true
access:
  api-keys:
    - "$downstream_key"
oauth:
  auth-dir: "$run_dir/auth"
observability:
  logs:
    logging-to-file: false
    debug: ${CPA_E2E_DEBUG:-false}
api-keys:
  claude:
    - name: "dummy-claude"
      base-url: "$upstream"
      models:
        - name: "$model"
      keys:
        - api-key: "dummy-upstream-key-e2e"
          cloak:
            mode: "never"
plugins:
  enabled: true
  dir: "$run_dir/plugins"
  configs:
    cpa-quota:
      enabled: true
      instance: "$instance"
      postgres_dsn: "$dsn"
      postgres_schema: "$schema"
      api_token: "$api_token"
      admin_token: "$admin_token"
      price_url: "$upstream/ratio_config"
      tier: max5x
      # The dummy upstream is reached with an API key credential.
      account_auth_types: [oauth, apikey]
      refresh_interval_seconds: 1
EOF
  "$binary" --config "$config" >"$run_dir/cpa.log" 2>&1 &
  pids+=($!)
  wait_for "CLIProxyAPI" 60 curl -fsS -H "Authorization: Bearer $downstream_key" "$base/v1/models"

  stage "Sending requests"
  local request
  for stream in false false false false false true true true; do
    request="$(jq -nc --arg model "$model" --argjson stream "$stream" \
      '{model:$model,max_tokens:64,stream:$stream,messages:[{role:"user",content:"hello"}]}')"
    curl -fsS -o /dev/null -H "x-api-key: $downstream_key" -H "anthropic-version: 2023-06-01" \
      -H "Content-Type: application/json" -d "$request" "$base/v1/messages" || fail "request failed (stream=$stream)"
  done
  local status
  for stream in false true; do
    request="$(jq -nc --arg model "$model" --argjson stream "$stream" \
      '{model:$model,max_tokens:64,stream:$stream,messages:[{role:"user",content:"E2E REFUSAL please"}]}')"
    status="$(curl -sS -o "$run_dir/refusal.$stream" -w '%{http_code}' -H "x-api-key: $downstream_key" -H "anthropic-version: 2023-06-01" \
      -H "Content-Type: application/json" -d "$request" "$base/v1/messages")"
    if [[ "$stream" == "false" && "$status" != "400" ]]; then
      fail "a refused request answered $status: $(cat "$run_dir/refusal.$stream")"
    fi
  done

  stage "Checking the stored rows"
  wait_for "10 requests in PostgreSQL" 30 test "$(sql "SELECT count(*) FROM requests WHERE instance = '$instance'")" = 10
  [[ "$(sql "SELECT count(*) FROM requests WHERE instance = '$instance' AND NOT failed AND priced AND cost_usd > 0 AND accounting = 'complete'")" == 8 ]] ||
    fail "successful requests are not priced: $(sql "SELECT json_agg(r) FROM requests r")"
  [[ "$(sql "SELECT count(*) FROM requests WHERE instance = '$instance' AND failed AND status_code = 400")" == 2 ]] ||
    fail "refused requests: $(sql "SELECT json_agg(r) FROM requests r WHERE failed")"
  local windows
  windows="$(sql "SELECT count(*) FROM quota_report_windows WHERE instance = '$instance' AND limit_key = 'unified'")"
  (( windows >= 16 )) || fail "unified windows = $windows"
  wait_for "2 safeguard events" 30 test "$(sql "SELECT count(*) FROM safeguard_events WHERE instance = '$instance'")" = 2
  [[ "$(sql "SELECT string_agg(category || '/' || status_code || '/' || stream, ',' ORDER BY stream) FROM safeguard_events")" == "cyber/400/false,cyber/400/true" ]] ||
    fail "safeguard events: $(sql "SELECT json_agg(e) FROM safeguard_events e")"
  [[ "$(sql "SELECT count(*) FROM price_tables WHERE instance = '$instance' AND models = 1")" == 1 ]] || fail "price table not stored"
  sleep 2
  curl -fsS -o /dev/null -H "Authorization: Bearer $api_token" "$base/v0/resource/plugins/cpa-quota/quota"
  wait_for "the estimate row" 30 test "$(sql "SELECT count(*) FROM estimates WHERE instance = '$instance'")" = 1

  stage "Checking the quota API"
  status="$(curl -sS -o /dev/null -w '%{http_code}' "$base/v0/resource/plugins/cpa-quota/quota")"
  [[ "$status" == 401 ]] || fail "quota without a token answered $status"
  status="$(curl -sS -o /dev/null -w '%{http_code}' -H "Authorization: Bearer wrong" "$base/v0/resource/plugins/cpa-quota/quota")"
  [[ "$status" == 401 ]] || fail "quota with a wrong token answered $status"
  local body="$run_dir/quota.json"
  curl -fsS -o "$body" -H "Authorization: Bearer $api_token" "$base/v0/resource/plugins/cpa-quota/quota"
  jq -e --arg instance "$instance" '
    .plugin == "cpa-quota" and .version != "" and .instance == $instance and .database.status == "ok" and
    .price_table.origin == "remote" and .price_table.models == 1 and (has("subscription") | not) and .tier == "max5x" and
    (.accounts | length) == 1 and (.accounts[0] | has("provider") | not) and .accounts[0].state == "cold" and
    .accounts[0].k_source == "default" and .accounts[0].week.total_usd == 1250 and
    .accounts[0].five_hour.used_percent > 0 and .accounts[0].week.remaining_usd > 0' "$body" >/dev/null ||
    fail "quota body: $(cat "$body")"
  status="$(curl -sS -o "$run_dir/prices.json" -w '%{http_code}' -H "Authorization: Bearer $admin_token" "$base/v0/resource/plugins/cpa-quota/admin/refresh-prices")"
  [[ "$status" == 200 ]] || fail "price refresh answered $status: $(cat "$run_dir/prices.json")"
  status="$(curl -sS -o "$run_dir/rebuild.json" -w '%{http_code}' -H "Authorization: Bearer $admin_token" "$base/v0/resource/plugins/cpa-quota/admin/rebuild")"
  [[ "$status" == 200 ]] || fail "rebuild answered $status: $(cat "$run_dir/rebuild.json")"
  jq '{instance, database, price_table: {origin: .price_table.origin, models: .price_table.models},
       account: (.accounts[0] | {auth_index, state, k, k_source,
                                 five_hour: (.five_hour | {used_percent, total_usd, remaining_usd}),
                                 week: (.week | {used_percent, total_usd, remaining_usd})})}' "$body"

  kill "${pids[-1]}" >/dev/null 2>&1 || true
  wait "${pids[-1]}" >/dev/null 2>&1 || true
  unset 'pids[-1]'
  psql "$dsn" -qAtc "DROP SCHEMA IF EXISTS \"$schema\" CASCADE" >/dev/null
}

targets=("$@")
if (( ${#targets[@]} == 0 )); then
  targets=("$default_target")
fi
for target in "${targets[@]}"; do
  run_target "$target"
done
stage "All end-to-end checks passed"
