#!/usr/bin/env bash
# Run the integration contracts against an explicitly supplied local Docker
# image. Linux/amd64 and Bash 4.4+ are required by the existing cgo driver.
# No image is built, pulled, or published by this script.
# Maintainer: interbase-go contributors. Updated: 2026-09-13.
# shellcheck disable=SC2154,SC2329 # ERR/cleanup/signal traps use callback state.

if ((BASH_VERSINFO[0] < 4 || (BASH_VERSINFO[0] == 4 && BASH_VERSINFO[1] < 4))); then
  printf '%s\n' 'ERROR: Bash 4.4 or newer is required.' >&2
  exit 2
fi

set -Eeuo pipefail
shopt -s inherit_errexit
IFS=$'\n\t'
umask 077

readonly SCRIPT_NAME='test-integration-docker.sh'
readonly SERVER_USER='SYSDBA'
readonly SERVER_PASSWORD='masterkey'
readonly INVALID_PASSWORD='interbase-go-readiness-invalid'
readonly SERVER_PORT='3050'
readonly SERVER_BIND='127.0.0.1'
readonly READINESS_MAX_ATTEMPTS=5
readonly READINESS_DEADLINE_SECONDS=30
readonly READINESS_DELAY='1s'
readonly READINESS_TEST_TIMEOUT='10s'
readonly READINESS_COMMAND_TIMEOUT='15s'
readonly COMMAND_TIMEOUT='30s'
readonly REQUESTED_TEST_TIMEOUT='10m'
readonly REQUESTED_COMMAND_TIMEOUT='11m'
readonly REQUESTED_TEST_TIMEOUT_SECONDS=600
readonly REQUESTED_COMMAND_GRACE_SECONDS=60
readonly SOAK_SETUP_GRACE_SECONDS=60
readonly SOAK_DEFAULT_RUNTIME_SECONDS=15
readonly SOAK_MAX_DURATION_SECONDS=86400
readonly SOAK_MAX_WORKERS=64
readonly CANCELLATION_DEFAULT_ITERATIONS=10
readonly CANCELLATION_MAX_ITERATIONS=10000
readonly CANCELLATION_ITERATION_TIMEOUT_SECONDS=30
readonly CANCELLATION_OPERATIONS_PER_ITERATION=5
readonly CANCELLATION_VERIFICATION_SECONDS=30
readonly AUTH_FAILURE_REGEX='(335544472|SQLSTATE[[:space:]]*[:=][[:space:]]*28000|[Yy]our[[:space:]]+[Uu]ser[[:space:]]+name[[:space:]]+and[[:space:]]+[Pp]assword[[:space:]]+are[[:space:]]+not[[:space:]]+defined)'

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)"
readonly SCRIPT_DIR
REPO_ROOT="$(cd -- "${SCRIPT_DIR}/.." && pwd -P)"
readonly REPO_ROOT

IMAGE="${IMAGE:-}"
INTERBASE_INCLUDE="${INTERBASE_INCLUDE:-}"
DOCKER_BIN=''
GO_BIN=''
TEMP_PARENT=''
TEST_ROOT=''
CONTAINER_NAME=''
CONTAINER_ID=''
CONTAINER_START_ATTEMPTED=0
ACTIVE_PID=0
CID_FILE=''
ENV_FILE=''
DOCKER_OUTPUT=''
GO_OUTPUT=''
ISQL_FILE=''
LIB_FILE=''
INTEGRATION_BIN=''
SERVICES_BIN=''
declare -a GO_ARGS=()
declare -a GO_BUILD_ARGS=()
declare -a GO_TEST_ARGS=()
declare -a FULL_GO_ARGS=()
declare -a TEST_ARGS=()
declare -a REQUESTED_WORKLOAD_ENV_ARGS=()
declare -a OWNED_FILES=()

PERFORMANCE_ENABLED=0
SOAK_ENABLED=0
SOAK_DURATION=''
SOAK_WORKERS=''
SOAK_SAMPLE_INTERVAL=''
NATIVE_LIFECYCLE_ENABLED=0
CANCELLATION_ENABLED=0
CANCELLATION_ITERATIONS=''
CANCELLATION_ITERATIONS_SET=0
EFFECTIVE_REQUESTED_TEST_TIMEOUT="${REQUESTED_TEST_TIMEOUT}"
REQUESTED_COMMAND_DURATION="${REQUESTED_COMMAND_TIMEOUT}"
SOAK_DURATION_SECONDS=0
CANCELLATION_DURATION_SECONDS=0
REQUESTED_GO_TIMEOUT_SECONDS=0
REQUESTED_TIMEOUT_EXPLICIT=0
REQUESTED_TIMEOUT_VALUE=''
PARSED_DURATION_SECONDS=0
PARSED_DURATION_NONZERO=0
NORMALIZED_DECIMAL=''

log_info() {
  printf '[%(%Y-%m-%dT%H:%M:%SZ)T] INFO %s\n' -1 "$*" >&2
}

log_error() {
  printf '[%(%Y-%m-%dT%H:%M:%SZ)T] ERROR %s\n' -1 "$*" >&2
}

trap 'error_status=$?; log_error "runner command failed at line ${LINENO} (status ${error_status})"; exit "${error_status}"' ERR

usage() {
  local status="${1:-0}"
  printf '%s\n' \
    "Usage: ${SCRIPT_NAME} [RUNNER_OPTIONS] [GO_TEST_ARGS...]" \
    '' \
    'Runs fixture readiness checks and then ./integration in a disposable' \
    'Docker-backed environment.' \
    '' \
    'Required environment:' \
    '  IMAGE         Existing local Docker image reference (never pulled).' \
    '  INTERBASE_INCLUDE  Host directory containing the matching ibase.h.' \
    '' \
    'Runner options:' \
    '  -h, --help    Show this help.' \
    '      --performance  Enable live benchmarks for the requested test command.' \
    '      --soak  Enable the concurrent live soak workload.' \
    '      --soak-duration=DURATION  Soak duration; positive integer duration using s, m, or h, max 24h.' \
    '      --soak-workers=WORKERS  Soak workers; an integer between 1 and 64.' \
    '      --soak-sample-interval=DURATION  Soak sample interval; positive integer duration using s, m, or h, max 24h.' \
    '      --native-lifecycle  Enable the native lifecycle race for the requested test command.' \
    '      --cancellation  Enable repeated live DSQL cancellation contracts.' \
    '      --cancellation-iterations=COUNT  Cancellation iterations; an integer between 1 and 10000.' \
    '      --        End runner options; pass the rest to go test.' \
    '' \
    'Supported compiled-binary flag: -race.' \
    'Runtime flags such as -run, -v, -benchmem, and -memprofilerate are translated' \
    'to the -test.* form accepted by the compiled binary.' \
    '' \
    'Defaults for the requested run are -count=1 -timeout=10m.' \
    'For soak runs, the default Go timeout is extended past the soak duration' \
    'with setup grace, and the external deadline adds another 60 seconds.' \
    'An explicit -timeout shorter than the soak duration is rejected.' \
    'Example: IMAGE=sha256:... ./scripts/test-integration-docker.sh -run ^TestRead$' \
    '' \
    'Exit status is the Go test status; cleanup failure is returned when tests succeed.' \
    'Prerequisites: Bash 4.4+, Docker CLI/daemon, Go, and GNU timeout.' >&2
  exit "${status}"
}

die() {
  local status="$1"
  shift
  log_error "$*"
  exit "${status}"
}

parse_arguments() {
  while (($# > 0)); do
    case "$1" in
      -h | --help)
        usage 0
        ;;
      --performance)
        PERFORMANCE_ENABLED=1
        shift
        ;;
      --performance=*)
        die 2 '--performance does not accept a value'
        ;;
      --soak)
        SOAK_ENABLED=1
        shift
        ;;
      --soak=*)
        die 2 '--soak does not accept a value'
        ;;
      --soak-duration=*)
        SOAK_DURATION="${1#*=}"
        shift
        ;;
      --soak-duration)
        (($# >= 2)) || die 2 '--soak-duration requires a value'
        SOAK_DURATION="$2"
        shift 2
        ;;
      --soak-workers=*)
        SOAK_WORKERS="${1#*=}"
        shift
        ;;
      --soak-workers)
        (($# >= 2)) || die 2 '--soak-workers requires a value'
        SOAK_WORKERS="$2"
        shift 2
        ;;
      --soak-sample-interval=*)
        SOAK_SAMPLE_INTERVAL="${1#*=}"
        shift
        ;;
      --soak-sample-interval)
        (($# >= 2)) || die 2 '--soak-sample-interval requires a value'
        SOAK_SAMPLE_INTERVAL="$2"
        shift 2
        ;;
      --native-lifecycle)
        NATIVE_LIFECYCLE_ENABLED=1
        shift
        ;;
      --native-lifecycle=*)
        die 2 '--native-lifecycle does not accept a value'
        ;;
      --cancellation)
        CANCELLATION_ENABLED=1
        shift
        ;;
      --cancellation=*)
        die 2 '--cancellation does not accept a value'
        ;;
      --cancellation-iterations=*)
        CANCELLATION_ITERATIONS="${1#*=}"
        CANCELLATION_ITERATIONS_SET=1
        shift
        ;;
      --cancellation-iterations)
        (($# >= 2)) || die 2 '--cancellation-iterations requires a value'
        CANCELLATION_ITERATIONS="$2"
        CANCELLATION_ITERATIONS_SET=1
        shift 2
        ;;
      --)
        shift
        GO_ARGS+=("$@")
        break
        ;;
      *)
        GO_ARGS+=("$1")
        shift
        ;;
    esac
  done
}

split_go_arguments() {
  GO_BUILD_ARGS=()
  GO_TEST_ARGS=()
  while (($# > 0)); do
    case "$1" in
      -race)
        GO_BUILD_ARGS+=("$1")
        ;;
      *)
        GO_TEST_ARGS+=("$1")
        ;;
    esac
    shift
  done
}

normalize_decimal() {
  local value="$1"
  while [[ "${#value}" -gt 1 && "${value:0:1}" == 0 ]]; do
    value="${value:1}"
  done
  NORMALIZED_DECIMAL="${value}"
}

parse_duration_seconds() {
  local option_name="$1"
  local value="$2"
  local amount=''
  local unit=''
  local maximum=0
  local factor=0

  if [[ ! "${value}" =~ ^([0-9]+)(s|m|h)$ ]]; then
    die 2 "${option_name} must be a positive integer duration using s, m, or h (max 24h)"
  fi
  amount="${BASH_REMATCH[1]}"
  unit="${BASH_REMATCH[2]}"
  normalize_decimal "${amount}"
  amount="${NORMALIZED_DECIMAL}"

  case "${unit}" in
    s)
      maximum="${SOAK_MAX_DURATION_SECONDS}"
      factor=1
      ;;
    m)
      maximum=$((SOAK_MAX_DURATION_SECONDS / 60))
      factor=60
      ;;
    h)
      maximum=$((SOAK_MAX_DURATION_SECONDS / 3600))
      factor=3600
      ;;
    *)
      die 2 "${option_name} must be a positive integer duration using s, m, or h (max 24h)"
      ;;
  esac
  if [[ "${amount}" == 0 || ${#amount} -gt ${#maximum} ]] || ((10#${amount} > maximum)); then
    die 2 "${option_name} must be a positive integer duration using s, m, or h (max 24h)"
  fi
  PARSED_DURATION_SECONDS=$((10#${amount} * factor))
}

parse_worker_count() {
  local value="$1"

  if [[ ! "${value}" =~ ^[0-9]+$ ]]; then
    die 2 '--soak-workers must be an integer between 1 and 64'
  fi
  normalize_decimal "${value}"
  value="${NORMALIZED_DECIMAL}"
  if [[ "${value}" == 0 || ${#value} -gt 2 ]] || ((10#${value} > SOAK_MAX_WORKERS)); then
    die 2 '--soak-workers must be an integer between 1 and 64'
  fi
}

parse_cancellation_iterations() {
  local value="$1"

  if [[ ! "${value}" =~ ^[0-9]+$ ]]; then
    die 2 '--cancellation-iterations must be an integer between 1 and 10000'
  fi
  normalize_decimal "${value}"
  value="${NORMALIZED_DECIMAL}"
  if [[ "${value}" == 0 || ${#value} -gt 5 ]] || ((10#${value} > CANCELLATION_MAX_ITERATIONS)); then
    die 2 '--cancellation-iterations must be an integer between 1 and 10000'
  fi
  CANCELLATION_ITERATIONS="${value}"
}

parse_go_timeout_seconds() {
  local value="$1"
  local remainder="${value}"
  local amount=''
  local unit=''
  local factor=0
  local total=0
  local subsecond_nanos=0

  if [[ "${value}" == 0 ]]; then
    PARSED_DURATION_SECONDS=0
    PARSED_DURATION_NONZERO=0
    return 0
  fi
  PARSED_DURATION_NONZERO=0
  while [[ -n "${remainder}" ]]; do
    if [[ ! "${remainder}" =~ ^([0-9]+)(ns|us|µs|ms|h|m|s)(.*)$ ]]; then
      return 1
    fi
    amount="${BASH_REMATCH[1]}"
    unit="${BASH_REMATCH[2]}"
    remainder="${BASH_REMATCH[3]}"
    normalize_decimal "${amount}"
    amount="${NORMALIZED_DECIMAL}"
    if [[ "${amount}" != 0 ]]; then
      PARSED_DURATION_NONZERO=1
    fi
    if [[ ${#amount} -gt 12 ]]; then
      return 1
    fi
    case "${unit}" in
      s) factor=1 ;;
      m) factor=60 ;;
      h) factor=3600 ;;
      ns) subsecond_nanos=$((subsecond_nanos + 10#${amount})) ;;
      us | µs) subsecond_nanos=$((subsecond_nanos + 10#${amount} * 1000)) ;;
      ms) subsecond_nanos=$((subsecond_nanos + 10#${amount} * 1000000)) ;;
      *) return 1 ;;
    esac
    if [[ "${unit}" == h || "${unit}" == m || "${unit}" == s ]]; then
      total=$((total + 10#${amount} * factor))
    fi
  done
  PARSED_DURATION_SECONDS=$((total + subsecond_nanos / 1000000000))
}

find_requested_timeout() {
  local index=0
  local argument=''

  REQUESTED_TIMEOUT_EXPLICIT=0
  REQUESTED_TIMEOUT_VALUE=''
  while ((index < ${#GO_TEST_ARGS[@]})); do
    argument="${GO_TEST_ARGS[index]}"
    case "${argument}" in
      -timeout=* | -test.timeout=*)
        REQUESTED_TIMEOUT_EXPLICIT=1
        REQUESTED_TIMEOUT_VALUE="${argument#*=}"
        index=$((index + 1))
        ;;
      -timeout | -test.timeout)
        ((index + 1 < ${#GO_TEST_ARGS[@]})) || die 2 "${argument} requires a duration value"
        REQUESTED_TIMEOUT_EXPLICIT=1
        REQUESTED_TIMEOUT_VALUE="${GO_TEST_ARGS[index + 1]}"
        index=$((index + 2))
        ;;
      *)
        index=$((index + 1))
        ;;
    esac
  done
}

validate_workload_options() {
  if ((CANCELLATION_ITERATIONS_SET == 1)); then
    parse_cancellation_iterations "${CANCELLATION_ITERATIONS}"
  fi
  if ((CANCELLATION_ENABLED == 0 && CANCELLATION_ITERATIONS_SET == 1)); then
    die 2 '--cancellation-iterations require --cancellation'
  fi
  if ((CANCELLATION_ENABLED == 1)); then
    if ((CANCELLATION_ITERATIONS_SET == 0)); then
      CANCELLATION_ITERATIONS="${CANCELLATION_DEFAULT_ITERATIONS}"
      parse_cancellation_iterations "${CANCELLATION_ITERATIONS}"
    fi
    CANCELLATION_DURATION_SECONDS=$((10#${CANCELLATION_ITERATIONS} * (CANCELLATION_OPERATIONS_PER_ITERATION * CANCELLATION_ITERATION_TIMEOUT_SECONDS + CANCELLATION_VERIFICATION_SECONDS)))
  fi
  if ((SOAK_ENABLED == 0)) && [[ -n "${SOAK_DURATION}" || -n "${SOAK_WORKERS}" || -n "${SOAK_SAMPLE_INTERVAL}" ]]; then
    die 2 '--soak-duration, --soak-workers, and --soak-sample-interval require --soak'
  fi
  if ((SOAK_ENABLED == 0)); then
    return 0
  fi

  SOAK_DURATION_SECONDS="${SOAK_DEFAULT_RUNTIME_SECONDS}"
  if [[ -n "${SOAK_DURATION}" ]]; then
    parse_duration_seconds '--soak-duration' "${SOAK_DURATION}"
    SOAK_DURATION_SECONDS="${PARSED_DURATION_SECONDS}"
  fi
  if [[ -n "${SOAK_SAMPLE_INTERVAL}" ]]; then
    parse_duration_seconds '--soak-sample-interval' "${SOAK_SAMPLE_INTERVAL}"
  fi
  if [[ -n "${SOAK_WORKERS}" ]]; then
    parse_worker_count "${SOAK_WORKERS}"
  fi
}

configure_requested_deadlines() {
  local minimum_go_timeout_seconds=0
  local minimum_command_seconds=0

  EFFECTIVE_REQUESTED_TEST_TIMEOUT="${REQUESTED_TEST_TIMEOUT}"
  REQUESTED_COMMAND_DURATION="${REQUESTED_COMMAND_TIMEOUT}"
  REQUESTED_GO_TIMEOUT_SECONDS="${REQUESTED_TEST_TIMEOUT_SECONDS}"
  find_requested_timeout

  if ((SOAK_ENABLED == 1 || CANCELLATION_ENABLED == 1)); then
    minimum_go_timeout_seconds=$((CANCELLATION_DURATION_SECONDS + SOAK_DURATION_SECONDS + SOAK_SETUP_GRACE_SECONDS))
    if ((REQUESTED_TIMEOUT_EXPLICIT == 1)); then
      EFFECTIVE_REQUESTED_TEST_TIMEOUT="${REQUESTED_TIMEOUT_VALUE}"
      if parse_go_timeout_seconds "${REQUESTED_TIMEOUT_VALUE}"; then
        REQUESTED_GO_TIMEOUT_SECONDS="${PARSED_DURATION_SECONDS}"
        if ((PARSED_DURATION_NONZERO == 1 && REQUESTED_GO_TIMEOUT_SECONDS < minimum_go_timeout_seconds)); then
          die 2 "-timeout must not be shorter than the soak duration or cancellation workload (${minimum_go_timeout_seconds}s)"
        fi
      else
        die 2 '-timeout must use an integer-unit Go duration (s, m, or h) for soak runs'
      fi
    elif ((minimum_go_timeout_seconds > REQUESTED_TEST_TIMEOUT_SECONDS)); then
      EFFECTIVE_REQUESTED_TEST_TIMEOUT="${minimum_go_timeout_seconds}s"
      REQUESTED_GO_TIMEOUT_SECONDS="${minimum_go_timeout_seconds}"
    fi
    if ((REQUESTED_GO_TIMEOUT_SECONDS == 0)); then
      minimum_command_seconds=$((minimum_go_timeout_seconds + REQUESTED_COMMAND_GRACE_SECONDS))
    else
      minimum_command_seconds=$((REQUESTED_GO_TIMEOUT_SECONDS + REQUESTED_COMMAND_GRACE_SECONDS))
    fi
    REQUESTED_COMMAND_DURATION="${minimum_command_seconds}s"
  fi

  FULL_GO_ARGS=(-count=1 -timeout="${EFFECTIVE_REQUESTED_TEST_TIMEOUT}" "${GO_TEST_ARGS[@]}")
}

build_requested_workload_environment() {
  REQUESTED_WORKLOAD_ENV_ARGS=()
  if ((PERFORMANCE_ENABLED == 1)); then
    REQUESTED_WORKLOAD_ENV_ARGS+=(--env 'INTERBASE_PERF=1')
  fi
  if ((SOAK_ENABLED == 1)); then
    REQUESTED_WORKLOAD_ENV_ARGS+=(--env 'INTERBASE_SOAK=1')
    if [[ -n "${SOAK_DURATION}" ]]; then
      REQUESTED_WORKLOAD_ENV_ARGS+=(--env "INTERBASE_SOAK_DURATION=${SOAK_DURATION}")
    fi
    if [[ -n "${SOAK_WORKERS}" ]]; then
      REQUESTED_WORKLOAD_ENV_ARGS+=(--env "INTERBASE_SOAK_WORKERS=${SOAK_WORKERS}")
    fi
    if [[ -n "${SOAK_SAMPLE_INTERVAL}" ]]; then
      REQUESTED_WORKLOAD_ENV_ARGS+=(--env "INTERBASE_SOAK_SAMPLE_INTERVAL=${SOAK_SAMPLE_INTERVAL}")
    fi
  fi
  if ((NATIVE_LIFECYCLE_ENABLED == 1)); then
    REQUESTED_WORKLOAD_ENV_ARGS+=(--env 'INTERBASE_NATIVE_LIFECYCLE_RACE=1')
  fi
  if ((CANCELLATION_ENABLED == 1)); then
    REQUESTED_WORKLOAD_ENV_ARGS+=(--env 'INTERBASE_CANCELLATION=1')
    REQUESTED_WORKLOAD_ENV_ARGS+=(--env "INTERBASE_CANCELLATION_ITERATIONS=${CANCELLATION_ITERATIONS}")
  fi
}

require_command() {
  local command_name="$1"
  command -v "${command_name}" >/dev/null 2>&1 ||
    die 2 "required command is unavailable: ${command_name}"
}

scrub_operational_environment() {
  local name=''
  while IFS= read -r name; do
    case "${name}" in
      INTERBASE | INTERBASE_* | IB_* | ISC_*)
        if [[ "${name}" != INTERBASE_INCLUDE ]]; then
          unset "${name}"
        fi
      ;;
    esac
  done < <(compgen -e || true)
}

validate_image() {
  if [[ -z "${IMAGE}" ]]; then
    die 2 'IMAGE is required and must name an existing local Docker image'
  fi
  if [[ ! "${IMAGE}" =~ ^[[:alnum:]][[:alnum:]_.:/@-]*$ ]]; then
    die 2 'IMAGE contains an invalid Docker image reference'
  fi
}

validate_interbase_include() {
  if [[ -z "${INTERBASE_INCLUDE}" ]]; then
    die 2 'INTERBASE_INCLUDE is required and must contain the matching ibase.h'
  fi
  if [[ "${INTERBASE_INCLUDE}" != /* || "${INTERBASE_INCLUDE}" == *:* || "${INTERBASE_INCLUDE}" == *,* || "${INTERBASE_INCLUDE}" == *[[:cntrl:]]* ]]; then
    die 2 'INTERBASE_INCLUDE must be an absolute local path without colons or commas or control characters'
  fi
  if [[ ! -d "${INTERBASE_INCLUDE}" || ! -f "${INTERBASE_INCLUDE}/ibase.h" || -L "${INTERBASE_INCLUDE}/ibase.h" ]]; then
    die 2 'INTERBASE_INCLUDE must contain a regular ibase.h file'
  fi
}

validate_temp_parent() {
  TEMP_PARENT="${TMPDIR:-/tmp}"
  if [[ "${TEMP_PARENT}" != /* || "${TEMP_PARENT}" == *:* || "${TEMP_PARENT}" == *,* || "${TEMP_PARENT}" == *[[:cntrl:]]* ]]; then
    die 2 'TMPDIR must be an absolute local path without colons or commas or control characters'
  fi
  if [[ ! -d "${TEMP_PARENT}" || ! -w "${TEMP_PARENT}" ]]; then
    die 2 'TMPDIR must be an existing writable directory'
  fi
  if [[ "${TEMP_PARENT}" != / ]]; then
    TEMP_PARENT="${TEMP_PARENT%/}"
  fi
}

create_temp_root() {
  local template=''
  if [[ "${TEMP_PARENT}" == / ]]; then
    template='/interbase-go-docker.XXXXXX'
  else
    template="${TEMP_PARENT}/interbase-go-docker.XXXXXX"
  fi

  if ! TEST_ROOT="$(mktemp -d -- "${template}")"; then
    die 1 'could not create a private temporary root'
  fi
  if [[ "${TEST_ROOT}" != /* || "${TEST_ROOT}" == *:* || "${TEST_ROOT}" == *,* || "${TEST_ROOT}" == *[[:cntrl:]]* || ! -d "${TEST_ROOT}" || -L "${TEST_ROOT}" ]]; then
    die 1 'mktemp returned an unsafe temporary root'
  fi
  if [[ "${TEMP_PARENT}" == / ]]; then
    case "${TEST_ROOT}" in
      /interbase-go-docker.*) ;;
      *) die 1 'mktemp returned a temporary root outside the requested parent' ;;
    esac
  else
    case "${TEST_ROOT}" in
      "${TEMP_PARENT}"/interbase-go-docker.*) ;;
      *) die 1 'mktemp returned a temporary root outside the requested parent' ;;
    esac
  fi

  CID_FILE="${TEST_ROOT}/container.cid"
  ENV_FILE="${TEST_ROOT}/container.env"
  DOCKER_OUTPUT="${TEST_ROOT}/docker.output"
  GO_OUTPUT="${TEST_ROOT}/go.output"
  ISQL_FILE="${TEST_ROOT}/isql"
  LIB_FILE="${TEST_ROOT}/libgds.so"
  INTEGRATION_BIN="${TEST_ROOT}/integration.test"
  SERVICES_BIN="${TEST_ROOT}/services.test"
  CONTAINER_NAME="interbase-go-test-${TEST_ROOT##*/}"
  OWNED_FILES=("${CID_FILE}" "${ENV_FILE}" "${DOCKER_OUTPUT}" "${GO_OUTPUT}" "${ISQL_FILE}" "${LIB_FILE}" "${INTEGRATION_BIN}" "${SERVICES_BIN}")
}

write_server_environment() {
  if ! printf '%s\n' \
    'IB_SYSDBA_USER=SYSDBA' \
    'IB_SYSDBA_PASSWORD=masterkey' \
    'IB_DATA_DIR=/var/lib/interbase/data' \
    'IB_BACKUP_DIR=/var/lib/interbase/backups' \
    'IB_LOG_DIR=/var/lib/interbase/logs' \
    'IB_BACKUP_DATABASES=' \
    'GBAK_BACKUP_OPTS=' >"${ENV_FILE}"; then
    die 1 'could not create the private Docker environment file'
  fi
}

run_timed() {
  local output_file="$1"
  local duration="$2"
  local status=0
  shift 2

  timeout --signal=TERM --kill-after=5s "${duration}" "$@" >"${output_file}" 2>&1 &
  ACTIVE_PID=$!
  if wait "${ACTIVE_PID}"; then
    status=0
  else
    status=$?
  fi
  ACTIVE_PID=0
  return "${status}"
}

validate_local_image() {
  if ! run_timed /dev/null "${COMMAND_TIMEOUT}" "${DOCKER_BIN}" image inspect "${IMAGE}"; then
    die 2 'IMAGE must name an existing local Docker image; no pull was attempted'
  fi
}

start_container() {
  local -a docker_args=(
    run
    --pull=never
    --detach
    --name "${CONTAINER_NAME}"
    --cidfile "${CID_FILE}"
    --publish "${SERVER_BIND}:${SERVER_PORT}:${SERVER_PORT}"
    --mount "type=bind,source=${TEST_ROOT},target=${TEST_ROOT}"
    --mount 'type=tmpfs,destination=/var/lib/interbase'
    --env-file "${ENV_FILE}"
    "${IMAGE}"
  )
  CONTAINER_START_ATTEMPTED=1
  if ! run_timed "${DOCKER_OUTPUT}" "${COMMAND_TIMEOUT}" "${DOCKER_BIN}" "${docker_args[@]}"; then
    load_container_id || true
    die 1 'could not start the isolated InterBase container'
  fi
  if ! load_container_id; then
    die 1 'Docker did not return an owned container ID'
  fi
  log_info 'started the isolated InterBase container'
}

load_container_id() {
  local candidate=''
  if [[ ! -r "${CID_FILE}" ]]; then
    return 1
  fi
  if ! candidate="$(<"${CID_FILE}")"; then
    return 1
  fi
  if [[ ! "${candidate}" =~ ^[[:xdigit:]]{12,64}$ ]]; then
    return 1
  fi
  CONTAINER_ID="${candidate}"
}

copy_client_file() {
  local container_path="$1"
  local destination="$2"
  local description="$3"
  if ! run_timed "${DOCKER_OUTPUT}" "${COMMAND_TIMEOUT}" "${DOCKER_BIN}" cp -L "${CONTAINER_ID}:${container_path}" "${destination}"; then
    die 1 "could not copy the ${description} from the owned container"
  fi
  if [[ ! -f "${destination}" || -L "${destination}" || ! -r "${destination}" ]]; then
    die 1 "Docker did not provide a regular ${description} file"
  fi
}

copy_client_tools() {
  copy_client_file /opt/interbase/lib/libgds.so "${LIB_FILE}" 'InterBase client library'
  copy_client_file /opt/interbase/bin/isql "${ISQL_FILE}" 'InterBase isql client'
  [[ -x "${ISQL_FILE}" ]] || die 1 'copied InterBase isql client is not executable'
  log_info 'copied matching client tools into the private temporary root'
}

quote_cgo_flag() {
  local prefix="$1"
  local path="$2"
  if [[ "${path}" != *[[:space:]]* ]]; then
    printf '%s%s' "${prefix}" "${path}"
    return 0
  fi
  if [[ "${path}" == *"'"* ]]; then
    [[ "${path}" != *'"'* ]] || return 2
    printf '"%s%s"' "${prefix}" "${path}"
  elif [[ "${path}" == *'"'* ]]; then
    printf "'%s%s'" "${prefix}" "${path}"
  else
    printf '"%s%s"' "${prefix}" "${path}"
  fi
}

build_test_binary() {
  local output_file="$1"
  local package_name="$2"
  local quoted_include=''
  local quoted_library=''
  local quoted_rpath=''

  quoted_include="$(quote_cgo_flag '-I' "${INTERBASE_INCLUDE}")"
  quoted_library="$(quote_cgo_flag '-L' "${TEST_ROOT}")"
  quoted_rpath="$(quote_cgo_flag '-Wl,-rpath,' "${TEST_ROOT}")"

  log_info "building ${package_name} integration test binary on the host"
  : >"${GO_OUTPUT}"
  if ! (
    scrub_operational_environment
    unset GOFLAGS GOTOOLCHAIN CGO_ENABLED LD_LIBRARY_PATH LD_PRELOAD DYLD_INSERT_LIBRARIES
    export CGO_ENABLED=1
    export GOTOOLCHAIN=local
    export CGO_CFLAGS="${quoted_include}${CGO_CFLAGS:+ ${CGO_CFLAGS}}"
    export CGO_LDFLAGS="${quoted_library} ${quoted_rpath}${CGO_LDFLAGS:+ ${CGO_LDFLAGS}}"
    exec timeout --signal=TERM --kill-after=5s "${COMMAND_TIMEOUT}" \
      "${GO_BIN}" test -tags=integration "${GO_BUILD_ARGS[@]}" -c "${package_name}" -o "${output_file}"
  ) >"${GO_OUTPUT}" 2>&1; then
    print_sanitized_file "${GO_OUTPUT}" >&2
    die 1 "could not build ${package_name} integration test binary"
  fi
  if [[ ! -f "${output_file}" || -L "${output_file}" || ! -x "${output_file}" ]]; then
    die 1 "Go did not produce a regular executable for ${package_name}"
  fi
}

build_test_binaries() {
  build_test_binary "${INTEGRATION_BIN}" ./integration
  build_test_binary "${SERVICES_BIN}" ./services
}

translate_test_arguments() {
  TEST_ARGS=()
  while (($# > 0)); do
    case "$1" in
      -test.*)
        TEST_ARGS+=("$1")
        ;;
      -run=* | -count=* | -timeout=* | -bench=* | -benchtime=* | -cpu=* | -list=* | -shuffle=* | -skip=*)
        TEST_ARGS+=("-test.${1#-}")
        ;;
      -blockprofile=* | -coverprofile=* | -memprofile=* | -memprofilerate=* | \
      -mutexprofile=* | -outputdir=* | -trace=*)
        TEST_ARGS+=("-test.${1#-}")
        ;;
      -benchmem=* | -failfast=* | -paniconexit0=* | -short=* | -v=*)
        TEST_ARGS+=("-test.${1#-}")
        ;;
      -benchmem | -failfast | -paniconexit0 | -short | -v)
        TEST_ARGS+=("-test.${1#-}")
        ;;
      -run | -count | -timeout | -bench | -benchtime | -cpu | -list | -shuffle | -skip | \
      -blockprofile | -blockprofilerate | -coverprofile | -memprofile | -memprofilerate | \
      -mutexprofile | -mutexprofilefraction | -outputdir | -trace)
        local flag="$1"
        shift
        (($# > 0)) || return 2
        TEST_ARGS+=("-test.${flag#-}" "$1")
        ;;
      *)
        TEST_ARGS+=("$1")
        ;;
    esac
    shift
  done
}

print_sanitized_file() {
  local file="$1"
  local line=''
  while IFS= read -r line || [[ -n "${line}" ]]; do
    line="${line//${SERVER_PASSWORD}/[redacted-password]}"
    line="${line//${INVALID_PASSWORD}/[redacted-password]}"
    line="${line//${SERVER_USER}/[redacted-user]}"
    printf '%s\n' "${line}"
  done <"${file}"
}

run_go_test() {
  local output_file="$1"
  local password_kind="$2"
  local duration="$3"
  local package_name="$4"
  local include_workload_env="$5"
  local password=''
  local test_binary=''
  local status=0
  shift 5

  case "${password_kind}" in
    correct) password="${SERVER_PASSWORD}" ;;
    wrong) password="${INVALID_PASSWORD}" ;;
    *) return 2 ;;
  esac
  case "${package_name}" in
    ./integration) test_binary="${INTEGRATION_BIN}" ;;
    ./services) test_binary="${SERVICES_BIN}" ;;
    *) die 2 "unsupported test package for the Docker runner: ${package_name}" ;;
  esac
  [[ -x "${test_binary}" ]] || die 1 "test binary is unavailable: ${test_binary}"
  translate_test_arguments "$@" || die 2 'invalid Go test arguments for the Docker test binary'
  : >"${output_file}"

  local -a docker_exec_args=(
    --env LD_LIBRARY_PATH
    --env TMPDIR
    --env INTERBASE_TEST_ISQL
    --env INTERBASE_TEST_SERVER
    --env INTERBASE_TEST_USER
    --env INTERBASE_TEST_PASSWORD
    --env SERVICES_NATIVE_TRACE
  )
  if [[ "${include_workload_env}" == 1 ]]; then
    docker_exec_args+=("${REQUESTED_WORKLOAD_ENV_ARGS[@]}")
  elif [[ "${include_workload_env}" != 0 ]]; then
    return 2
  fi

  (
    scrub_operational_environment
    unset GOFLAGS GOTOOLCHAIN CGO_ENABLED LD_LIBRARY_PATH LD_PRELOAD DYLD_INSERT_LIBRARIES
    export LD_LIBRARY_PATH="${TEST_ROOT}:/opt/interbase/lib"
    export TMPDIR="${TEST_ROOT}"
    export INTERBASE_TEST_ISQL="${ISQL_FILE}"
    export INTERBASE_TEST_SERVER='localhost/3050'
    export INTERBASE_TEST_USER="${SERVER_USER}"
    export INTERBASE_TEST_PASSWORD="${password}"
    exec timeout --signal=TERM --kill-after=5s "${duration}" "${DOCKER_BIN}" exec \
      "${docker_exec_args[@]}" "${CONTAINER_ID}" "${test_binary}" "${TEST_ARGS[@]}"
  ) >"${output_file}" 2>&1 &
  ACTIVE_PID=$!
  if wait "${ACTIVE_PID}"; then
    status=0
  else
    status=$?
  fi
  ACTIVE_PID=0
  return "${status}"
}

has_authentication_diagnostic() {
  local output=''
  output="$(<"${GO_OUTPUT}")"
  [[ "${output}" =~ ${AUTH_FAILURE_REGEX} ]]
}

run_readiness_checks() {
  local attempt=0
  local ready=0
  local last_status=1
  local wrong_status=0

  SECONDS=0
  while ((attempt < READINESS_MAX_ATTEMPTS && SECONDS < READINESS_DEADLINE_SECONDS)); do
    attempt=$((attempt + 1))
    log_info "checking fixture SQL readiness (attempt ${attempt}/${READINESS_MAX_ATTEMPTS})"
    if run_go_test "${GO_OUTPUT}" correct "${READINESS_COMMAND_TIMEOUT}" ./integration 0 -run '^TestReadFixtureSmoke$' -count=1 -timeout="${READINESS_TEST_TIMEOUT}"; then
      ready=1
      break
    else
      last_status=$?
    fi
    if ((attempt < READINESS_MAX_ATTEMPTS && SECONDS < READINESS_DEADLINE_SECONDS)); then
      sleep "${READINESS_DELAY}"
    fi
  done

  if ((ready == 0)); then
    log_error "fixture SQL readiness failed after ${attempt} attempt(s); last error:"
    print_sanitized_file "${GO_OUTPUT}" >&2
    die "${last_status}" 'fixture SQL readiness did not complete before its deadline'
  fi

  log_info 'checking that an invalid password is rejected'
  if run_go_test "${GO_OUTPUT}" wrong "${READINESS_COMMAND_TIMEOUT}" ./integration 0 -run '^TestReadFixtureSmoke$' -count=1 -timeout="${READINESS_TEST_TIMEOUT}"; then
    die 1 'invalid password was accepted by the isolated InterBase server'
  else
    wrong_status=$?
  fi
  if ((wrong_status == 124 || wrong_status == 137 || wrong_status == 143)); then
    die 1 'invalid-password readiness check timed out'
  fi
  if ! has_authentication_diagnostic; then
    log_error 'invalid-password readiness output did not contain an expected authentication diagnostic:'
    print_sanitized_file "${GO_OUTPUT}" >&2
    die 1 'invalid-password readiness check did not report an authentication failure'
  fi
}

run_requested_tests() {
  local status=0
  if run_go_test "${GO_OUTPUT}" correct "${REQUESTED_COMMAND_DURATION}" ./integration 1 "${FULL_GO_ARGS[@]}"; then
    status=0
  else
    status=$?
  fi
  print_sanitized_file "${GO_OUTPUT}"
  return "${status}"
}

run_native_service_frame_check() {
  log_info 'checking native Services output framing'
  if run_go_test "${GO_OUTPUT}" correct "${COMMAND_TIMEOUT}" ./services 0 \
    -run '^TestNativeService(OutputUsesLengthDelimitedFrame|JobReturnsCleanOutputAndCanBeReused)$' \
    -count=1 -timeout="${READINESS_TEST_TIMEOUT}"; then
    return 0
  fi
  print_sanitized_file "${GO_OUTPUT}" >&2
  die 1 'native Services output framing check failed'
}

cleanup_container() {
  local stop_status=0
  local remove_status=0
  if run_timed "${DOCKER_OUTPUT}" "${COMMAND_TIMEOUT}" "${DOCKER_BIN}" stop -- "${CONTAINER_ID}"; then
    :
  else
    stop_status=$?
  fi
  if run_timed "${DOCKER_OUTPUT}" "${COMMAND_TIMEOUT}" "${DOCKER_BIN}" rm --force -- "${CONTAINER_ID}"; then
    :
  else
    remove_status=$?
  fi
  if ((stop_status != 0)); then
    log_error 'could not stop the owned Docker container during cleanup'
  fi
  if ((remove_status != 0)); then
    log_error 'could not remove the owned Docker container during cleanup'
  fi
  ((stop_status == 0 && remove_status == 0))
}

cleanup_files() {
  local file=''
  local failed=0
  for file in "${OWNED_FILES[@]}"; do
    [[ -n "${file}" ]] || continue
    if [[ -e "${file}" || -L "${file}" ]]; then
      if ! rm -f -- "${file}" 2>/dev/null; then
        log_error 'could not remove an owned temporary file during cleanup'
        failed=1
      fi
    fi
  done
  return "${failed}"
}

cleanup() {
  local primary_status="$?"
  local cleanup_status=0

  trap - ERR EXIT HUP INT TERM
  trap '' HUP INT TERM

  if [[ -z "${CONTAINER_ID}" ]] && ((CONTAINER_START_ATTEMPTED == 1)); then
    load_container_id || true
  fi
  if [[ -n "${CONTAINER_ID}" ]]; then
    cleanup_container || cleanup_status=1
  elif ((CONTAINER_START_ATTEMPTED == 1)); then
    log_error 'owned container ID was unavailable; no unsafe name-based cleanup was attempted'
    cleanup_status=1
  fi
  cleanup_files || cleanup_status=1

  if [[ -n "${TEST_ROOT}" ]]; then
    if [[ -L "${TEST_ROOT}" ]]; then
      log_error 'private temporary root became a symlink; refusing to remove it'
      cleanup_status=1
    elif [[ -d "${TEST_ROOT}" ]]; then
      if ! rmdir -- "${TEST_ROOT}" 2>/dev/null; then
        log_error "private temporary root retained at ${TEST_ROOT}; inspect unexpected fixture artifacts"
        cleanup_status=1
      fi
    elif [[ -e "${TEST_ROOT}" ]]; then
      log_error 'private temporary root was replaced; refusing unsafe cleanup'
      cleanup_status=1
    fi
  fi

  if ((cleanup_status != 0)); then
    if ((primary_status == 0)); then
      primary_status=1
    fi
    log_error 'cleanup failed'
  fi
  exit "${primary_status}"
}

handle_signal() {
  local status="$1"
  if ((ACTIVE_PID > 0)); then
    kill -TERM -- "-${ACTIVE_PID}" 2>/dev/null || true
    kill -KILL -- "-${ACTIVE_PID}" 2>/dev/null || true
    kill -TERM -- "${ACTIVE_PID}" 2>/dev/null || true
    kill -KILL -- "${ACTIVE_PID}" 2>/dev/null || true
    wait "${ACTIVE_PID}" 2>/dev/null || true
    ACTIVE_PID=0
  fi
  log_error 'received signal; stopping the owned container'
  exit "${status}"
}

trap cleanup EXIT
trap 'handle_signal 129' HUP
trap 'handle_signal 130' INT
trap 'handle_signal 143' TERM

main() {
  parse_arguments "$@"
  split_go_arguments "${GO_ARGS[@]}"
  validate_workload_options
  configure_requested_deadlines
  build_requested_workload_environment
  scrub_operational_environment

  require_command docker
  require_command go
  require_command mktemp
  require_command timeout
  require_command sleep
  require_command rm
  require_command rmdir
  DOCKER_BIN="$(command -v docker)"
  GO_BIN="$(command -v go)"

  validate_image
  validate_interbase_include
  validate_temp_parent
  validate_local_image
  create_temp_root
  write_server_environment
  start_container
  copy_client_tools
  if ! cd -- "${REPO_ROOT}"; then
    die 1 "could not change directory to ${REPO_ROOT}"
  fi
  build_test_binaries
  run_readiness_checks
  run_native_service_frame_check

  log_info 'running the requested integration contracts'
  run_requested_tests
}

main "$@"
