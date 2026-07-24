#!/usr/bin/env bash

set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"

version="${1:-0.1.0-beta.1}"
commit="${COMMIT_SHA:-$(git rev-parse --short=12 HEAD 2>/dev/null || printf 'dev')}"
built_at="${BUILT_AT:-$(date -u +%Y-%m-%dT%H:%M:%SZ)}"
dist_dir="$repo_root/dist/npm-smoke"
stage_dir="$repo_root/.npm-stage"

mkdir -p "$dist_dir/linux-x64" "$dist_dir/linux-arm64"

build_binary() {
  local goarch="$1"
  local output="$2"

  GOWORK=off CGO_ENABLED=0 GOOS=linux GOARCH="$goarch" \
    go build \
      -trimpath \
      -ldflags="-s -w -X selfmind/internal/buildinfo.Version=v${version} -X selfmind/internal/buildinfo.Commit=${commit} -X selfmind/internal/buildinfo.BuiltAt=${built_at}" \
      -o "$output" \
      ./cmd/selfmind
}

build_binary amd64 "$dist_dir/linux-x64/selfmind"
build_binary arm64 "$dist_dir/linux-arm64/selfmind"

node scripts/stage-npm-packages.mjs \
  --version "$version" \
  --linux-x64 "$dist_dir/linux-x64/selfmind" \
  --linux-arm64 "$dist_dir/linux-arm64/selfmind" \
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
pack_package "$stage_dir/selfmind"

launcher_tgz="$(find "$pack_dir" -maxdepth 1 -type f -name 'selfmind-[0-9]*.tgz' -print -quit)"
platform_tgz="$(find "$pack_dir" -maxdepth 1 -type f -name 'selfmind-linux-x64-*.tgz' -print -quit)"

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
