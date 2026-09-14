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
readonly AUTH_FAILURE_REGEX='(335544472|SQLSTATE[[:space:]]*[:=][[:space:]]*28000|[Yy]our[[:space:]]+[Uu]ser[[:space:]]+name[[:space:]]+and[[:space:]]+[Pp]assword[[:space:]]+are[[:space:]]+not[[:space:]]+defined)'

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)"
readonly SCRIPT_DIR
REPO_ROOT="$(cd -- "${SCRIPT_DIR}/.." && pwd -P)"
readonly REPO_ROOT

IMAGE="${IMAGE:-}"
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
declare -a GO_ARGS=()
declare -a FULL_GO_ARGS=()
declare -a OWNED_FILES=()

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
    '' \
    'Runner options:' \
    '  -h, --help    Show this help.' \
    '      --        End runner options; pass the rest to go test.' \
    '' \
    'Defaults for the requested run are -count=1 -timeout=10m.' \
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
  FULL_GO_ARGS=(-count=1 -timeout="${REQUESTED_TEST_TIMEOUT}" "${GO_ARGS[@]}")
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
        unset "${name}"
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
  CONTAINER_NAME="interbase-go-test-${TEST_ROOT##*/}"
  OWNED_FILES=("${CID_FILE}" "${ENV_FILE}" "${DOCKER_OUTPUT}" "${GO_OUTPUT}" "${ISQL_FILE}" "${LIB_FILE}")
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
  local password=''
  local status=0
  shift 3

  case "${password_kind}" in
    correct) password="${SERVER_PASSWORD}" ;;
    wrong) password="${INVALID_PASSWORD}" ;;
    *) return 2 ;;
  esac
  : >"${output_file}"

  (
    scrub_operational_environment
    unset GOFLAGS GOTOOLCHAIN CGO_ENABLED LD_LIBRARY_PATH LD_PRELOAD DYLD_INSERT_LIBRARIES
    export CGO_ENABLED=1
    export GOTOOLCHAIN=local
    export LD_LIBRARY_PATH="${TEST_ROOT}:/opt/interbase/lib"
    export TMPDIR="${TEST_ROOT}"
    export INTERBASE_TEST_ISQL="${ISQL_FILE}"
    export INTERBASE_TEST_USER="${SERVER_USER}"
    export INTERBASE_TEST_PASSWORD="${password}"
    exec timeout --signal=TERM --kill-after=5s "${duration}" "${GO_BIN}" test -tags=integration ./integration "$@"
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
    if run_go_test "${GO_OUTPUT}" correct "${READINESS_COMMAND_TIMEOUT}" -run '^TestReadFixtureSmoke$' -count=1 -timeout="${READINESS_TEST_TIMEOUT}"; then
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
  if run_go_test "${GO_OUTPUT}" wrong "${READINESS_COMMAND_TIMEOUT}" -run '^TestReadFixtureSmoke$' -count=1 -timeout="${READINESS_TEST_TIMEOUT}"; then
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
  if run_go_test "${GO_OUTPUT}" correct "${REQUESTED_COMMAND_TIMEOUT}" "${FULL_GO_ARGS[@]}"; then
    status=0
  else
    status=$?
  fi
  print_sanitized_file "${GO_OUTPUT}"
  return "${status}"
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
  validate_temp_parent
  validate_local_image
  create_temp_root
  write_server_environment
  start_container
  copy_client_tools
  if ! cd -- "${REPO_ROOT}"; then
    die 1 "could not change directory to ${REPO_ROOT}"
  fi
  run_readiness_checks

  log_info 'running the requested integration contracts'
  run_requested_tests
}

main "$@"
