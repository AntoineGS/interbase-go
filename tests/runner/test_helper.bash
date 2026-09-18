#!/usr/bin/env bash

# Test helpers for the Docker integration runner.  The doubles intentionally
# record only structural facts, never the credential values themselves.
# shellcheck disable=SC2154

setup_runner_test() {
  RUNNER_PID=''
  export RUNNER="${BATS_TEST_DIRNAME}/../../scripts/test-integration-docker.sh"
  if [[ -v TMPDIR ]]; then
    ORIGINAL_TMPDIR_SET=1
    ORIGINAL_TMPDIR="$TMPDIR"
  else
    ORIGINAL_TMPDIR_SET=0
    ORIGINAL_TMPDIR=''
  fi
  ORIGINAL_PATH="$PATH"
  TEST_SANDBOX="$(mktemp -d "${TMPDIR:-/tmp}/interbase-go-runner-test.XXXXXX")"
  TEST_BIN="${TEST_SANDBOX}/bin"
  TEST_TMP="${TEST_SANDBOX}/tmp"
  mkdir -p -- "$TEST_BIN" "$TEST_TMP"
  mkdir -p -- "$TEST_TMP/include"
  printf '%s\n' '/* controlled SDK header */' > "$TEST_TMP/include/ibase.h"

  export FAKE_DOCKER_LOG="${TEST_SANDBOX}/docker.log"
  export FAKE_TIMEOUT_LOG="${TEST_SANDBOX}/timeout.log"
  export FAKE_GO_LOG="${TEST_SANDBOX}/go.log"
  export FAKE_GO_PHASE_LOG="${TEST_SANDBOX}/go.phases"
  export FAKE_GO_ATTEMPTS_FILE="${TEST_SANDBOX}/go.attempts"
  export FAKE_GO_STARTED="${TEST_SANDBOX}/go.started"
  export FAKE_DOCKER_STOP_STARTED="${TEST_SANDBOX}/docker.stop.started"
  export FAKE_ROOT_FILE="${TEST_SANDBOX}/runner-root"
  FAKE_REAL_TIMEOUT="$(command -v timeout)"
  export FAKE_REAL_TIMEOUT
  export PATH="${TEST_BIN}:${PATH}"
  export TMPDIR="$TEST_TMP"
  export INTERBASE_INCLUDE="$TEST_TMP/include"
  export IMAGE="example.invalid/interbase:fixture"
  export INTERBASE_DATABASE="/srv/interbase/operational.ib"
  export INTERBASE_USER="operational-user"
  export INTERBASE_PASSWORD="operational-password"
  export INTERBASE_TEST_ISQL="/tmp/override-isql"
  export INTERBASE_TEST_USER="override-user"
  export INTERBASE_TEST_PASSWORD="override-password"
  export INTERBASE_TEST_EXTRA="override-extra"
  unset CGO_CFLAGS CGO_LDFLAGS SERVICES_NATIVE_TRACE
  unset FAKE_DOCKER_INSPECT_FAIL FAKE_DOCKER_RUN_FAIL FAKE_DOCKER_CP_FAIL \
    FAKE_DOCKER_STOP_FAIL FAKE_DOCKER_STOP_SLEEP FAKE_DOCKER_RM_FAIL \
    FAKE_GO_FAIL_MODE FAKE_GO_READINESS_FAILURES FAKE_GO_READINESS_MODE \
    FAKE_GO_WRONG_ERROR_MODE FAKE_GO_CREATE_RETAINED FAKE_GO_SLEEP \
    FAKE_GO_REQUIRE_QUOTED_CGO_PATHS
  unset INTERBASE_PERF INTERBASE_SOAK INTERBASE_SOAK_DURATION \
    INTERBASE_SOAK_WORKERS INTERBASE_SOAK_SAMPLE_INTERVAL \
    INTERBASE_NATIVE_LIFECYCLE_RACE INTERBASE_UNSAFE_WORKLOAD_OVERRIDE

  : > "$FAKE_DOCKER_LOG"
  : > "$FAKE_TIMEOUT_LOG"
  : > "$FAKE_GO_LOG"
  : > "$FAKE_GO_PHASE_LOG"
  : > "$FAKE_GO_ATTEMPTS_FILE"
  create_fake_docker
  create_fake_timeout
  create_fake_go
  export FAKE_GO_RUNTIME="$TEST_BIN/go"
}

teardown_runner_test() {
  if [[ -n "${RUNNER_PID:-}" ]]; then
    kill -TERM "$RUNNER_PID" 2>/dev/null || true
    wait "$RUNNER_PID" 2>/dev/null || true
  fi
  rm -rf -- "${TEST_SANDBOX:?}"
  if ((ORIGINAL_TMPDIR_SET == 1)); then
    export TMPDIR="$ORIGINAL_TMPDIR"
  else
    unset TMPDIR
  fi
  export PATH="$ORIGINAL_PATH"
  RUNNER_PID=''
}

create_fake_docker() {
  cat > "$TEST_BIN/docker" <<'EOF'
#!/usr/bin/env bash
set -Eeuo pipefail

log_docker_call() {
  local argument
  printf 'docker' >> "$FAKE_DOCKER_LOG"
  for argument in "$@"; do
    printf ' %s' "$argument" >> "$FAKE_DOCKER_LOG"
  done
  printf '\n' >> "$FAKE_DOCKER_LOG"
}

log_docker_call "$@"

if [[ -n "${INTERBASE_DATABASE-}${INTERBASE_USER-}${INTERBASE_PASSWORD-}" ||
  -n "${IB_SYSDBA_USER-}${IB_SYSDBA_PASSWORD-}${IB_DATA_DIR-}${IB_BACKUP_DIR-}" ]]; then
  printf 'host-operational-vars=present\n' >> "$FAKE_DOCKER_LOG"
else
  printf 'host-operational-vars=absent\n' >> "$FAKE_DOCKER_LOG"
fi

case "${1-}" in
  image)
    if [[ "${FAKE_DOCKER_INSPECT_FAIL:-0}" == 1 ]]; then
      exit 31
    fi
    [[ "${2-}" == inspect ]]
    ;;
  run)
    cidfile=''
    env_file=''
    while (($# > 0)); do
      case "$1" in
        --cidfile)
          cidfile="$2"
          shift 2
          ;;
        --env-file)
          env_file="$2"
          shift 2
          ;;
        *)
          shift
          ;;
      esac
    done
    [[ -n "$cidfile" && -n "$env_file" ]]
    printf '%064d\n' 1 > "$cidfile"
    env_content="$(<"$env_file")"
    if [[ "$env_content" == *'IB_SYSDBA_PASSWORD=masterkey'* ]]; then
      printf 'server-password=default\n' >> "$FAKE_DOCKER_LOG"
    fi
    if [[ "$env_content" == *$'IB_BACKUP_DATABASES=\n'* ]]; then
      printf 'backups=disabled\n' >> "$FAKE_DOCKER_LOG"
    fi
    if [[ "${FAKE_DOCKER_RUN_FAIL:-0}" == 1 ]]; then
      exit 32
    fi
    ;;
  cp)
    if [[ "${FAKE_DOCKER_CP_FAIL:-0}" == 1 ]]; then
      exit 33
    fi
    source_ref="$3"
    destination="$4"
    case "$source_ref" in
      */opt/interbase/bin/isql)
        printf '#!/usr/bin/env bash\nexit 0\n' > "$destination"
        chmod 700 "$destination"
        ;;
      */opt/interbase/lib/libgds.so)
        printf 'controlled client library\n' > "$destination"
        chmod 600 "$destination"
        ;;
      *)
        exit 34
        ;;
    esac
    ;;
  exec)
    shift
    while (($# > 0)) && [[ "$1" == --env ]]; do
      [[ $# -ge 2 ]]
      if [[ "$2" == *=* ]]; then
        export "$2"
      elif [[ -v "$2" ]]; then
        export "$2"
      else
        printf -v "$2" '%s' ''
        export "$2"
      fi
      shift 2
    done
    [[ $# -ge 2 ]]
    container_id="$1"
    test_binary="$2"
    shift 2
    [[ -n "$container_id" && -x "$test_binary" ]]
    "$FAKE_GO_RUNTIME" "$@"
    ;;
  stop)
    if [[ "${FAKE_DOCKER_STOP_FAIL:-0}" == 1 ]]; then
      exit 35
    fi
    if [[ "${FAKE_DOCKER_STOP_SLEEP:-0}" == 1 ]]; then
      : > "$FAKE_DOCKER_STOP_STARTED"
      sleep 1
    fi
    ;;
  rm)
    if [[ "${FAKE_DOCKER_RM_FAIL:-0}" == 1 ]]; then
      exit 36
    fi
    ;;
  *)
    exit 37
    ;;
esac
EOF
  chmod 700 "$TEST_BIN/docker"
}

create_fake_timeout() {
  cat > "$TEST_BIN/timeout" <<'EOF'
#!/usr/bin/env bash
set -Eeuo pipefail

printf 'timeout' >> "$FAKE_TIMEOUT_LOG"
for argument in "$@"; do
  printf ' %q' "$argument" >> "$FAKE_TIMEOUT_LOG"
done
printf '\n' >> "$FAKE_TIMEOUT_LOG"
exec "$FAKE_REAL_TIMEOUT" "$@"
EOF
  chmod 700 "$TEST_BIN/timeout"
}

create_fake_go() {
  cat > "$TEST_BIN/go" <<'EOF'
#!/usr/bin/env bash
set -Eeuo pipefail

log_go_call() {
  local argument
  printf 'go' >> "$FAKE_GO_LOG"
  for argument in "$@"; do
    printf ' %q' "$argument" >> "$FAKE_GO_LOG"
  done
  printf '\n' >> "$FAKE_GO_LOG"
}

log_go_call "$@"

is_build=0
output_file=''
previous=''
for argument in "$@"; do
  if [[ "$argument" == -c ]]; then
    is_build=1
  elif [[ "$previous" == -o ]]; then
    output_file="$argument"
  fi
  previous="$argument"
done
if [[ "$is_build" -eq 1 ]]; then
  [[ -n "$output_file" ]]
  printf 'build-cflags=%s\n' "${CGO_CFLAGS-}" >> "$FAKE_GO_LOG"
  printf 'build-ldflags=%s\n' "${CGO_LDFLAGS-}" >> "$FAKE_GO_LOG"
  if [[ "${FAKE_GO_REQUIRE_QUOTED_CGO_PATHS:-0}" == 1 ]]; then
    expected_include="\"-I${INTERBASE_INCLUDE}\""
    build_root="${output_file%/*}"
    expected_root="\"-L${build_root}\" \"-Wl,-rpath,${build_root}\""
    [[ "${CGO_CFLAGS-}" == "$expected_include" ]]
    [[ "${CGO_LDFLAGS-}" == "$expected_root" ]]
  fi
  printf '#!/usr/bin/env bash\nexit 0\n' > "$output_file"
  chmod 700 "$output_file"
  exit 0
fi

printf 'database=%s user=%s test-isql=%s\n' \
  "${INTERBASE_DATABASE+present}" "${INTERBASE_USER+present}" \
  "${INTERBASE_TEST_ISQL+present}" >> "$FAKE_GO_LOG"
printf 'server=%s\n' "${INTERBASE_TEST_SERVER-}" >> "$FAKE_GO_LOG"
if [[ "${INTERBASE_TEST_ISQL-}" == "$TMPDIR/isql" ]]; then
  printf 'test-isql-path=owned\n' >> "$FAKE_GO_LOG"
else
  printf 'test-isql-path=unexpected\n' >> "$FAKE_GO_LOG"
fi
printf 'password-state=' >> "$FAKE_GO_LOG"
if [[ "${INTERBASE_TEST_PASSWORD-}" == masterkey ]]; then
  printf 'default\n' >> "$FAKE_GO_LOG"
  password_state=default
elif [[ -n "${INTERBASE_TEST_PASSWORD-}" ]]; then
  printf 'wrong\n' >> "$FAKE_GO_LOG"
  password_state=wrong
else
  printf 'unexpected\n' >> "$FAKE_GO_LOG"
  password_state=unexpected
fi
printf 'workload-env perf=%s soak=%s duration=%s workers=%s sample=%s native=%s\n' \
  "${INTERBASE_PERF-}" "${INTERBASE_SOAK-}" \
  "${INTERBASE_SOAK_DURATION-}" "${INTERBASE_SOAK_WORKERS-}" \
  "${INTERBASE_SOAK_SAMPLE_INTERVAL-}" \
  "${INTERBASE_NATIVE_LIFECYCLE_RACE-}" >> "$FAKE_GO_LOG"
printf '%s\n' "$TMPDIR" > "$FAKE_ROOT_FILE"

is_smoke=0
is_service=0
for argument in "$@"; do
  if [[ "$argument" == '^TestReadFixtureSmoke$' ]]; then
    is_smoke=1
  elif [[ "$argument" == '^TestNativeService('* ]]; then
    is_service=1
  fi
done

if [[ "$is_service" -eq 1 ]]; then
  printf 'service-precheck\n' >> "$FAKE_GO_PHASE_LOG"
elif [[ "$is_smoke" -eq 1 && "$password_state" == default ]]; then
  printf 'readiness-default\n' >> "$FAKE_GO_PHASE_LOG"
elif [[ "$is_smoke" -eq 1 && "$password_state" == wrong ]]; then
  printf 'readiness-wrong\n' >> "$FAKE_GO_PHASE_LOG"
else
  printf 'requested\n' >> "$FAKE_GO_PHASE_LOG"
fi

if [[ "$is_smoke" -eq 1 && "$password_state" == default ]]; then
  attempts=0
  if [[ -s "$FAKE_GO_ATTEMPTS_FILE" ]]; then
    attempts="$(<"$FAKE_GO_ATTEMPTS_FILE")"
  fi
  attempts=$((attempts + 1))
  printf '%s\n' "$attempts" > "$FAKE_GO_ATTEMPTS_FILE"
  if [[ "${FAKE_GO_READINESS_MODE:-}" == always-fail ||
    "$attempts" -le "${FAKE_GO_READINESS_FAILURES:-0}" ]]; then
    printf 'readiness failed with password=masterkey\n' >&2
    exit 42
  fi
fi

if [[ "${FAKE_GO_CREATE_RETAINED:-0}" == 1 && "$password_state" == default ]]; then
  mkdir -p -- "$TMPDIR/retained-fixture"
  printf 'fixture artifact\n' > "$TMPDIR/retained-fixture/database.ib"
fi

if [[ "$password_state" == wrong ]]; then
  case "${FAKE_GO_WRONG_ERROR_MODE:-auth}" in
    generic)
      printf 'SQLCODE = -902\nconnection refused\n' >&2
      ;;
    code)
      printf 'native status 335544472\n' >&2
      ;;
    auth)
      printf 'Statement failed, SQLSTATE = 28000\nYour user name and password are not defined.\n' >&2
      ;;
    *)
      printf 'unrecognized controlled error mode\n' >&2
      ;;
  esac
  exit 41
fi

if [[ "${FAKE_GO_FAIL_MODE:-}" == requested && "$is_smoke" -eq 0 && "$is_service" -eq 0 ]]; then
  printf 'controlled test failure\n' >&2
  exit 23
fi

if [[ "${FAKE_GO_SLEEP:-0}" == 1 ]]; then
  : > "$FAKE_GO_STARTED"
  trap 'exit 143' TERM INT HUP
  sleep 30
fi

printf 'controlled go test success\n'
EOF
  chmod 700 "$TEST_BIN/go"
}

runner_root_from_log() {
  local root
  root="$(<"$FAKE_ROOT_FILE")"
  [[ -n "$root" ]]
  printf '%s\n' "$root"
}
