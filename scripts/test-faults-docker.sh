#!/usr/bin/env bash
# Run opt-in TCP fault contracts against one disposable, locally owned image.
set -Eeuo pipefail
IFS=$'\n\t'
umask 077

readonly IMAGE="${IMAGE:-}"
readonly INTERBASE_INCLUDE="${INTERBASE_INCLUDE:-}"
TOKEN="$(date +%s)-${RANDOM}-${RANDOM}"
readonly TOKEN
readonly FAULT_TEST_PATTERN="${INTERBASE_FAULT_TEST_PATTERN:-^TestFault}"
readonly SERVER_BIND='127.0.0.1'
readonly SERVER_PORT='3050'
readonly HOST_PORT_BASE=20000
readonly HOST_PORT_RANGE=40000
readonly HOST_PORT_ATTEMPTS=20
ROOT=''
CONTAINER_ID=''
CONTAINER_NAME="interbase-go-fault-${TOKEN}"
CONTAINER_START_ATTEMPTED=0
ACTIVE_CHILD_PID=0
PORT=''

usage() {
	printf '%s\n' \
		'Usage: IMAGE=interbase-go-parity-test:local INTERBASE_INCLUDE=/path/to/sdk ./scripts/test-faults-docker.sh' \
		'' \
		'Runs opt-in INTERBASE_FAULT_TESTS=1 contracts against one uniquely labelled' \
		'private Docker container. Only that labelled container is removed during cleanup.'
}

die() {
	printf 'ERROR: %s\n' "$*" >&2
	exit 2
}

container_is_owned() {
	local container_ref="$1"
	local label=''
	if ! label="$(docker inspect --format '{{index .Config.Labels "interbase-go.fault-token"}}' "$container_ref" 2>/dev/null)"; then
		return 1
	fi
	[[ "$label" == "$TOKEN" ]]
}

remove_owned_container() {
	local container_ref="$1"
	if ! container_is_owned "$container_ref"; then
		printf 'ERROR: refusing to remove container with an ownership mismatch: %s\n' "$container_ref" >&2
		return 1
	fi
	docker rm --force -- "$container_ref" >/dev/null 2>&1
}

remove_owned_named_container() {
	remove_owned_container "$CONTAINER_NAME"
}

run_tracked() {
	local status=0

	"$@" &
	ACTIVE_CHILD_PID=$!
	if wait "$ACTIVE_CHILD_PID"; then
		status=0
	else
		status=$?
	fi
	ACTIVE_CHILD_PID=0
	return "$status"
}

terminate_active_child() {
	local child_pid="$ACTIVE_CHILD_PID"
	if ((child_pid <= 0)); then
		return 0
	fi

	kill -TERM -- "-$child_pid" 2>/dev/null || true
	kill -KILL -- "-$child_pid" 2>/dev/null || true
	kill -TERM -- "$child_pid" 2>/dev/null || true
	kill -KILL -- "$child_pid" 2>/dev/null || true
	wait "$child_pid" 2>/dev/null || true
	ACTIVE_CHILD_PID=0
}

cleanup() {
	local status=$?
	trap - EXIT HUP INT TERM
	trap '' HUP INT TERM
	terminate_active_child
	if [[ -z "$CONTAINER_ID" && "$CONTAINER_START_ATTEMPTED" -eq 1 ]]; then
		remove_owned_named_container || status=1
	fi
	if [[ -n "$CONTAINER_ID" ]]; then
		if container_is_owned "$CONTAINER_ID"; then
			docker exec "$CONTAINER_ID" rm -rf -- "$ROOT/server-state" >/dev/null 2>&1 || true
			if ! docker rm --force -- "$CONTAINER_ID" >/dev/null 2>&1; then
				printf 'ERROR: retained owned fault container %s\n' "$CONTAINER_ID" >&2
				status=1
			fi
		else
			printf 'ERROR: refusing to clean up container with an ownership mismatch: %s\n' "$CONTAINER_ID" >&2
			status=1
		fi
	fi
	if [[ -n "$ROOT" && -d "$ROOT" && ! -L "$ROOT" ]]; then
		rm -rf -- "$ROOT"
	fi
	exit "$status"
}

handle_signal() {
	local status="$1"
	terminate_active_child
	exit "$status"
}

if [[ "${1:-}" == '--help' || "${1:-}" == '-h' ]]; then
	usage
	exit 0
fi
[[ $# -eq 0 ]] || die 'this runner accepts no positional arguments'
[[ -n "$IMAGE" ]] || die 'IMAGE is required and must name an existing local Docker image'
[[ "$IMAGE" =~ ^[[:alnum:]][[:alnum:]_.:/@-]*$ ]] || die 'IMAGE contains an invalid Docker image reference'
[[ -n "$INTERBASE_INCLUDE" && "$INTERBASE_INCLUDE" == /* && -f "$INTERBASE_INCLUDE/ibase.h" ]] || die 'INTERBASE_INCLUDE must contain ibase.h'
for command in docker go timeout mktemp; do command -v "$command" >/dev/null || die "required command is unavailable: $command"; done
docker image inspect "$IMAGE" >/dev/null 2>&1 || die 'IMAGE must name an existing local Docker image; no pull was attempted'

ROOT="$(mktemp -d "${TMPDIR:-/tmp}/interbase-go-fault.XXXXXX")"
trap cleanup EXIT
trap 'handle_signal 129' HUP
trap 'handle_signal 130' INT
trap 'handle_signal 143' TERM
mkdir -p -- "$ROOT/server-state"
for ((attempt=1; attempt<=HOST_PORT_ATTEMPTS; attempt++)); do
	PORT=$((HOST_PORT_BASE + RANDOM % HOST_PORT_RANGE))
	CONTAINER_START_ATTEMPTED=1
	if run_tracked docker run --pull=never --detach --name "$CONTAINER_NAME" \
		--label "interbase-go.fault-token=$TOKEN" \
		--publish "${SERVER_BIND}:${PORT}:${SERVER_PORT}" \
		--mount "type=bind,source=$ROOT,target=$ROOT" \
		--mount "type=bind,source=$ROOT/server-state,target=/var/lib/interbase" \
		-e IB_SYSDBA_USER=SYSDBA -e IB_SYSDBA_PASSWORD=masterkey \
		-e IB_DATA_DIR=/var/lib/interbase/data -e IB_BACKUP_DIR=/var/lib/interbase/backups -e IB_LOG_DIR=/var/lib/interbase/logs \
		"$IMAGE" >"$ROOT/cid" 2>"$ROOT/docker-run-error"; then
		candidate_id="$(<"$ROOT/cid")"
		[[ "$candidate_id" =~ ^[[:xdigit:]]{12,64}$ ]] || die 'Docker did not return an owned container ID'
		CONTAINER_ID="$candidate_id"
		break
	fi
	if [[ "$(<"$ROOT/docker-run-error")" != *'port is already allocated'* ]]; then
		printf '%s\n' "$(<"$ROOT/docker-run-error")" >&2
		die 'could not start the owned fault container'
	fi
	remove_owned_named_container || die 'could not remove the owned fault container after a port collision'
done
[[ -n "$CONTAINER_ID" ]] || die 'could not allocate a stable host port for the owned fault container'
run_tracked docker cp -L "$CONTAINER_ID:/opt/interbase/lib/libgds.so" "$ROOT/libgds.so"
run_tracked docker cp -L "$CONTAINER_ID:/opt/interbase/bin/isql" "$ROOT/isql"
chmod 700 "$ROOT/isql"

CGO_CFLAGS="-I$INTERBASE_INCLUDE" CGO_LDFLAGS="-L$ROOT -Wl,-rpath,$ROOT -lgds" \
	run_tracked timeout --signal=TERM --kill-after=5s 60s go test -tags=integration -c ./integration -o "$ROOT/integration.test"
INTERBASE_FAULT_TESTS=1 INTERBASE_FAULT_OWNED_CONTAINER="$CONTAINER_NAME" INTERBASE_FAULT_OWNED_TOKEN="$TOKEN" \
	INTERBASE_TEST_SERVER="127.0.0.1/$PORT" INTERBASE_TEST_USER=SYSDBA INTERBASE_TEST_PASSWORD=masterkey \
	INTERBASE_TEST_ISQL="$ROOT/isql" TMPDIR="$ROOT" LD_LIBRARY_PATH="$ROOT" \
	run_tracked timeout --signal=TERM --kill-after=5s 3m "$ROOT/integration.test" -test.run "$FAULT_TEST_PATTERN" -test.count=1 -test.timeout=150s
