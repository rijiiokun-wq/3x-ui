#!/usr/bin/env bash
set -Eeuo pipefail
umask 077

die() {
  printf 'mainpanel-preflight: %s\n' "$1" >&2
  exit 1
}

[[ $# -eq 5 ]] || die 'usage_error'
repo=$1
source_sha=$2
run_id=$3
mode=$4
output_dir=$5

[[ "$repo" =~ ^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$ ]] || die 'invalid_repository'
[[ "$source_sha" =~ ^[0-9a-f]{40}$ ]] || die 'invalid_source_sha'
[[ "$run_id" =~ ^[1-9][0-9]*$ ]] || die 'invalid_release_run_id'
[[ "$mode" == 'metadata' || "$mode" == 'download' ]] || die 'invalid_mode'
[[ -n "${GH_TOKEN:-}" ]] || die 'missing_github_token'

mkdir -p "$output_dir"
tmp_dir=$(mktemp -d "${TMPDIR:-/tmp}/mainpanel-preflight.XXXXXX")
trap 'rm -rf "$tmp_dir"' EXIT

gh api "repos/$repo/git/ref/heads/maintenance/v3.3" >"$tmp_dir/ref.json"
branch_sha=$(jq -er '.object.sha' "$tmp_dir/ref.json")
[[ "$branch_sha" == "$source_sha" ]] || die 'source_sha_is_not_current_maintenance_head'

gh api "repos/$repo/actions/runs/$run_id" >"$tmp_dir/run.json"
jq -e \
  --arg sha "$source_sha" \
  '.event == "push" and .head_branch == "maintenance/v3.3" and .head_sha == $sha and
   .status == "completed" and .conclusion == "success" and
   .path == ".github/workflows/release.yml"' \
  "$tmp_dir/run.json" >/dev/null || die 'release_run_provenance_mismatch'

gh api "repos/$repo/actions/runs/$run_id/jobs?per_page=100" >"$tmp_dir/jobs.json"
amd64_successes=$(jq '[.jobs[] | select(.name == "build (amd64)" and .status == "completed" and .conclusion == "success")] | length' "$tmp_dir/jobs.json")
[[ "$amd64_successes" == '1' ]] || die 'amd64_job_not_exactly_one_success'

gh api "repos/$repo/actions/runs/$run_id/artifacts?per_page=100" >"$tmp_dir/artifacts.json"
artifact_count=$(jq '[.artifacts[] | select(.name == "x-ui-linux-amd64" and (.expired | not))] | length' "$tmp_dir/artifacts.json")
[[ "$artifact_count" == '1' ]] || die 'amd64_artifact_not_exactly_one'
artifact_id=$(jq -er '.artifacts[] | select(.name == "x-ui-linux-amd64" and (.expired | not)) | .id' "$tmp_dir/artifacts.json")
artifact_digest=$(jq -er '.artifacts[] | select(.name == "x-ui-linux-amd64" and (.expired | not)) | .digest' "$tmp_dir/artifacts.json")
[[ "$artifact_id" =~ ^[1-9][0-9]*$ ]] || die 'invalid_artifact_id'
[[ "$artifact_digest" =~ ^sha256:[0-9a-f]{64}$ ]] || die 'invalid_artifact_digest'

binary_sha=''
if [[ "$mode" == 'download' ]]; then
  archive="$output_dir/github-artifact.zip"
  candidate="$output_dir/x-ui"
  gh api -H 'Accept: application/vnd.github+json' "repos/$repo/actions/artifacts/$artifact_id/zip" >"$archive"
  python3 scripts/mainpanel-deploy/validate_release_artifact.py \
    --archive "$archive" \
    --expected-digest "$artifact_digest" \
    --output "$candidate" >"$tmp_dir/validation.json"
  binary_sha=$(jq -er 'select(.ok == true) | .binary_sha256' "$tmp_dir/validation.json")
  file "$candidate" | grep -Eq 'ELF 64-bit.*x86-64.*statically linked' || die 'binary_not_static_amd64'
  if readelf -l "$candidate" | grep -q 'INTERP'; then
    die 'binary_has_dynamic_interpreter'
  fi
  version=$("$candidate" -v)
  [[ "$version" == '3.3.0' ]] || die 'binary_version_mismatch'
fi

if [[ -n "${GITHUB_OUTPUT:-}" ]]; then
  {
    printf 'source_sha=%s\n' "$source_sha"
    printf 'release_run_id=%s\n' "$run_id"
    printf 'artifact_id=%s\n' "$artifact_id"
    printf 'artifact_digest=%s\n' "$artifact_digest"
    printf 'binary_sha=%s\n' "$binary_sha"
  } >>"$GITHUB_OUTPUT"
fi

printf 'mainpanel-preflight: ok mode=%s source_sha=%s run_id=%s artifact_id=%s\n' \
  "$mode" "$source_sha" "$run_id" "$artifact_id"
