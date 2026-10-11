#!/usr/bin/env bash
# CI tests exercise the real isolation boundary, including approval middleware.
set -euo pipefail

sudo apt-get update
sudo apt-get install -y bubblewrap
if sysctl -n kernel.apparmor_restrict_unprivileged_userns >/dev/null 2>&1; then
  sudo sysctl -w kernel.apparmor_restrict_unprivileged_userns=0
fi
bwrap --die-with-parent --unshare-pid --unshare-ipc --unshare-uts --unshare-net \
  --ro-bind / / --dev /dev --proc /proc --tmpfs /tmp true
