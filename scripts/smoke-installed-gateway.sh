#!/usr/bin/env bash

set -euo pipefail

binary_input="${1:-}"
expected_version="${2:-}"
if [[ -z "${binary_input}" || -z "${expected_version}" ]]; then
  echo "usage: smoke-installed-gateway.sh BINARY EXPECTED_VERSION" >&2
  exit 2
fi
if [[ ! -x "${binary_input}" ]]; then
  echo "installed SelfMind binary is not executable: ${binary_input}" >&2
  exit 1
fi

binary_dir="$(cd "$(dirname "${binary_input}")" && pwd)"
binary="${binary_dir}/$(basename "${binary_input}")"
smoke_root="$(mktemp -d "${TMPDIR:-/tmp}/selfmind-gateway-smoke.XXXXXX")"
smoke_home="${smoke_root}/home"
data_dir="${smoke_root}/data"
config_path="${smoke_root}/config.yaml"
auth_path="${smoke_root}/auth.json"
mkdir -p "${smoke_home}" "${data_dir}"

port="$(node -e '
  const net = require("node:net");
  const server = net.createServer();
  server.listen(0, "127.0.0.1", () => {
    process.stdout.write(String(server.address().port));
    server.close();
  });
')"
gateway_addr="127.0.0.1:${port}"
gateway_url="http://${gateway_addr}"
gateway_token="selfmind-release-smoke"

umask 077
printf '%s\n' \
  'providers: {}' \
  'auth:' \
  "  credentials_file: \"${auth_path}\"" \
  'storage:' \
  '  type: "sqlite"' \
  "  data_dir: \"${data_dir}\"" \
  'gateway:' \
  "  addr: \"${gateway_addr}\"" \
  "  url: \"${gateway_url}\"" \
  "  token: \"${gateway_token}\"" \
  'updates:' \
  '  enabled: false' \
  'evolution:' \
  '  enabled: false' \
  > "${config_path}"

run_selfmind() {
  env \
    HOME="${smoke_home}" \
    SELF_CONFIG="${config_path}" \
    SELF_GATEWAY_ADDR="${gateway_addr}" \
    SELF_GATEWAY_URL="${gateway_url}" \
    SELF_GATEWAY_TOKEN="${gateway_token}" \
    "${binary}" --config "${config_path}" "$@"
}

cleanup() {
  run_selfmind gateway stop --force >/dev/null 2>&1 || true
  case "${smoke_root}" in
    "${TMPDIR:-/tmp}"/selfmind-gateway-smoke.*)
      rm -rf "${smoke_root}"
      ;;
  esac
}
trap cleanup EXIT INT TERM

assert_state() {
  local status_json="$1" expected="$2"
  STATUS_JSON="${status_json}" EXPECTED_STATE="${expected}" node -e '
    const value = JSON.parse(process.env.STATUS_JSON || "{}");
    if (value.state !== process.env.EXPECTED_STATE) {
      console.error(`gateway state ${JSON.stringify(value.state)}; expected ${process.env.EXPECTED_STATE}`);
      process.exit(1);
    }
  '
}

wait_for_running() {
  local status_json=""
  for _ in $(seq 1 80); do
    status_json="$(run_selfmind gateway status --json 2>/dev/null || true)"
    if STATUS_JSON="${status_json}" node -e '
      const value = JSON.parse(process.env.STATUS_JSON || "{}");
      process.exit(value.state === "running" && Number(value.runtime?.pid) > 0 ? 0 : 1);
    ' 2>/dev/null; then
      printf '%s' "${status_json}"
      return 0
    fi
    sleep 0.25
  done
  echo "installed gateway did not become healthy at ${gateway_url}" >&2
  if [[ -f "${data_dir}/gateway/gateway.log" ]]; then
    tail -n 80 "${data_dir}/gateway/gateway.log" >&2
  fi
  return 1
}

version_output="$(env HOME="${smoke_home}" "${binary}" --version)"
grep -F "${expected_version}" <<<"${version_output}" >/dev/null

run_selfmind gateway start
first_status="$(wait_for_running)"
first_pid="$(STATUS_JSON="${first_status}" node -e 'process.stdout.write(String(JSON.parse(process.env.STATUS_JSON).runtime.pid))')"
if [[ ! -s "${data_dir}/control.db" ]]; then
  echo "installed gateway did not create its isolated control.db" >&2
  exit 1
fi

# Exercise the authenticated short-lived client path against durable state.
# These model-free commands prove more than process liveness without spending a
# provider call or depending on external network access.
control_status="$(run_selfmind status)"
grep -F "No active task." <<<"${control_status}" >/dev/null
tasks_before_restart="$(run_selfmind tasks)"
grep -F "No open tasks." <<<"${tasks_before_restart}" >/dev/null

run_selfmind gateway restart --drain
second_status="$(wait_for_running)"
second_pid="$(STATUS_JSON="${second_status}" node -e 'process.stdout.write(String(JSON.parse(process.env.STATUS_JSON).runtime.pid))')"
if [[ "${first_pid}" == "${second_pid}" ]]; then
  echo "gateway restart reused pid ${first_pid}; expected a new daemon process" >&2
  exit 1
fi
tasks_after_restart="$(run_selfmind tasks)"
grep -F "No open tasks." <<<"${tasks_after_restart}" >/dev/null

run_selfmind gateway stop
stopped_status="$(run_selfmind gateway status --json)"
assert_state "${stopped_status}" "stopped"

echo "installed gateway lifecycle smoke passed (${first_pid} -> ${second_pid})"
