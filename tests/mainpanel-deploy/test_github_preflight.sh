#!/usr/bin/env bash
set -Eeuo pipefail

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
temp=$(mktemp -d "${TMPDIR:-/tmp}/mainpanel-preflight-test.XXXXXX")
trap 'rm -rf "$temp"' EXIT
mkdir "$temp/bin"
ln -s "$root/tests/mainpanel-deploy/mock-gh.sh" "$temp/bin/gh"
sha='0123456789abcdef0123456789abcdef01234567'

PATH="$temp/bin:$PATH" \
  GH_TOKEN='test-token' \
  MOCK_SHA="$sha" \
  GITHUB_OUTPUT="$temp/output" \
  "$root/scripts/mainpanel-deploy/github_release_preflight.sh" \
  example/repo "$sha" 123 metadata "$temp/release"

grep -qx "source_sha=$sha" "$temp/output"
grep -qx 'release_run_id=123' "$temp/output"
grep -qx 'artifact_id=456' "$temp/output"

if PATH="$temp/bin:$PATH" GH_TOKEN='test-token' MOCK_SHA="$sha" MOCK_CONCLUSION='failure' \
  "$root/scripts/mainpanel-deploy/github_release_preflight.sh" \
  example/repo "$sha" 123 metadata "$temp/rejected" >/dev/null 2>&1; then
  printf 'failed release run was accepted\n' >&2
  exit 1
fi

if PATH="$temp/bin:$PATH" GH_TOKEN='test-token' MOCK_SHA="$sha" MOCK_BRANCH_SHA='aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa' \
  "$root/scripts/mainpanel-deploy/github_release_preflight.sh" \
  example/repo "$sha" 123 metadata "$temp/stale" >/dev/null 2>&1; then
  printf 'stale source SHA was accepted\n' >&2
  exit 1
fi

printf 'github preflight tests: OK\n'
