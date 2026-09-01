#!/usr/bin/env bash

set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"

version="${1:-0.1.0-beta.1}"
# Optional second argument: comma-separated npm platform ids to build, stage,
# and pack (default: all four). CI passes only the platform it actually
# installs — the other cross-compiled binaries were never used by the smoke —
# while a bare local run still exercises the full staging matrix. The full
# four-platform release path keeps its own staging in release.yml.
platforms="${2:-linux-x64,linux-arm64,darwin-x64,darwin-arm64}"
commit="${COMMIT_SHA:-$(git rev-parse --short=12 HEAD 2>/dev/null || printf 'dev')}"
built_at="${BUILT_AT:-$(date -u +%Y-%m-%dT%H:%M:%SZ)}"
dist_dir="$repo_root/dist/npm-smoke"
stage_dir="$repo_root/.npm-stage"

goos_for_platform() {
  case "$1" in
    linux-*) printf 'linux' ;;
    darwin-*) printf 'darwin' ;;
    *) return 1 ;;
  esac
}

goarch_for_platform() {
  case "$1" in
    *-x64) printf 'amd64' ;;
    *-arm64) printf 'arm64' ;;
    *) return 1 ;;
  esac
}

build_binary() {
  local goos="$1"
  local goarch="$2"
  local output="$3"

  GOWORK=off CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" \
    go build \
      -trimpath \
      -ldflags="-s -w -X selfmind/internal/buildinfo.Version=v${version} -X selfmind/internal/buildinfo.Commit=${commit} -X selfmind/internal/buildinfo.BuiltAt=${built_at}" \
      -o "$output" \
      ./cmd/selfmind
}

stage_args=(--version "$version" --out "$stage_dir")
selected=()
IFS=',' read -r -a requested <<<"$platforms"
for platform in "${requested[@]}"; do
  platform="$(printf '%s' "$platform" | tr -d '[:space:]')"
  [[ -z "$platform" ]] && continue
  if ! goos="$(goos_for_platform "$platform")" || ! goarch="$(goarch_for_platform "$platform")"; then
    printf 'npm smoke: unknown platform id %s (expected linux-x64, linux-arm64, darwin-x64, or darwin-arm64)\n' "$platform" >&2
    exit 2
  fi
  mkdir -p "$dist_dir/$platform"
  build_binary "$goos" "$goarch" "$dist_dir/$platform/selfmind"
  stage_args+=("--$platform" "$dist_dir/$platform/selfmind")
  selected+=("$platform")
done

if [[ "${#selected[@]}" -eq 0 ]]; then
  printf 'npm smoke: no platforms selected\n' >&2
  exit 2
fi

node scripts/stage-npm-packages.mjs "${stage_args[@]}"

pack_dir="$stage_dir/packs"
smoke_dir="$stage_dir/smoke"
rm -rf "$smoke_dir"
mkdir -p "$pack_dir" "$smoke_dir"

pack_package() {
  local package_dir="$1"
  npm pack "$package_dir" --pack-destination "$pack_dir" --silent
}

for platform in "${selected[@]}"; do
  pack_package "$stage_dir/selfmind-$platform"
done
pack_package "$stage_dir/selfmind"

case "$(uname -s)-$(uname -m)" in
  Linux-x86_64)
    host_platform="linux-x64"
    ;;
  Linux-aarch64|Linux-arm64)
    host_platform="linux-arm64"
    ;;
  Darwin-x86_64)
    host_platform="darwin-x64"
    ;;
  Darwin-arm64)
    host_platform="darwin-arm64"
    ;;
  *)
    printf 'npm smoke: unsupported host %s/%s\n' "$(uname -s)" "$(uname -m)" >&2
    exit 1
    ;;
esac

host_selected=0
for platform in "${selected[@]}"; do
  if [[ "$platform" == "$host_platform" ]]; then
    host_selected=1
    break
  fi
done
if [[ "$host_selected" -ne 1 ]]; then
  printf 'npm smoke: host platform %s is not in the selected set (%s); the install smoke needs the host binary\n' \
    "$host_platform" "$platforms" >&2
  exit 2
fi
platform_pattern="selfmind-cli-${host_platform}-*.tgz"

shopt -s nullglob
launcher_candidates=("$pack_dir"/selfmind-cli-[0-9]*.tgz)
platform_candidates=("$pack_dir"/$platform_pattern)
launcher_tgz="${launcher_candidates[0]:-}"
platform_tgz="${platform_candidates[0]:-}"

if [[ -z "$launcher_tgz" || -z "$platform_tgz" ]]; then
  printf 'npm smoke: expected package tarballs were not produced\n' >&2
  exit 1
fi

(
  cd "$smoke_dir"
  npm init --yes --silent >/dev/null
  # Install the explicitly packed host binary while suppressing registry
  # resolution of the launcher's other optional platform packages. This keeps
  # the release smoke deterministic before those sibling packages are public.
  npm install --ignore-scripts --omit=optional --no-audit --no-fund "$platform_tgz" "$launcher_tgz"
  ./node_modules/.bin/selfmind --version
  bash "$repo_root/scripts/smoke-installed-gateway.sh" \
    "./node_modules/.bin/selfmind" "v${version}"
)

printf 'npm smoke passed for selfmind@%s\n' "$version"
