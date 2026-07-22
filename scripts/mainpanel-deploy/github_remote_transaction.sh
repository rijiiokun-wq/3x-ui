#!/usr/bin/env bash
set -Eeuo pipefail
umask 077

die() {
  printf 'mainpanel-runner: %s\n' "$1" >&2
  exit 1
}

[[ $# -ge 2 ]] || die 'usage_error'
operation=$1
controller_path=$2
shift 2

: "${MAINPANEL_HOST:?}"
: "${MAINPANEL_USER:?}"
: "${MAINPANEL_SSH_KEY:?}"
: "${MAINPANEL_KNOWN_HOSTS:?}"
: "${MAINPANEL_HEALTH_TOKEN:?}"
: "${GITHUB_RUN_ID:?}"
: "${GITHUB_RUN_ATTEMPT:?}"
: "${RUNNER_TEMP:?}"

[[ "$MAINPANEL_HOST" =~ ^[A-Za-z0-9.-]+$ ]] || die 'invalid_host'
[[ "$MAINPANEL_USER" == 'root' ]] || die 'invalid_user'
[[ "$MAINPANEL_HEALTH_TOKEN" =~ ^[A-Za-z0-9]{48}$ ]] || die 'invalid_health_token'
[[ "$GITHUB_RUN_ID" =~ ^[1-9][0-9]*$ ]] || die 'invalid_github_run_id'
[[ "$GITHUB_RUN_ATTEMPT" =~ ^[1-9][0-9]*$ ]] || die 'invalid_github_run_attempt'
[[ -f "$controller_path" && ! -L "$controller_path" ]] || die 'invalid_controller_path'

case "$operation" in
  deploy)
    [[ $# -eq 5 ]] || die 'deploy_usage_error'
    candidate_path=$1
    source_sha=$2
    binary_sha=$3
    release_run_id=$4
    artifact_digest=$5
    [[ -f "$candidate_path" && ! -L "$candidate_path" ]] || die 'invalid_candidate_path'
    [[ "$source_sha" =~ ^[0-9a-f]{40}$ ]] || die 'invalid_source_sha'
    [[ "$binary_sha" =~ ^[0-9a-f]{64}$ ]] || die 'invalid_binary_sha'
    [[ "$release_run_id" =~ ^[1-9][0-9]*$ ]] || die 'invalid_release_run_id'
    [[ "$artifact_digest" =~ ^sha256:[0-9a-f]{64}$ ]] || die 'invalid_artifact_digest'
    ;;
  rollback-binary)
    [[ $# -eq 1 ]] || die 'rollback_usage_error'
    backup_ref=$1
    [[ "$backup_ref" =~ ^[0-9]{8}T[0-9]{6}Z-[0-9a-f]{12}$ ]] || die 'invalid_backup_ref'
    ;;
  *) die 'invalid_operation' ;;
esac

key_file="$RUNNER_TEMP/mainpanel-key"
known_hosts_file="$RUNNER_TEMP/mainpanel-known-hosts"
token_file="$RUNNER_TEMP/mainpanel-health-token"
result_file="$RUNNER_TEMP/mainpanel-transaction-result"
remote_stage="/opt/3x-ui-deploy/incoming/${GITHUB_RUN_ID}-${GITHUB_RUN_ATTEMPT}"
remote_controller="$remote_stage/remote_controller.sh"
remote_token_upload="$remote_stage/.health-token.upload"
remote_token="$remote_stage/health-token"
remote_result="$remote_stage/result"
unit_name="mainpanel-${operation}-${GITHUB_RUN_ID}-${GITHUB_RUN_ATTEMPT}"
unit_service="${unit_name}.service"
target="$MAINPANEL_USER@$MAINPANEL_HOST"
launch_attempted='false'
terminal_result_received='false'
unit_terminal='false'

cleanup_local_secrets() {
  local rc=$?
  trap - EXIT HUP INT TERM
  rm -f -- "$token_file" "$result_file" "$key_file" "$known_hosts_file" || true
  exit "$rc"
}
trap cleanup_local_secrets EXIT
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM

printf '%s\n' "$MAINPANEL_SSH_KEY" >"$key_file"
printf '%s\n' "$MAINPANEL_KNOWN_HOSTS" >"$known_hosts_file"
printf '%s\n' "$MAINPANEL_HEALTH_TOKEN" >"$token_file"
unset MAINPANEL_SSH_KEY MAINPANEL_KNOWN_HOSTS MAINPANEL_HEALTH_TOKEN
chmod 0600 "$key_file" "$known_hosts_file" "$token_file"
[[ -s "$key_file" && -s "$known_hosts_file" && -s "$token_file" ]] || die 'empty_connection_secret'

ssh_opts=(
  -i "$key_file"
  -o BatchMode=yes
  -o IdentitiesOnly=yes
  -o StrictHostKeyChecking=yes
  -o "UserKnownHostsFile=$known_hosts_file"
  -o ConnectTimeout=8
  -o ServerAliveInterval=10
  -o ServerAliveCountMax=3
)

remote_exec() {
  local command=''
  printf -v command '%q ' "$@"
  ssh "${ssh_opts[@]}" "$target" "$command"
}

remote_stage_is_safe_to_remove() {
  if [[ "$launch_attempted" == 'false' ]]; then
    return 0
  fi
  [[ "$terminal_result_received" == 'true' && "$unit_terminal" == 'true' ]]
}

cleanup() {
  local rc=$?
  trap - EXIT HUP INT TERM
  rm -f -- "$token_file" "$result_file"
  if remote_stage_is_safe_to_remove; then
    remote_exec systemctl stop "$unit_service" >/dev/null 2>&1 || true
    remote_exec systemctl reset-failed "$unit_service" >/dev/null 2>&1 || true
    remote_exec rm -rf -- "$remote_stage" >/dev/null 2>&1 || true
  fi
  rm -f -- "$key_file" "$known_hosts_file"
  exit "$rc"
}
trap cleanup EXIT
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM

remote_exec install -d -o root -g root -m 0700 "$remote_stage"
scp "${ssh_opts[@]}" "$controller_path" "$target:$remote_controller"
if [[ "$operation" == 'deploy' ]]; then
  scp "${ssh_opts[@]}" "$candidate_path" "$target:$remote_stage/x-ui"
fi
scp "${ssh_opts[@]}" "$token_file" "$target:$remote_token_upload"
remote_exec install -o root -g root -m 0600 "$remote_token_upload" "$remote_token"
remote_exec rm -f -- "$remote_token_upload"
remote_exec install -o root -g root -m 0600 /dev/null "$remote_result"
remote_exec chmod 0700 "$remote_controller"
if [[ "$operation" == 'deploy' ]]; then
  remote_exec chmod 0700 "$remote_stage/x-ui"
fi

controller_sha=$(sha256sum "$controller_path" | awk '{print $1}')
remote_controller_sha=$(remote_exec sha256sum "$remote_controller" | awk '{print $1}')
[[ "$remote_controller_sha" == "$controller_sha" ]] || die 'remote_controller_hash_mismatch'

unit_command=(
  systemd-run
  --quiet
  --no-block
  "--unit=$unit_name"
  --service-type=oneshot
  --property=User=root
  --property=Group=root
  --property=UMask=0077
  --property=StandardInput=null
  --property=RemainAfterExit=yes
  "--property=StandardOutput=append:$remote_result"
  "--property=StandardError=append:$remote_result"
  --property=KillMode=control-group
  --property=TimeoutStartSec=18min
  --property=RuntimeMaxSec=18min
  --property=TimeoutStopSec=7min
  /bin/bash
  "$remote_controller"
  --token-file
  "$remote_token"
  "$operation"
)
if [[ "$operation" == 'deploy' ]]; then
  unit_command+=("$remote_stage/x-ui" "$source_sha" "$binary_sha" "$release_run_id" "$artifact_digest")
else
  unit_command+=("$backup_ref")
fi

# From this point a lost SSH response is ambiguous: the unit may already be
# running. Cleanup must therefore leave the stage intact unless a terminal
# result and terminal unit state have both been observed.
launch_attempted='true'
set +e
remote_exec "${unit_command[@]}"
launch_rc=$?
set -e
if ((launch_rc != 0)); then
  printf 'mainpanel-runner: systemd_run_response_ambiguous_reconciling\n' >&2
fi

poll_state() {
  remote_exec /bin/bash -c '
    if grep -Eq "^RESULT " "$1"; then
      printf "result-ready\n"
      exit 0
    fi
    state=$(systemctl show "$2" --property=ActiveState --value 2>/dev/null || true)
    [[ -n "$state" ]] || state=unknown
    printf "%s\n" "$state"
  ' bash "$remote_result" "$unit_service"
}

deadline=$((SECONDS + 1560))
terminal_without_result=0
while ((SECONDS < deadline)); do
  set +e
  state=$(poll_state 2>/dev/null)
  poll_rc=$?
  set -e
  if ((poll_rc != 0)); then
    sleep 4
    continue
  fi
  case "$state" in
    result-ready)
      scp "${ssh_opts[@]}" "$target:$remote_result" "$result_file"
      terminal_result_received='true'
      break
      ;;
    active|activating|deactivating|reloading)
      terminal_without_result=0
      ;;
    inactive|failed|unknown)
      ((terminal_without_result += 1))
      if ((terminal_without_result >= 5)); then
        remote_exec rm -f -- "$remote_token" "$remote_token_upload" >/dev/null 2>&1 || true
        die 'unit_stopped_without_terminal_result'
      fi
      ;;
    *) die 'invalid_remote_unit_state' ;;
  esac
  sleep 4
done
[[ "$terminal_result_received" == 'true' ]] || die 'transaction_poll_timeout'

result_count=$(grep -cE '^RESULT ' "$result_file" || true)
[[ "$result_count" == '1' ]] || die 'invalid_result_count'
result_line=$(grep -E '^RESULT ' "$result_file")
grep -E '^(RESULT|mainpanel-controller:)' "$result_file" || true

unit_result=''
unit_exit=''
unit_substate=''
for _ in {1..30}; do
  set +e
  unit_status=$(remote_exec systemctl show "$unit_service" --property=ActiveState --property=SubState --property=Result --property=ExecMainStatus 2>/dev/null)
  status_rc=$?
  set -e
  if ((status_rc == 0)); then
    active_state=$(awk -F= '$1 == "ActiveState" {print $2}' <<<"$unit_status")
    unit_substate=$(awk -F= '$1 == "SubState" {print $2}' <<<"$unit_status")
    unit_result=$(awk -F= '$1 == "Result" {print $2}' <<<"$unit_status")
    unit_exit=$(awk -F= '$1 == "ExecMainStatus" {print $2}' <<<"$unit_status")
    if [[ "$active_state" == 'failed' || "$active_state" == 'inactive' || ("$active_state" == 'active' && "$unit_substate" == 'exited') ]]; then
      unit_terminal='true'
      break
    fi
  fi
  sleep 2
done
[[ "$unit_terminal" == 'true' ]] || die 'unit_not_terminal_after_result'

if [[ "$operation" == 'deploy' ]]; then
  success_pattern="^RESULT status=success operation=deploy source_sha=$source_sha binary_sha=$binary_sha backup_ref=([0-9]{8}T[0-9]{6}Z-[0-9a-f]{12}) safety_ref=([0-9]{8}T[0-9]{6}Z-[0-9a-f]{12}) release_run_id=$release_run_id$"
  [[ "$result_line" =~ $success_pattern ]] || die 'deploy_did_not_succeed'
  [[ "$unit_result" == 'success' && "$unit_exit" == '0' ]] || die 'deploy_unit_failed'
  if [[ -n ${GITHUB_OUTPUT:-} ]]; then
    printf 'backup_ref=%s\n' "${BASH_REMATCH[1]}" >>"$GITHUB_OUTPUT"
  fi
else
  success_pattern="^RESULT status=success operation=rollback-binary binary_sha=[0-9a-f]{64} backup_ref=$backup_ref safety_ref=[0-9]{8}T[0-9]{6}Z-[0-9a-f]{12} online_safety_ref=[0-9]{8}T[0-9]{6}Z-[0-9a-f]{12}$"
  [[ "$result_line" =~ $success_pattern ]] || die 'rollback_did_not_succeed'
  [[ "$unit_result" == 'success' && "$unit_exit" == '0' ]] || die 'rollback_unit_failed'
fi
