#!/usr/bin/env bash
set -euo pipefail

start_service=0

for arg in "$@"; do
  case "${arg}" in
    --start)
      start_service=1
      ;;
    -h|--help)
      echo "Usage: sudo ./install.sh [--start]"
      exit 0
      ;;
    *)
      echo "Unknown argument: ${arg}" >&2
      exit 2
      ;;
  esac
done

if [[ "${EUID}" -ne 0 ]]; then
  echo "install.sh must be run as root. Try: sudo ./install.sh" >&2
  exit 1
fi

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
binary_src="${script_dir}/selfmind"
service_src="${script_dir}/selfmind.service"

if [[ ! -x "${binary_src}" ]]; then
  echo "Missing executable: ${binary_src}" >&2
  exit 1
fi

if [[ ! -f "${service_src}" ]]; then
  echo "Missing systemd service file: ${service_src}" >&2
  exit 1
fi

if ! getent group selfmind >/dev/null; then
  groupadd --system selfmind
fi

if ! id -u selfmind >/dev/null 2>&1; then
  nologin_shell="/usr/sbin/nologin"
  if [[ ! -x "${nologin_shell}" && -x /sbin/nologin ]]; then
    nologin_shell="/sbin/nologin"
  fi

  useradd \
    --system \
    --gid selfmind \
    --home-dir /var/lib/selfmind \
    --create-home \
    --shell "${nologin_shell}" \
    selfmind
fi

install -m 0755 "${binary_src}" /usr/local/bin/selfmind
install -d -o selfmind -g selfmind -m 0750 /var/lib/selfmind
install -d -o selfmind -g selfmind -m 0750 /var/log/selfmind
install -d -o root -g selfmind -m 0750 /etc/selfmind

if [[ ! -f /etc/selfmind/selfmind.env ]]; then
  install -o root -g selfmind -m 0640 /dev/null /etc/selfmind/selfmind.env
  cat > /etc/selfmind/selfmind.env <<'EOF'
# SelfMind gateway settings.
# Keep this file readable only by root and the selfmind group.

SELF_TENANT_ID=default
SELF_GATEWAY_ADDR=127.0.0.1:8765

# Recommended when exposing the gateway to anything beyond local trusted tools.
# SELF_GATEWAY_TOKEN=change-me

# Put provider keys here only if you do not use /var/lib/selfmind/.selfmind/config.yaml.
# ANTHROPIC_API_KEY=
# OPENAI_API_KEY=
# GEMINI_API_KEY=
# OPENROUTER_API_KEY=
# MINIMAX_API_KEY=
EOF
fi

install -m 0644 "${service_src}" /etc/systemd/system/selfmind.service
systemctl daemon-reload
systemctl enable selfmind.service >/dev/null

if [[ "${start_service}" -eq 1 ]]; then
  systemctl restart selfmind.service
fi

cat <<'EOF'
SelfMind installed.

Next steps:
  1. Edit /etc/selfmind/selfmind.env if you want gateway env overrides.
  2. Put normal SelfMind config at /var/lib/selfmind/.selfmind/config.yaml.
  3. Start the gateway:
       sudo systemctl start selfmind
  4. Check status and logs:
       systemctl status selfmind
       journalctl -u selfmind -f
EOF
