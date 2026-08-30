#!/usr/bin/env bash

set -euo pipefail

release_tag="${TAG:?TAG is required}"
release_sha="${GITHUB_SHA:?GITHUB_SHA is required}"

if ! git cat-file -e "${release_sha}^{commit}" 2>/dev/null; then
  printf 'verified release commit %s is unavailable in the checkout\n' "${release_sha}" >&2
  exit 1
fi

resolve_remote_tag() {
  local resolved
  resolved="$(git ls-remote --tags origin "refs/tags/${release_tag}^{}" | cut -f1)"
  if [[ -z "${resolved}" ]]; then
    resolved="$(git ls-remote --refs --tags origin "refs/tags/${release_tag}" | cut -f1)"
  fi
  printf '%s' "${resolved}"
}

remote_tag="$(resolve_remote_tag)"
if [[ -n "${remote_tag}" ]]; then
  if [[ "${remote_tag}" != "${release_sha}" ]]; then
    printf 'release tag %s points at %s, not verified commit %s\n' \
      "${release_tag}" "${remote_tag}" "${release_sha}" >&2
    exit 1
  fi
  printf 'verified existing %s -> %s\n' "${release_tag}" "${release_sha}"
  exit 0
fi

local_tag="$(git rev-parse -q --verify "refs/tags/${release_tag}^{}" 2>/dev/null || true)"
if [[ -n "${local_tag}" && "${local_tag}" != "${release_sha}" ]]; then
  printf 'local release tag %s points at %s, not verified commit %s\n' \
    "${release_tag}" "${local_tag}" "${release_sha}" >&2
  exit 1
fi
if [[ -z "${local_tag}" ]]; then
  git tag "${release_tag}" "${release_sha}"
fi

if git push origin "refs/tags/${release_tag}"; then
  printf 'created %s -> %s after package verification\n' "${release_tag}" "${release_sha}"
  exit 0
fi

# A concurrent idempotent release may have won the create race.
remote_tag="$(resolve_remote_tag)"
if [[ "${remote_tag}" == "${release_sha}" ]]; then
  printf 'verified concurrently-created %s -> %s\n' "${release_tag}" "${release_sha}"
  exit 0
fi
printf 'failed to create exact release tag %s for %s\n' "${release_tag}" "${release_sha}" >&2
exit 1
