#!/usr/bin/env bash

set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"

version="${1:-0.1.0-beta.1}"
commit="${COMMIT_SHA:-$(git rev-parse --short=12 HEAD 2>/dev/null || printf 'dev')}"
built_at="${BUILT_AT:-$(date -u +%Y-%m-%dT%H:%M:%SZ)}"
dist_dir="$repo_root/dist/npm-smoke"
stage_dir="$repo_root/.npm-stage"

mkdir -p \
  "$dist_dir/linux-x64" \
  "$dist_dir/linux-arm64" \
  "$dist_dir/darwin-x64" \
  "$dist_dir/darwin-arm64"

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

build_binary linux amd64 "$dist_dir/linux-x64/selfmind"
build_binary linux arm64 "$dist_dir/linux-arm64/selfmind"
build_binary darwin amd64 "$dist_dir/darwin-x64/selfmind"
build_binary darwin arm64 "$dist_dir/darwin-arm64/selfmind"

node scripts/stage-npm-packages.mjs \
  --version "$version" \
  --linux-x64 "$dist_dir/linux-x64/selfmind" \
  --linux-arm64 "$dist_dir/linux-arm64/selfmind" \
  --darwin-x64 "$dist_dir/darwin-x64/selfmind" \
  --darwin-arm64 "$dist_dir/darwin-arm64/selfmind" \
  --out "$stage_dir"

pack_dir="$stage_dir/packs"
smoke_dir="$stage_dir/smoke"
mkdir -p "$pack_dir" "$smoke_dir"

pack_package() {
  local package_dir="$1"
  npm pack "$package_dir" --pack-destination "$pack_dir" --silent
}

pack_package "$stage_dir/selfmind-linux-x64"
pack_package "$stage_dir/selfmind-linux-arm64"
pack_package "$stage_dir/selfmind-darwin-x64"
pack_package "$stage_dir/selfmind-darwin-arm64"
pack_package "$stage_dir/selfmind"

case "$(uname -s)-$(uname -m)" in
  Linux-x86_64)
    platform_pattern="selfmind-cli-linux-x64-*.tgz"
    ;;
  Linux-aarch64|Linux-arm64)
    platform_pattern="selfmind-cli-linux-arm64-*.tgz"
    ;;
  Darwin-x86_64)
    platform_pattern="selfmind-cli-darwin-x64-*.tgz"
    ;;
  Darwin-arm64)
    platform_pattern="selfmind-cli-darwin-arm64-*.tgz"
    ;;
  *)
    printf 'npm smoke: unsupported host %s/%s\n' "$(uname -s)" "$(uname -m)" >&2
    exit 1
    ;;
esac

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
  npm install --ignore-scripts --silent "$platform_tgz" "$launcher_tgz"
  ./node_modules/.bin/selfmind --version
)

printf 'npm smoke passed for selfmind@%s\n' "$version"
