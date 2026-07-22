#!/usr/bin/env bash
set -Eeuo pipefail

[[ ${1:-} == 'api' ]]
endpoint=${2:-}
case "$endpoint" in
  repos/example/repo/git/ref/heads/maintenance/v3.3)
    printf '{"object":{"sha":"%s"}}\n' "${MOCK_BRANCH_SHA:-$MOCK_SHA}"
    ;;
  repos/example/repo/actions/runs/123)
    printf '{"event":"push","head_branch":"maintenance/v3.3","head_sha":"%s","status":"completed","conclusion":"%s","path":".github/workflows/release.yml"}\n' \
      "$MOCK_SHA" "${MOCK_CONCLUSION:-success}"
    ;;
  'repos/example/repo/actions/runs/123/jobs?per_page=100')
    printf '{"jobs":[{"name":"build (amd64)","status":"completed","conclusion":"success"}]}\n'
    ;;
  'repos/example/repo/actions/runs/123/artifacts?per_page=100')
    printf '{"artifacts":[{"id":456,"name":"x-ui-linux-amd64","expired":false,"digest":"sha256:%064d"}]}\n' 0
    ;;
  *)
    printf 'unexpected mock gh endpoint: %s\n' "$endpoint" >&2
    exit 2
    ;;
esac
