#!/usr/bin/env bats
# shellcheck disable=SC2030,SC2031,SC2154 # Bats isolates each test in a subshell.

load test_helper

setup() {
  setup_runner_test
}

teardown() {
  teardown_runner_test
}

@test "requires an explicit image without invoking Docker" {
  unset IMAGE

  run "$RUNNER"

  [ "$status" -eq 2 ]
  [[ "$output" == *"IMAGE"* ]]
  [ ! -s "$FAKE_DOCKER_LOG" ]
}

@test "rejects an image that is not already present locally" {
  export FAKE_DOCKER_INSPECT_FAIL=1

  run "$RUNNER"

  [ "$status" -eq 2 ]
  [[ "$output" == *"existing local Docker image"* ]]
  [[ "$(<"$FAKE_DOCKER_LOG")" == *"docker image inspect"* ]]
  [[ "$(<"$FAKE_DOCKER_LOG")" != *"docker run "* ]]
}

@test "rejects a comma in TMPDIR before invoking Docker" {
  comma_tmp="${TEST_SANDBOX}/tmp,unsafe"
  mkdir -p -- "$comma_tmp"
  export TMPDIR="$comma_tmp"

  run "$RUNNER"

  [ "$status" -eq 2 ]
  [[ "$output" == *"colons or commas"* ]]
  [ ! -s "$FAKE_DOCKER_LOG" ]
}

@test "cleans up an owned container when startup fails after assigning its CID" {
  export FAKE_DOCKER_RUN_FAIL=1

  run "$RUNNER"

  [ "$status" -ne 0 ]
  [[ "$output" == *"could not start"* ]]
  docker_log="$(<"$FAKE_DOCKER_LOG")"
  [[ "$docker_log" == *"docker stop -- 0000000000000000000000000000000000000000000000000000000000000001"* ]]
  [[ "$docker_log" == *"docker rm"*"0000000000000000000000000000000000000000000000000000000000000001"* ]]
}

@test "uses isolated Docker flags, performs SQL readiness, and preserves Go args" {
  run "$RUNNER" -run '^TestRead$'

  [ "$status" -eq 0 ]
  [[ "$output" == *"controlled go test success"* ]]
  docker_log="$(<"$FAKE_DOCKER_LOG")"
  go_log="$(<"$FAKE_GO_LOG")"
  [[ "$docker_log" == *"--pull=never"* ]]
  [[ "$docker_log" == *"--publish 127.0.0.1:3050:3050"* ]]
  [[ "$docker_log" == *"type=tmpfs,destination=/var/lib/interbase"* ]]
  [[ "$docker_log" == *"type=bind,source=$TMPDIR/interbase-go-docker."* ]]
  [[ "$docker_log" == *"target=$TMPDIR/interbase-go-docker."* ]]
  [[ "$docker_log" == *"/opt/interbase/lib/libgds.so"* ]]
  [[ "$docker_log" == *"/opt/interbase/bin/isql"* ]]
  [[ "$docker_log" == *"server-password=default"* ]]
  [[ "$docker_log" == *"backups=disabled"* ]]
  [[ "$docker_log" == *"docker stop -- 0000000000000000000000000000000000000000000000000000000000000001"* ]]
  [[ "$docker_log" == *"docker rm"*"0000000000000000000000000000000000000000000000000000000000000001"* ]]
  [[ "$docker_log" == *"host-operational-vars=absent"* ]]
  [[ "$go_log" == *"database= user= test-isql=present"* ]]
  [[ "$go_log" == *"test-isql-path=owned"* ]]
  [[ "$go_log" == *"password-state=default"* ]]
  [[ "$go_log" == *"password-state=wrong"* ]]
  [[ "$go_log" == *'-run \^TestRead\$'* ]]
  [ ! -d "$(runner_root_from_log)" ]
}

@test "does not put credentials into the timeout command arguments" {
  run "$RUNNER" -run '^TestRead$'

  [ "$status" -eq 0 ]
  timeout_log="$(<"$FAKE_TIMEOUT_LOG")"
  [[ "$timeout_log" != *"masterkey"* ]]
  [[ "$timeout_log" != *"INTERBASE_TEST_PASSWORD"* ]]
  [[ "$timeout_log" != *" env -i "* ]]
  [[ "$timeout_log" == *" test -tags=integration "* ]]
}

@test "retries only readiness and never retries requested tests" {
  export FAKE_GO_READINESS_FAILURES=2

  run "$RUNNER" -run '^TestRead$'

  [ "$status" -eq 0 ]
  [ "$(grep -c '^readiness-default$' "$FAKE_GO_PHASE_LOG")" -eq 3 ]
  [ "$(grep -c '^readiness-wrong$' "$FAKE_GO_PHASE_LOG")" -eq 1 ]
  [ "$(grep -c '^requested$' "$FAKE_GO_PHASE_LOG")" -eq 1 ]
}

@test "reports a sanitized final readiness error without running requested tests" {
  export FAKE_GO_READINESS_MODE=always-fail

  run "$RUNNER" -run '^TestRead$'

  [ "$status" -ne 0 ]
  [[ "$output" == *"last error"* ]]
  [[ "$output" == *"[redacted-password]"* ]]
  [[ "$output" != *"masterkey"* ]]
  [ "$(grep -c '^requested$' "$FAKE_GO_PHASE_LOG")" -eq 0 ]
}

@test "does not treat generic connection errors as authentication rejection" {
  export FAKE_GO_WRONG_ERROR_MODE=generic

  run "$RUNNER" -run '^TestRead$'

  [ "$status" -ne 0 ]
  [[ "$output" == *"did not report an authentication failure"* ]]
  [ "$(grep -c '^requested$' "$FAKE_GO_PHASE_LOG")" -eq 0 ]
}

@test "accepts the native authentication diagnostic" {
  export FAKE_GO_WRONG_ERROR_MODE=code

  run "$RUNNER" -run '^TestRead$'

  [ "$status" -eq 0 ]
  [ "$(grep -c '^requested$' "$FAKE_GO_PHASE_LOG")" -eq 1 ]
}

@test "returns the requested Go test failure status after cleanup" {
  export FAKE_GO_FAIL_MODE=requested

  run "$RUNNER" -run '^TestWrite$'

  [ "$status" -eq 23 ]
  [[ "$output" == *"controlled test failure"* ]]
  [[ "$output" != *"masterkey"* ]]
  [ ! -d "$(runner_root_from_log)" ]
}

@test "reports cleanup failure without deleting retained fixture artifacts" {
  export FAKE_GO_CREATE_RETAINED=1

  run "$RUNNER" -run '^TestRead$'

  [ "$status" -ne 0 ]
  [[ "$output" == *"cleanup"* ]]
  retained_root="$(runner_root_from_log)"
  [ -f "$retained_root/retained-fixture/database.ib" ]
  [ -d "$retained_root" ]
}

@test "reports a container removal failure" {
  export FAKE_DOCKER_RM_FAIL=1

  run "$RUNNER" -run '^TestRead$'

  [ "$status" -ne 0 ]
  [[ "$output" == *"remove the owned Docker container"* ]]
}

@test "ignores signals during cleanup and preserves the original test status" {
  export FAKE_GO_FAIL_MODE=requested
  export FAKE_DOCKER_STOP_SLEEP=1

  "$RUNNER" -run '^TestWrite$' >"${TEST_SANDBOX}/cleanup-signal.log" 2>&1 &
  RUNNER_PID=$!
  for _ in {1..100}; do
    [ -f "$FAKE_DOCKER_STOP_STARTED" ] && break
    sleep 0.01
  done
  [ -f "$FAKE_DOCKER_STOP_STARTED" ]

  kill -TERM "$RUNNER_PID"
  set +e
  wait "$RUNNER_PID"
  status=$?
  set -e

  [ "$status" -eq 23 ]
  docker_log="$(<"$FAKE_DOCKER_LOG")"
  [[ "$docker_log" == *"docker rm"*"0000000000000000000000000000000000000000000000000000000000000001"* ]]
  [ ! -d "$(runner_root_from_log)" ]
  unset RUNNER_PID
}

@test "stops and removes only the owned container when interrupted" {
  export FAKE_GO_SLEEP=1

  "$RUNNER" >"${TEST_SANDBOX}/signal.log" 2>&1 &
  RUNNER_PID=$!
  for _ in {1..100}; do
    [ -f "$FAKE_GO_STARTED" ] && break
    sleep 0.01
  done
  [ -f "$FAKE_GO_STARTED" ]

  kill -TERM "$RUNNER_PID"
  set +e
  wait "$RUNNER_PID"
  status=$?
  set -e

  [ "$status" -eq 143 ]
  docker_log="$(<"$FAKE_DOCKER_LOG")"
  [[ "$docker_log" == *"docker stop -- 0000000000000000000000000000000000000000000000000000000000000001"* ]]
  [[ "$docker_log" == *"docker rm"*"0000000000000000000000000000000000000000000000000000000000000001"* ]]
  [ ! -d "$(runner_root_from_log)" ]
  unset RUNNER_PID
}
