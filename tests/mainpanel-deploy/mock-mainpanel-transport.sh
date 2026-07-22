#!/usr/bin/env bash
set -Eeuo pipefail

: "${MOCK_REMOTE_ROOT:?}"
: "${MOCK_REMOTE_LOG:?}"

mock_name=${0##*/}

log_call() {
  local argument
  printf '%s' "$mock_name" >>"$MOCK_REMOTE_LOG"
  for argument in "$@"; do
    printf '\t%q' "$argument" >>"$MOCK_REMOTE_LOG"
  done
  printf '\n' >>"$MOCK_REMOTE_LOG"
}

map_remote_path() {
  local value=$1
  case "$value" in
    /opt/*) printf '%s%s\n' "$MOCK_REMOTE_ROOT" "$value" ;;
    *) printf '%s\n' "$value" ;;
  esac
}

map_remote_argument() {
  local value=$1
  local path
  case "$value" in
    --property=StandardOutput=append:/opt/*|--property=StandardError=append:/opt/*)
      path=${value#*=append:}
      path=$(map_remote_path "$path")
      printf '%s=append:%s\n' "${value%%=append:*}" "$path"
      ;;
    /opt/*) map_remote_path "$value" ;;
    *) printf '%s\n' "$value" ;;
  esac
}

case "$mock_name" in
  ssh)
    command=''
    for command in "$@"; do :; done
    [[ -n "$command" ]]

    # github_remote_transaction.sh constructs this string itself with
    # printf %q, so it is safe and important to exercise its quoting here.
    eval "set -- $command"
    mapped=()
    for argument in "$@"; do
      mapped+=("$(map_remote_argument "$argument")")
    done
    set -- "${mapped[@]}"
    log_call "$@"
    PATH="$MOCK_REMOTE_BIN:/usr/bin:/bin:/usr/sbin:/sbin" "$@"
    ;;

  scp)
    arguments=("$@")
    count=${#arguments[@]}
    ((count >= 2))
    source_path=${arguments[$((count - 2))]}
    destination_path=${arguments[$((count - 1))]}
    log_call "$source_path" "$destination_path"

    if [[ "$source_path" == *:* ]]; then
      source_path=$(map_remote_path "${source_path#*:}")
    fi
    if [[ "$destination_path" == *:* ]]; then
      destination_path=$(map_remote_path "${destination_path#*:}")
    fi
    /bin/mkdir -p "${destination_path%/*}"
    /bin/cp "$source_path" "$destination_path"
    ;;

  install)
    log_call "$@"
    arguments=("$@")
    count=${#arguments[@]}
    destination_path=${arguments[$((count - 1))]}
    if [[ " $* " == *' -d '* ]]; then
      /bin/mkdir -p "$destination_path"
    else
      ((count >= 2))
      source_path=${arguments[$((count - 2))]}
      /bin/mkdir -p "${destination_path%/*}"
      if [[ "$source_path" == '/dev/null' ]]; then
        : >"$destination_path"
      else
        /bin/cp "$source_path" "$destination_path"
      fi
    fi
    ;;

  rm)
    log_call "$@"
    /bin/rm "$@"
    ;;

  chmod)
    log_call "$@"
    /bin/chmod "$@"
    ;;

  sha256sum)
    log_call "$@"
    : "${MOCK_REAL_SHA256SUM:?}"
    "$MOCK_REAL_SHA256SUM" "$@"
    ;;

  systemd-run)
    log_call "$@"
    result_file=''
    for argument in "$@"; do
      case "$argument" in
        --property=StandardOutput=append:*) result_file=${argument#*=append:} ;;
      esac
    done
    [[ -n "$result_file" ]]

    case "${MOCK_SCENARIO:?}" in
      success)
        printf '%s\n' "${MOCK_SUCCESS_RESULT:?}" >"$result_file"
        ;;
      rolled_back)
        printf '%s\n' "${MOCK_ROLLED_BACK_RESULT:?}" >"$result_file"
        ;;
      failed)
        printf '%s\n' "${MOCK_FAILED_RESULT:?}" >"$result_file"
        ;;
      duplicate_result)
        printf '%s\n%s\n' \
          "${MOCK_SUCCESS_RESULT:?}" \
          "${MOCK_SUCCESS_RESULT:?}" >"$result_file"
        ;;
      ambiguous_launch)
        # The transient unit was created and completed, but SSH lost the
        # systemd-run response. Reconciliation must consume this result.
        printf '%s\n' "${MOCK_SUCCESS_RESULT:?}" >"$result_file"
        : >"${result_file%/result}/launch-attempted"
        exit 255
        ;;
      definite_no_unit)
        # A failed launch with no unit and no result is reconciled via five
        # consecutive unknown states.
        exit 255
        ;;
      *)
        printf 'unknown mock scenario: %s\n' "$MOCK_SCENARIO" >&2
        exit 2
        ;;
    esac
    ;;

  systemctl)
    log_call "$@"
    if [[ " $* " == *' --value '* ]]; then
      if [[ "${MOCK_SCENARIO:?}" == 'definite_no_unit' ]]; then
        printf 'unknown\n'
      else
        printf 'inactive\n'
      fi
    elif [[ "${MOCK_SCENARIO:?}" == 'rolled_back' || "$MOCK_SCENARIO" == 'failed' ]]; then
      printf 'ActiveState=failed\nSubState=failed\nResult=exit-code\nExecMainStatus=1\n'
    else
      printf 'ActiveState=active\nSubState=exited\nResult=success\nExecMainStatus=0\n'
    fi
    ;;

  sleep)
    # Keep retry/polling tests deterministic and fast.
    log_call "$@"
    ;;

  *)
    printf 'unsupported mock command: %s\n' "$mock_name" >&2
    exit 2
    ;;
esac
