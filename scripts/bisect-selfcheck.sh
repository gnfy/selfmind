#!/usr/bin/env bash
set -u

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
MODE="${1:-fast}"
PROFILE="local-fast"
if [[ "$MODE" == "full" ]]; then
  PROFILE="local-full"
elif [[ "$MODE" != "fast" ]]; then
  printf 'usage: %s [fast|full]\n' "$0" >&2
  exit 125
fi

TMP_ROOT="$(mktemp -d)"
cleanup() {
  rm -rf "$TMP_ROOT"
}
trap cleanup EXIT

export HOME="$TMP_ROOT/home"
export SELFMIND_HOME="$HOME/.selfmind"
export GOWORK=off
mkdir -p "$HOME" "$SELFMIND_HOME"

if [[ -n "${SELFMIND_BISECT_PATH:-}" ]]; then
  export PATH="$SELFMIND_BISECT_PATH"
fi

cd "$ROOT" || exit 125
if ! go build -o "$TMP_ROOT/selfmind" ./cmd/selfmind; then
  exit 125
fi

"$TMP_ROOT/selfmind" selfcheck --profile "$PROFILE"
status=$?
case "$status" in
  0) exit 0 ;;
  1) exit 1 ;;
  2) exit 125 ;;
  *) exit 125 ;;
esac
