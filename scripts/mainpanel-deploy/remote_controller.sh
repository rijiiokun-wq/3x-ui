#!/usr/bin/env bash
set -Eeuo pipefail
umask 077

readonly service_name='x-ui'
readonly binary_path='/usr/local/x-ui/x-ui'
readonly database_path='/etc/x-ui/x-ui.db'
readonly deploy_root='/opt/3x-ui-deploy'
readonly backups_root="$deploy_root/backups"
readonly state_root="$deploy_root/state"
readonly lock_path='/var/lock/mainpanel-github-deploy.lock'
readonly required_version='3.3.0'
created_backup_ref=''
rollback_armed='false'
rollback_operation='unknown'
recovery_backup_ref=''
recovery_secondary_backup_ref=''
recovery_candidate_sha=''
recovery_source_sha=''
recovery_release_run_id=''
recovery_artifact_digest=''
health_token=''
token_file=''
result_emitted='false'
failure_reason='unexpected_failure'
active_health_dir=''

die() {
  failure_reason=$1
  printf 'mainpanel-controller: %s\n' "$1" >&2
  exit 1
}

emit_result() {
  [[ "$result_emitted" == 'false' ]] || return 1
  result_emitted='true'
  printf '%s\n' "$1"
}

valid_sha() {
  [[ "$1" =~ ^[0-9a-f]{64}$ ]]
}

valid_source_sha() {
  [[ "$1" =~ ^[0-9a-f]{40}$ ]]
}

valid_run_id() {
  [[ "$1" =~ ^[1-9][0-9]*$ ]]
}

valid_backup_ref() {
  [[ "$1" =~ ^[0-9]{8}T[0-9]{6}Z-[0-9a-f]{12}$ ]]
}

binary_version() {
  "$1" -v 2>/dev/null | tr -d '\r\n'
}

schema_sha() {
  sqlite_machine "$database_path" '.schema' | sha256sum | awk '{print $1}'
}

user_version() {
  sqlite_machine "$database_path" 'PRAGMA user_version;'
}

setting_value() {
  local key=$1
  sqlite_machine "$database_path" "SELECT value FROM settings WHERE key = '$key' LIMIT 1;"
}

sqlite_machine() {
  sqlite3 -batch -noheader -init /dev/null "$@"
}

cleanup_health_scratch() {
  if [[ -n "$active_health_dir" ]]; then
    rm -rf -- "$active_health_dir" || return 1
    active_health_dir=''
  fi
}

write_state() {
  local status=$1 operation=$2 source_sha=$3 binary_sha=$4 backup_ref=$5 run_id=$6 artifact_digest=$7
  local temp="$state_root/current.json.tmp.$$"
  install -d -m 0700 "$state_root"
  python3 - "$temp" "$status" "$operation" "$source_sha" "$binary_sha" "$backup_ref" "$run_id" "$artifact_digest" "$(date -u +%FT%TZ)" <<'PY'
import json
import pathlib
import sys

target = pathlib.Path(sys.argv[1])
keys = ("status", "operation", "source_sha", "binary_sha256", "backup_ref", "release_run_id", "artifact_digest", "deployed_at")
payload = {"schema": 1, **dict(zip(keys, sys.argv[2:], strict=True))}
target.write_text(json.dumps(payload, separators=(",", ":")) + "\n")
PY
  chmod 0600 "$temp"
  sync -f "$temp"
  mv -f "$temp" "$state_root/current.json"
  sync -f "$state_root"
}

atomic_install_binary() {
  local source=$1 expected_sha=$2
  local temp="${binary_path}.new.$$"
  install -o root -g root -m 0755 "$source" "$temp" || return 1
  local actual
  actual=$(sha256sum "$temp" | awk '{print $1}') || return 1
  [[ "$actual" == "$expected_sha" ]] || {
    rm -f "$temp"
    return 1
  }
  sync -f "$temp" || return 1
  mv -f "$temp" "$binary_path" || return 1
  sync -f "$(dirname "$binary_path")" || return 1
}

create_backup() {
  local old_sha old_version timestamp backup_ref backup_dir temp_dir db_backup created_at attempt
  created_backup_ref=''
  old_sha=$(sha256sum "$binary_path" | awk '{print $1}') || return 1
  valid_sha "$old_sha" || return 1
  old_version=$(binary_version "$binary_path") || return 1
  [[ "$old_version" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || return 1
  install -d -o root -g root -m 0700 "$backups_root" || return 1
  for ((attempt = 1; attempt <= 5; attempt++)); do
    timestamp=$(date -u +%Y%m%dT%H%M%SZ) || return 1
    backup_ref="${timestamp}-${old_sha:0:12}"
    backup_dir="$backups_root/$backup_ref"
    temp_dir="$backups_root/.${backup_ref}.tmp.$$"
    if [[ ! -e "$backup_dir" && ! -e "$temp_dir" ]] && mkdir -m 0700 "$temp_dir"; then
      break
    fi
    temp_dir=''
    sleep 1
  done
  [[ -n "$temp_dir" && -d "$temp_dir" ]] || return 1
  db_backup="$temp_dir/x-ui.db"
  created_at=$(date -u +%FT%TZ) || {
    rm -rf -- "$temp_dir"
    return 1
  }
  install -o root -g root -m 0755 "$binary_path" "$temp_dir/x-ui" || {
    rm -rf -- "$temp_dir"
    return 1
  }
  sqlite_machine "$database_path" ".timeout 10000" ".backup '$db_backup'" || {
    rm -rf -- "$temp_dir"
    return 1
  }
  chmod 0600 "$db_backup" || {
    rm -rf -- "$temp_dir"
    return 1
  }
  local quick_check backup_schema backup_user
  quick_check=$(sqlite_machine "$db_backup" 'PRAGMA quick_check;') || {
    rm -rf -- "$temp_dir"
    return 1
  }
  [[ "$quick_check" == 'ok' ]] || {
    rm -rf -- "$temp_dir"
    return 1
  }
  backup_schema=$(sqlite_machine "$db_backup" '.schema' | sha256sum | awk '{print $1}') || {
    rm -rf -- "$temp_dir"
    return 1
  }
  backup_user=$(sqlite_machine "$db_backup" 'PRAGMA user_version;') || {
    rm -rf -- "$temp_dir"
    return 1
  }
  {
    printf 'binary_sha=%s\n' "$old_sha"
    printf 'binary_version=%s\n' "$old_version"
    printf 'db_schema_sha=%s\n' "$backup_schema"
    printf 'db_user_version=%s\n' "$backup_user"
    printf 'created_at=%s\n' "$created_at"
  } >"$temp_dir/metadata" || {
    rm -rf -- "$temp_dir"
    return 1
  }
  chmod 0600 "$temp_dir/metadata" || {
    rm -rf -- "$temp_dir"
    return 1
  }
  sync -f "$temp_dir/x-ui" || {
    rm -rf -- "$temp_dir"
    return 1
  }
  sync -f "$db_backup" || return 1
  sync -f "$temp_dir/metadata" || return 1
  sync -f "$temp_dir" || return 1
  mv -f "$temp_dir" "$backup_dir" || return 1
  sync -f "$backups_root" || return 1
  created_backup_ref=$backup_ref
}

metadata_value() {
  local metadata=$1 key=$2
  awk -F= -v key="$key" '$1 == key {sub(/^[^=]*=/, ""); print; exit}' "$metadata"
}

restore_database_if_schema_changed() {
  local backup_dir metadata
  backup_dir=$1
  metadata="$backup_dir/metadata"
  local before_schema before_user current_schema current_user
  before_schema=$(metadata_value "$metadata" 'db_schema_sha') || return 1
  before_user=$(metadata_value "$metadata" 'db_user_version') || return 1
  current_schema=$(schema_sha) || return 1
  current_user=$(user_version) || return 1
  if [[ "$current_schema" == "$before_schema" && "$current_user" == "$before_user" ]]; then
    return 0
  fi

  restore_database_from_backup "$backup_dir" || return 1
}

restore_database_from_backup() {
  local backup_dir=$1
  local temp="${database_path}.rollback.$$"
  install -o root -g root -m 0600 "$backup_dir/x-ui.db" "$temp" || return 1
  local quick_check
  quick_check=$(sqlite_machine "$temp" 'PRAGMA quick_check;') || return 1
  [[ "$quick_check" == 'ok' ]] || return 1
  rm -f "${database_path}-wal" "${database_path}-shm" || return 1
  sync -f "$temp" || return 1
  mv -f "$temp" "$database_path" || return 1
  sync -f "$(dirname "$database_path")" || return 1
}

health_check() {
  local expected_sha=$1 expected_version=$2 health_token=$3 require_repair_route=$4
  local port base_path cert_file key_file scheme curl_tls url_root temp_dir auth_config
  port=$(setting_value 'webPort')
  base_path=$(setting_value 'webBasePath')
  cert_file=$(setting_value 'webCertFile')
  key_file=$(setting_value 'webKeyFile')
  [[ "$port" =~ ^[1-9][0-9]{0,4}$ ]] || return 1
  ((port <= 65535)) || return 1
  [[ "$base_path" == /* ]] || base_path="/$base_path"
  [[ "$base_path" == */ ]] || base_path="$base_path/"
  [[ "$base_path" != *'..'* && "$base_path" != *$'\n'* && "$base_path" != *$'\r'* ]] || return 1
  scheme='http'
  curl_tls=()
  if [[ -n "$cert_file" && -n "$key_file" ]]; then
    scheme='https'
    # The panel's existing certificate is self-signed. This check is made only
    # through loopback after SSH host verification and binary/hash validation.
    curl_tls=(--insecure)
  fi
  url_root="${scheme}://127.0.0.1:${port}${base_path}"
  cleanup_health_scratch || return 1
  temp_dir=$(mktemp -d '/run/mainpanel-deploy-health.XXXXXX') || return 1
  active_health_dir=$temp_dir
  auth_config="$temp_dir/auth.conf"
  printf 'header = "Authorization: Bearer %s"\n' "$health_token" >"$auth_config"
  chmod 0600 "$auth_config"

  local attempt current_sha current_version
  for ((attempt = 1; attempt <= 30; attempt++)); do
    if ! systemctl is-active --quiet "$service_name"; then
      sleep 1
      continue
    fi
    current_sha=$(sha256sum "$binary_path" | awk '{print $1}')
    current_version=$(binary_version "$binary_path")
    if [[ "$current_sha" != "$expected_sha" || "$current_version" != "$expected_version" ]]; then
      sleep 1
      continue
    fi
    if ! curl -fsS "${curl_tls[@]}" --max-time 5 "${url_root}panel/api/openapi.json" -o "$temp_dir/openapi.json"; then
      sleep 1
      continue
    fi
    if ! curl -fsS "${curl_tls[@]}" --max-time 5 --config "$auth_config" "${url_root}panel/api/server/status" -o "$temp_dir/status.json"; then
      sleep 1
      continue
    fi
    if python3 - "$temp_dir/openapi.json" "$temp_dir/status.json" "$expected_version" "$require_repair_route" <<'PY'
import json
import pathlib
import sys

openapi = json.loads(pathlib.Path(sys.argv[1]).read_text())
status = json.loads(pathlib.Path(sys.argv[2]).read_text())
expected_version = sys.argv[3]
require_route = sys.argv[4] == "true"
route = "/panel/api/inbounds/{id}/repairClientTrafficCycles"
route_ok = not require_route or route in openapi.get("paths", {})
obj = status.get("obj") if isinstance(status, dict) else None
xray = obj.get("xray") if isinstance(obj, dict) else None
ok = (
    route_ok
    and status.get("success") is True
    and isinstance(xray, dict)
    and xray.get("state") == "running"
    and obj.get("panelVersion") == expected_version
)
raise SystemExit(0 if ok else 1)
PY
    then
      cleanup_health_scratch || return 1
      return 0
    fi
    sleep 1
  done
  cleanup_health_scratch || return 1
  return 1
}

restore_binary_and_health() {
  local backup_ref=$1 health_token=$2
  local backup_dir metadata
  backup_dir="$backups_root/$backup_ref"
  metadata="$backup_dir/metadata"
  local old_sha old_version
  old_sha=$(metadata_value "$metadata" 'binary_sha') || return 1
  old_version=$(metadata_value "$metadata" 'binary_version') || return 1
  valid_sha "$old_sha" || return 1
  systemctl stop "$service_name" || return 1
  atomic_install_binary "$backup_dir/x-ui" "$old_sha" || return 1
  restore_database_if_schema_changed "$backup_dir" || return 1
  if ! systemctl start "$service_name" || ! systemctl is-active --quiet "$service_name"; then
    systemctl stop "$service_name" || return 1
    restore_database_from_backup "$backup_dir" || return 1
    systemctl start "$service_name" || return 1
  fi
  health_check "$old_sha" "$old_version" "$health_token" 'true' || return 1
}

best_effort_start_known_binary() {
  local current_sha expected_sha ref
  local known='false'
  [[ -x "$binary_path" ]] || return 1
  current_sha=$(sha256sum "$binary_path" | awk '{print $1}') || return 1
  valid_sha "$current_sha" || return 1
  if valid_sha "$recovery_candidate_sha" && [[ "$current_sha" == "$recovery_candidate_sha" ]]; then
    known='true'
  fi
  for ref in "$recovery_backup_ref" "$recovery_secondary_backup_ref"; do
    if valid_backup_ref "$ref" && [[ -f "$backups_root/$ref/metadata" ]]; then
      expected_sha=$(metadata_value "$backups_root/$ref/metadata" 'binary_sha') || expected_sha=''
      if valid_sha "$expected_sha" && [[ "$current_sha" == "$expected_sha" ]]; then
        known='true'
      fi
    fi
  done
  [[ "$known" == 'true' ]] || return 1
  systemctl start "$service_name" || return 1
  systemctl is-active --quiet "$service_name" || return 1
}

deploy() {
  [[ $# -eq 5 ]] || die 'deploy_usage_error'
  local staged_binary=$1 source_sha=$2 expected_sha=$3 release_run_id=$4 artifact_digest=$5
  valid_source_sha "$source_sha" || die 'invalid_source_sha'
  valid_sha "$expected_sha" || die 'invalid_binary_sha'
  valid_run_id "$release_run_id" || die 'invalid_release_run_id'
  [[ "$artifact_digest" =~ ^sha256:[0-9a-f]{64}$ ]] || die 'invalid_artifact_digest'
  [[ -f "$staged_binary" && ! -L "$staged_binary" ]] || die 'invalid_staged_binary'
  [[ "$(sha256sum "$staged_binary" | awk '{print $1}')" == "$expected_sha" ]] || die 'staged_binary_hash_mismatch'
  [[ "$(binary_version "$staged_binary")" == "$required_version" ]] || die 'staged_binary_version_mismatch'

  local backup_ref online_backup_ref
  create_backup || die 'backup_failed'
  backup_ref=$created_backup_ref
  valid_backup_ref "$backup_ref" || die 'invalid_backup_ref'
  online_backup_ref=$backup_ref

  rollback_operation='deploy'
  recovery_backup_ref=$backup_ref
  recovery_secondary_backup_ref=''
  recovery_candidate_sha=$expected_sha
  recovery_source_sha=$source_sha
  recovery_release_run_id=$release_run_id
  recovery_artifact_digest=$artifact_digest
  rollback_armed='true'

  systemctl stop "$service_name"
  create_backup || die 'stopped_backup_failed'
  backup_ref=$created_backup_ref
  valid_backup_ref "$backup_ref" || die 'invalid_stopped_backup_ref'
  recovery_secondary_backup_ref=$online_backup_ref
  recovery_backup_ref=$backup_ref
  atomic_install_binary "$staged_binary" "$expected_sha"
  systemctl start "$service_name"
  health_check "$expected_sha" "$required_version" "$health_token" 'true'
  write_state 'deployed' 'deploy' "$source_sha" "$expected_sha" "$backup_ref" "$release_run_id" "$artifact_digest"
  rollback_armed='false'
  emit_result "RESULT status=success operation=deploy source_sha=$source_sha binary_sha=$expected_sha backup_ref=$backup_ref safety_ref=$online_backup_ref release_run_id=$release_run_id"
}

rollback_binary() {
  [[ $# -eq 1 ]] || die 'rollback_usage_error'
  local target_ref=$1
  valid_backup_ref "$target_ref" || die 'invalid_backup_ref'
  local target_dir metadata
  target_dir="$backups_root/$target_ref"
  metadata="$target_dir/metadata"
  [[ -d "$target_dir" && -f "$target_dir/x-ui" && -f "$metadata" && ! -L "$target_dir/x-ui" ]] || die 'backup_not_found'
  local target_sha target_version safety_ref online_safety_ref
  target_sha=$(metadata_value "$metadata" 'binary_sha')
  target_version=$(metadata_value "$metadata" 'binary_version')
  valid_sha "$target_sha" || die 'invalid_backup_binary_sha'
  [[ "$(sha256sum "$target_dir/x-ui" | awk '{print $1}')" == "$target_sha" ]] || die 'backup_binary_hash_mismatch'
  [[ "$(binary_version "$target_dir/x-ui")" == "$target_version" ]] || die 'backup_binary_version_mismatch'
  create_backup || die 'rollback_safety_backup_failed'
  safety_ref=$created_backup_ref
  online_safety_ref=$safety_ref

  rollback_operation='rollback-binary'
  recovery_backup_ref=$safety_ref
  recovery_secondary_backup_ref=''
  recovery_candidate_sha=$target_sha
  recovery_source_sha=''
  recovery_release_run_id=''
  recovery_artifact_digest=''
  rollback_armed='true'

  systemctl stop "$service_name"
  create_backup || die 'rollback_stopped_safety_backup_failed'
  safety_ref=$created_backup_ref
  valid_backup_ref "$safety_ref" || die 'invalid_rollback_safety_ref'
  recovery_secondary_backup_ref=$online_safety_ref
  recovery_backup_ref=$safety_ref
  atomic_install_binary "$target_dir/x-ui" "$target_sha"
  systemctl start "$service_name"
  health_check "$target_sha" "$target_version" "$health_token" 'true'
  write_state 'rolled_back_manually' 'rollback-binary' '' "$target_sha" "$target_ref" '' ''
  rollback_armed='false'
  emit_result "RESULT status=success operation=rollback-binary binary_sha=$target_sha backup_ref=$target_ref safety_ref=$safety_ref online_safety_ref=$online_safety_ref"
}

on_exit() {
  local rc=$?
  trap - EXIT ERR HUP INT TERM
  cleanup_health_scratch || true
  if [[ "$rollback_armed" == 'true' ]]; then
    rollback_armed='false'
    local restored_ref=''
    if restore_binary_and_health "$recovery_backup_ref" "$health_token"; then
      restored_ref=$recovery_backup_ref
    elif valid_backup_ref "$recovery_secondary_backup_ref" && [[ "$recovery_secondary_backup_ref" != "$recovery_backup_ref" ]] && restore_binary_and_health "$recovery_secondary_backup_ref" "$health_token"; then
      restored_ref=$recovery_secondary_backup_ref
    fi
    if [[ -n "$restored_ref" ]]; then
      local restored_sha
      restored_sha=$(sha256sum "$binary_path" | awk '{print $1}') || restored_sha='unknown'
      write_state 'rolled_back' "$rollback_operation" "$recovery_source_sha" "$restored_sha" "$restored_ref" "$recovery_release_run_id" "$recovery_artifact_digest" || true
      emit_result "RESULT status=rolled_back operation=$rollback_operation backup_ref=$restored_ref" || true
    else
      best_effort_start_known_binary || true
      emit_result "RESULT status=rollback_failed operation=$rollback_operation backup_ref=$recovery_backup_ref" || true
    fi
    ((rc == 0)) && rc=1
  elif ((rc != 0)) && [[ "$result_emitted" != 'true' ]]; then
    emit_result "RESULT status=failed operation=$rollback_operation reason=$failure_reason" || true
  fi
  if [[ -n "$token_file" ]]; then
    rm -f "$token_file" || true
  fi
  cleanup_health_scratch || true
  health_token=''
  exit "$rc"
}

main() {
  trap on_exit EXIT
  trap 'failure_reason=signal_hup; exit 129' HUP
  trap 'failure_reason=signal_interrupt; exit 130' INT
  trap 'failure_reason=signal_terminate; exit 143' TERM
  [[ $EUID -eq 0 ]] || die 'root_required'
  command -v flock >/dev/null && command -v sqlite3 >/dev/null && command -v python3 >/dev/null && command -v curl >/dev/null || die 'missing_runtime_dependency'
  [[ -x "$binary_path" && -f "$database_path" ]] || die 'production_layout_mismatch'
  install -d -m 0700 "$deploy_root" "$backups_root" "$state_root"
  exec 9>"$lock_path"
  flock -n 9 || die 'deployment_lock_busy'

  [[ ${1:-} == '--token-file' && $# -ge 3 ]] || die 'missing_token_file_argument'
  local candidate_token_file=$2
  shift 2
  [[ "$candidate_token_file" =~ ^/opt/3x-ui-deploy/incoming/[0-9]+-[0-9]+/health-token$ ]] || die 'invalid_token_file_path'
  token_file=$candidate_token_file
  [[ -f "$token_file" && ! -L "$token_file" ]] || die 'invalid_token_file'
  [[ "$(stat -c '%u:%a' "$token_file")" == '0:600' ]] || die 'unsafe_token_file_permissions'
  IFS= read -r health_token <"$token_file" || die 'missing_health_token'
  rm -f "$token_file"
  token_file=''
  [[ "$health_token" =~ ^[A-Za-z0-9]{48}$ ]] || die 'invalid_health_token'

  local operation=${1:-}
  shift || true
  case "$operation" in
    deploy) deploy "$@" ;;
    rollback-binary) rollback_binary "$@" ;;
    *) die 'invalid_operation' ;;
  esac
  health_token=''
}

main "$@"
