#!/usr/bin/env bats

setup() {
	FAULT_RUNNER="${BATS_TEST_DIRNAME}/../../scripts/test-faults-docker.sh"
	FAULT_RUNNER_SANDBOX=''
	FAULT_RUNNER_PID=''
}

teardown() {
	if [[ -n "${FAULT_RUNNER_PID:-}" ]]; then
		kill -TERM "$FAULT_RUNNER_PID" 2>/dev/null || true
		wait "$FAULT_RUNNER_PID" 2>/dev/null || true
	fi
	if [[ -s "${FAULT_CHILD_PID_FILE:-}" ]]; then
		child_pid="$(<"$FAULT_CHILD_PID_FILE")"
		kill -KILL "$child_pid" 2>/dev/null || true
	fi
	if [[ -n "${FAULT_RUNNER_SANDBOX:-}" ]]; then
		rm -rf -- "$FAULT_RUNNER_SANDBOX"
	fi
}

@test "fault runner help requires no Docker image or SDK" {
	run env -u IMAGE -u INTERBASE_INCLUDE bash "$FAULT_RUNNER" --help
	[ "$status" -eq 0 ]
	[[ "$output" == *"INTERBASE_FAULT_TESTS"* ]]
}

@test "fault runner rejects a missing explicit image before Docker work" {
	run env -u IMAGE INTERBASE_INCLUDE=/tmp/opencode/interbase-parity-include bash "$FAULT_RUNNER"
	[ "$status" -eq 2 ]
	[[ "$output" == *"IMAGE is required"* ]]
}

@test "fault runner publishes an explicit host port that survives container restart" {
	FAULT_RUNNER_SANDBOX="$(mktemp -d "${BATS_TEST_TMPDIR:-/tmp}/interbase-go-fault-runner.XXXXXX")"
	fault_sandbox="$FAULT_RUNNER_SANDBOX"
	mkdir -p -- "$fault_sandbox/bin" "$fault_sandbox/include" "$fault_sandbox/tmp"
	printf '%s\n' '/* controlled SDK header */' >"$fault_sandbox/include/ibase.h"
	export FAULT_DOCKER_LOG="$fault_sandbox/docker.log"
	export FAULT_OWNER_TOKEN_FILE="$fault_sandbox/owner-token"

	cat >"$fault_sandbox/bin/docker" <<'EOF'
#!/usr/bin/env bash
set -Eeuo pipefail

printf 'docker' >>"$FAULT_DOCKER_LOG"
for argument in "$@"; do
  printf ' %s' "$argument" >>"$FAULT_DOCKER_LOG"
done
printf '\n' >>"$FAULT_DOCKER_LOG"

case "${1-}" in
  image)
    [[ "${2-}" == inspect ]]
    ;;
  run)
    token=''
    previous=''
    for argument in "$@"; do
      if [[ "$previous" == --label ]]; then
        token="${argument#*=}"
      fi
      previous="$argument"
    done
    printf '%s\n' "$token" >"$FAULT_OWNER_TOKEN_FILE"
    printf '%064d\n' 1
    ;;
  inspect)
    [[ -s "$FAULT_OWNER_TOKEN_FILE" ]]
    cat "$FAULT_OWNER_TOKEN_FILE"
    ;;
  cp)
    source_ref="$3"
    destination="$4"
    case "$source_ref" in
      */opt/interbase/bin/isql)
        printf '#!/usr/bin/env bash\nexit 0\n' >"$destination"
        chmod 700 "$destination"
        ;;
      */opt/interbase/lib/libgds.so)
        printf 'controlled client library\n' >"$destination"
        chmod 600 "$destination"
        ;;
      *)
        exit 34
        ;;
    esac
    ;;
  port)
    printf '%s\n' '127.0.0.1:41000'
    ;;
  exec|rm)
    ;;
  *)
    exit 37
    ;;
esac
EOF
	chmod 700 "$fault_sandbox/bin/docker"

	cat >"$fault_sandbox/bin/go" <<'EOF'
#!/usr/bin/env bash
set -Eeuo pipefail

output_file=''
is_build=0
previous=''
for argument in "$@"; do
  [[ "$argument" == -c ]] && is_build=1
  if [[ "$previous" == -o ]]; then
    output_file="$argument"
  fi
  previous="$argument"
done
if [[ "$is_build" -eq 1 ]]; then
  [[ -n "$output_file" ]]
  printf '#!/usr/bin/env bash\nexit 0\n' >"$output_file"
  chmod 700 "$output_file"
fi
EOF
	chmod 700 "$fault_sandbox/bin/go"

	run env IMAGE=example.invalid/interbase:fixture \
		INTERBASE_INCLUDE="$fault_sandbox/include" TMPDIR="$fault_sandbox/tmp" \
		PATH="$fault_sandbox/bin:$PATH" bash "$FAULT_RUNNER"
	[ "$status" -eq 0 ]

	docker_log="$(<"$FAULT_DOCKER_LOG")"
	[[ "$docker_log" =~ --publish[[:space:]]127\.0\.0\.1:[0-9]+:3050 ]]
	[[ "$docker_log" != *'--publish 127.0.0.1::3050'* ]]
}

@test "fault runner terminates its active child before ownership-checked cleanup on SIGTERM" {
	FAULT_RUNNER_SANDBOX="$(mktemp -d "${BATS_TEST_TMPDIR:-/tmp}/interbase-go-fault-signal.XXXXXX")"
	fault_sandbox="$FAULT_RUNNER_SANDBOX"
	mkdir -p -- "$fault_sandbox/bin" "$fault_sandbox/include" "$fault_sandbox/tmp"
	printf '%s\n' '/* controlled SDK header */' >"$fault_sandbox/include/ibase.h"
	export FAULT_DOCKER_LOG="$fault_sandbox/docker.log"
	export FAULT_OWNER_TOKEN_FILE="$fault_sandbox/owner-token"
	export FAULT_CHILD_STARTED="$fault_sandbox/child.started"
	export FAULT_CHILD_PID_FILE="$fault_sandbox/child.pid"
	export FAULT_CHILD_TERM="$fault_sandbox/child.term"

	cat >"$fault_sandbox/bin/docker" <<'EOF'
#!/usr/bin/env bash
set -Eeuo pipefail

printf 'docker' >>"$FAULT_DOCKER_LOG"
for argument in "$@"; do
  printf ' %s' "$argument" >>"$FAULT_DOCKER_LOG"
done
printf '\n' >>"$FAULT_DOCKER_LOG"

case "${1-}" in
  image)
    [[ "${2-}" == inspect ]]
    ;;
  run)
    token=''
    previous=''
    for argument in "$@"; do
      if [[ "$previous" == --label ]]; then
        token="${argument#*=}"
      fi
      previous="$argument"
    done
    printf '%s\n' "$token" >"$FAULT_OWNER_TOKEN_FILE"
    printf '%064d\n' 1
    ;;
  inspect)
    [[ -s "$FAULT_OWNER_TOKEN_FILE" ]]
    cat "$FAULT_OWNER_TOKEN_FILE"
    ;;
  cp)
    source_ref="$3"
    destination="$4"
    case "$source_ref" in
      */opt/interbase/bin/isql)
        printf '#!/usr/bin/env bash\nexit 0\n' >"$destination"
        chmod 700 "$destination"
        ;;
      */opt/interbase/lib/libgds.so)
        printf 'controlled client library\n' >"$destination"
        chmod 600 "$destination"
        ;;
      *)
        exit 34
        ;;
    esac
    ;;
  exec|rm)
    ;;
  *)
    exit 37
    ;;
esac
EOF
	chmod 700 "$fault_sandbox/bin/docker"

	cat >"$fault_sandbox/bin/go" <<'EOF'
#!/usr/bin/env bash
set -Eeuo pipefail

output_file=''
previous=''
is_build=0
for argument in "$@"; do
  [[ "$argument" == -c ]] && is_build=1
  if [[ "$previous" == -o ]]; then
    output_file="$argument"
  fi
  previous="$argument"
done

if [[ "$is_build" -eq 1 ]]; then
  [[ -n "$output_file" ]]
  cat >"$output_file" <<'SCRIPT'
#!/usr/bin/env bash
set -Eeuo pipefail

printf '%s\n' "$BASHPID" >"$FAULT_CHILD_PID_FILE"
: >"$FAULT_CHILD_STARTED"
trap ': >"$FAULT_CHILD_TERM"; exit 143' TERM INT HUP
while :; do
  sleep 1
done
SCRIPT
  chmod 700 "$output_file"
fi
EOF
	chmod 700 "$fault_sandbox/bin/go"

	: >"$FAULT_DOCKER_LOG"
	env IMAGE=example.invalid/interbase:fixture \
		INTERBASE_INCLUDE="$fault_sandbox/include" TMPDIR="$fault_sandbox/tmp" \
		PATH="$fault_sandbox/bin:$PATH" \
		FAULT_DOCKER_LOG="$FAULT_DOCKER_LOG" \
		FAULT_OWNER_TOKEN_FILE="$FAULT_OWNER_TOKEN_FILE" \
		FAULT_CHILD_STARTED="$FAULT_CHILD_STARTED" \
		FAULT_CHILD_PID_FILE="$FAULT_CHILD_PID_FILE" \
		FAULT_CHILD_TERM="$FAULT_CHILD_TERM" \
		bash "$FAULT_RUNNER" >"$fault_sandbox/runner.log" 2>&1 &
	FAULT_RUNNER_PID=$!
	for _ in {1..200}; do
		[[ -f "$FAULT_CHILD_STARTED" ]] && break
		sleep 0.01
	done
	[ -f "$FAULT_CHILD_STARTED" ]

	kill -TERM "$FAULT_RUNNER_PID"
	if wait "$FAULT_RUNNER_PID"; then
		status=0
	else
		status=$?
	fi
	[ "$status" -eq 143 ]

	child_pid="$(<"$FAULT_CHILD_PID_FILE")"
	for _ in {1..200}; do
		if ! kill -0 "$child_pid" 2>/dev/null; then
			break
		fi
		sleep 0.01
	done
	! kill -0 "$child_pid" 2>/dev/null

	docker_log="$(<"$FAULT_DOCKER_LOG")"
	[[ "$docker_log" == *"docker inspect --format"* ]]
	[[ "$docker_log" == *"docker rm --force -- 0000000000000000000000000000000000000000000000000000000000000001"* ]]
}
