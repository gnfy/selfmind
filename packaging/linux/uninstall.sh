#!/usr/bin/env bash
set -euo pipefail

purge=0

for arg in "$@"; do
  case "${arg}" in
    --purge)
      purge=1
      ;;
    -h|--help)
      echo "Usage: sudo ./uninstall.sh [--purge]"
      exit 0
      ;;
    *)
      echo "Unknown argument: ${arg}" >&2
      exit 2
      ;;
  esac
done

if [[ "${EUID}" -ne 0 ]]; then
  echo "uninstall.sh must be run as root. Try: sudo ./uninstall.sh" >&2
  exit 1
fi

if systemctl list-unit-files selfmind.service >/dev/null 2>&1; then
  systemctl stop selfmind.service >/dev/null 2>&1 || true
  systemctl disable selfmind.service >/dev/null 2>&1 || true
fi

rm -f /etc/systemd/system/selfmind.service
systemctl daemon-reload
rm -f /usr/local/bin/selfmind

if [[ "${purge}" -eq 1 ]]; then
  rm -rf /etc/selfmind /var/lib/selfmind /var/log/selfmind
  userdel selfmind >/dev/null 2>&1 || true
  groupdel selfmind >/dev/null 2>&1 || true
else
  echo "Preserved /etc/selfmind, /var/lib/selfmind, and /var/log/selfmind."
  echo "Run sudo ./uninstall.sh --purge to remove data, config, logs, and the selfmind user."
fi
