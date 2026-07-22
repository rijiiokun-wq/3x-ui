#!/usr/bin/env bash
set -Eeuo pipefail

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
runner="$root/scripts/mainpanel-deploy/github_remote_transaction.sh"
controller="$root/scripts/mainpanel-deploy/remote_controller.sh"
mock_transport="$root/tests/mainpanel-deploy/mock-mainpanel-transport.sh"
temp=$(mktemp -d "${TMPDIR:-/tmp}/mainpanel-transaction-test.XXXXXX")
trap 'rm -rf "$temp"' EXIT

fail() {
  printf 'github remote transaction test: %s\n' "$1" >&2
  exit 1
}

real_sha256sum=$(command -v sha256sum) || fail 'sha256sum is unavailable'
[[ -x "$real_sha256sum" ]] || fail 'sha256sum is not executable'

assert_file_exact() {
  local expected=$1
  local path=$2
  local actual
  if ! printf '%s\n' "$expected" | cmp -s - "$path"; then
    actual=$(<"$path")
    printf 'expected %s to contain:\n%s\nactual:\n%s\n' "$path" "$expected" "$actual" >&2
    exit 1
  fi
}

mkdir -p "$temp/local-bin" "$temp/remote-bin"
ln -s "$mock_transport" "$temp/local-bin/ssh"
ln -s "$mock_transport" "$temp/local-bin/scp"
ln -s "$mock_transport" "$temp/local-bin/sleep"
for command in install rm chmod sha256sum systemd-run systemctl; do
  ln -s "$mock_transport" "$temp/remote-bin/$command"
done

source_sha='0123456789abcdef0123456789abcdef01234567'
binary_sha='aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa'
release_run_id='123'
artifact_digest="sha256:$binary_sha"
backup_ref='20260723T120000Z-0123456789ab'
safety_ref='20260723T120001Z-abcdefabcdef'
success_result="RESULT status=success operation=deploy source_sha=$source_sha binary_sha=$binary_sha backup_ref=$backup_ref safety_ref=$safety_ref release_run_id=$release_run_id"
rolled_back_result="RESULT status=rolled_back operation=deploy backup_ref=$backup_ref"
failed_result='RESULT status=failed operation=deploy reason=health_check_failed'
remote_stage_rel='opt/3x-ui-deploy/incoming/9001-2'

run_scenario() {
  local scenario=$1
  local case_dir="$temp/$scenario"
  mkdir -p "$case_dir/runner" "$case_dir/remote"
  printf 'mock candidate binary\n' >"$case_dir/x-ui"
  : >"$case_dir/transport.log"

  PATH="$temp/local-bin:$PATH" \
    MAINPANEL_HOST='example.test' \
    MAINPANEL_USER='root' \
    MAINPANEL_SSH_KEY='mock-private-key' \
    MAINPANEL_KNOWN_HOSTS='example.test ssh-ed25519 AAAAC3NzaMock' \
    MAINPANEL_HEALTH_TOKEN='0123456789abcdef0123456789abcdef0123456789abcdef' \
    GITHUB_RUN_ID='9001' \
    GITHUB_RUN_ATTEMPT='2' \
    RUNNER_TEMP="$case_dir/runner" \
    GITHUB_OUTPUT="$case_dir/github-output" \
    MOCK_REMOTE_ROOT="$case_dir/remote" \
    MOCK_REMOTE_LOG="$case_dir/transport.log" \
    MOCK_REMOTE_BIN="$temp/remote-bin" \
    MOCK_REAL_SHA256SUM="$real_sha256sum" \
    MOCK_SCENARIO="$scenario" \
    MOCK_SUCCESS_RESULT="$success_result" \
    MOCK_ROLLED_BACK_RESULT="$rolled_back_result" \
    MOCK_FAILED_RESULT="$failed_result" \
    "$runner" deploy "$controller" "$case_dir/x-ui" \
      "$source_sha" "$binary_sha" "$release_run_id" "$artifact_digest" \
      >"$case_dir/stdout" 2>"$case_dir/stderr"
}

if ! run_scenario success; then
  sed -n '1,120p' "$temp/success/stderr" >&2
  fail 'success scenario failed'
fi
assert_file_exact "$success_result" "$temp/success/stdout"
assert_file_exact "backup_ref=$backup_ref" "$temp/success/github-output"
[[ ! -s "$temp/success/stderr" ]] || fail 'success wrote to stderr'
[[ ! -e "$temp/success/remote/$remote_stage_rel" ]] || fail 'success left remote stage behind'
[[ -z "$(find "$temp/success/runner" -mindepth 1 -print -quit)" ]] || fail 'success left local secret/result files behind'

if run_scenario rolled_back; then
  fail 'rolled_back RESULT was accepted'
fi
grep -qx 'mainpanel-runner: deploy_did_not_succeed' "$temp/rolled_back/stderr" || fail 'rolled_back rejection reason changed'
[[ ! -e "$temp/rolled_back/remote/$remote_stage_rel" ]] || fail 'terminal rolled_back scenario left remote stage behind'
[[ -z "$(find "$temp/rolled_back/runner" -mindepth 1 -print -quit)" ]] || fail 'rolled_back scenario left local secret/result files behind'

if run_scenario failed; then
  fail 'failed RESULT was accepted'
fi
grep -qx 'mainpanel-runner: deploy_did_not_succeed' "$temp/failed/stderr" || fail 'failed RESULT rejection reason changed'
[[ ! -e "$temp/failed/remote/$remote_stage_rel" ]] || fail 'terminal failed scenario left remote stage behind'
[[ -z "$(find "$temp/failed/runner" -mindepth 1 -print -quit)" ]] || fail 'failed scenario left local secret/result files behind'

if run_scenario duplicate_result; then
  fail 'duplicate RESULT records were accepted'
fi
grep -qx 'mainpanel-runner: invalid_result_count' "$temp/duplicate_result/stderr" || fail 'duplicate RESULT rejection reason changed'
[[ -e "$temp/duplicate_result/remote/$remote_stage_rel" ]] || fail 'duplicate RESULT scenario removed stage without a terminal unit state'
[[ -z "$(find "$temp/duplicate_result/runner" -mindepth 1 -print -quit)" ]] || fail 'duplicate RESULT scenario left local secret/result files behind'

if ! run_scenario ambiguous_launch; then
  sed -n '1,120p' "$temp/ambiguous_launch/stderr" >&2
  fail 'ambiguous launch was not reconciled to success'
fi
assert_file_exact "$success_result" "$temp/ambiguous_launch/stdout"
assert_file_exact "backup_ref=$backup_ref" "$temp/ambiguous_launch/github-output"
assert_file_exact 'mainpanel-runner: systemd_run_response_ambiguous_reconciling' "$temp/ambiguous_launch/stderr"
[[ ! -e "$temp/ambiguous_launch/remote/$remote_stage_rel" ]] || fail 'reconciled ambiguous launch left remote stage behind'
[[ -z "$(find "$temp/ambiguous_launch/runner" -mindepth 1 -print -quit)" ]] || fail 'reconciled ambiguous launch left local secret/result files behind'

if run_scenario definite_no_unit; then
  fail 'definite no-unit scenario was accepted'
fi
assert_file_exact $'mainpanel-runner: systemd_run_response_ambiguous_reconciling\nmainpanel-runner: unit_stopped_without_terminal_result' "$temp/definite_no_unit/stderr"
no_unit_stage="$temp/definite_no_unit/remote/$remote_stage_rel"
[[ -d "$no_unit_stage" ]] || fail 'definite no-unit scenario did not preserve remote stage'
if grep -Fq $'rm\t-rf\t--\t'"$no_unit_stage" "$temp/definite_no_unit/transport.log"; then
  fail 'definite no-unit scenario attempted to remove remote stage'
fi
[[ "$(grep -c $'^systemctl\tshow\t' "$temp/definite_no_unit/transport.log")" == '5' ]] || fail 'definite no-unit scenario did not reconcile five unknown states'
[[ "$(grep -c $'^sleep\t4$' "$temp/definite_no_unit/transport.log")" == '4' ]] || fail 'definite no-unit scenario did not use fast mocked polling sleeps'
[[ -z "$(find "$temp/definite_no_unit/runner" -mindepth 1 -print -quit)" ]] || fail 'definite no-unit scenario left local secret/result files behind'

printf 'github remote transaction tests: OK\n'
