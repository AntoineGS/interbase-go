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

@test "help documents the live workload flags and duration format" {
  run "$RUNNER" --help

  [ "$status" -eq 0 ]
  [[ "$output" == *"--performance"* ]]
  [[ "$output" == *"--soak"* ]]
  [[ "$output" == *"--soak-duration=DURATION"* ]]
  [[ "$output" == *"--soak-workers=WORKERS"* ]]
  [[ "$output" == *"--soak-sample-interval=DURATION"* ]]
  [[ "$output" == *"--native-lifecycle"* ]]
  [[ "$output" == *"positive integer duration"* ]]
}

@test "passes performance opt-in only to the requested test command" {
  run "$RUNNER" --performance -run '^TestRead$'

  [ "$status" -eq 0 ]
  go_log="$(<"$FAKE_GO_LOG")"
  [ "$(grep -c '^workload-env perf= soak= duration= workers= sample= native=$' <<<"$go_log")" -eq 3 ]
  [ "$(grep -c '^workload-env perf=1 soak= duration= workers= sample= native=$' <<<"$go_log")" -eq 1 ]
}

@test "passes soak configuration only to the requested test command" {
  run "$RUNNER" --soak --soak-duration=120s --soak-workers=4 \
    --soak-sample-interval=1s -run '^TestRead$'

  [ "$status" -eq 0 ]
  go_log="$(<"$FAKE_GO_LOG")"
  [ "$(grep -c '^workload-env perf= soak= duration= workers= sample= native=$' <<<"$go_log")" -eq 3 ]
  [ "$(grep -c '^workload-env perf= soak=1 duration=120s workers=4 sample=1s native=$' <<<"$go_log")" -eq 1 ]
}

@test "passes native lifecycle opt-in only to the requested test command" {
  run "$RUNNER" --native-lifecycle -run '^TestRead$'

  [ "$status" -eq 0 ]
  go_log="$(<"$FAKE_GO_LOG")"
  [ "$(grep -c '^workload-env perf= soak= duration= workers= sample= native=$' <<<"$go_log")" -eq 3 ]
  [ "$(grep -c '^workload-env perf= soak= duration= workers= sample= native=1$' <<<"$go_log")" -eq 1 ]
}

@test "supports split workload values and preserves the double-dash Go argument boundary" {
  run "$RUNNER" --soak --soak-duration 2m --soak-workers 1 \
    --soak-sample-interval 2s -- -run '^TestRead$'

  [ "$status" -eq 0 ]
  go_log="$(<"$FAKE_GO_LOG")"
  [ "$(grep -c '^workload-env perf= soak=1 duration=2m workers=1 sample=2s native=$' <<<"$go_log")" -eq 1 ]
  [[ "$go_log" == *'-test.run \^TestRead\$'* ]]
}

@test "separates compiled race flags and translates boolean and profile runtime flags" {
  run "$RUNNER" -race -v=false -benchmem=true -memprofilerate=1 -run '^TestRead$'

  [ "$status" -eq 0 ]
  go_log="$(<"$FAKE_GO_LOG")"
  docker_log="$(<"$FAKE_DOCKER_LOG")"
  root_path="$(runner_root_from_log)"
  [[ "$go_log" == *"go test -tags=integration -race -c ./integration"* ]]
  [[ "$go_log" == *"go test -tags=integration -race -c ./services"* ]]
  [[ "$docker_log" == *"${root_path}/integration.test -test.count=1 -test.timeout=10m -test.v=false -test.benchmem=true -test.memprofilerate=1"* ]]
  [[ "$docker_log" != *"${root_path}/integration.test -race"* ]]
  [[ "$docker_log" != *"${root_path}/integration.test -v=false"* ]]
  [[ "$docker_log" != *"${root_path}/integration.test -benchmem=true"* ]]
  [[ "$docker_log" != *"${root_path}/integration.test -memprofilerate=1"* ]]
}

@test "does not inherit workload opt-ins from the host environment" {
  export INTERBASE_PERF=1
  export INTERBASE_SOAK=1
  export INTERBASE_SOAK_DURATION=24h
  export INTERBASE_SOAK_WORKERS=64
  export INTERBASE_SOAK_SAMPLE_INTERVAL=1s
  export INTERBASE_NATIVE_LIFECYCLE_RACE=1
  export INTERBASE_UNSAFE_WORKLOAD_OVERRIDE=1

  run "$RUNNER" -run '^TestRead$'

  [ "$status" -eq 0 ]
  go_log="$(<"$FAKE_GO_LOG")"
  [ "$(grep -c '^workload-env perf= soak= duration= workers= sample= native=$' <<<"$go_log")" -eq 4 ]
  [[ "$go_log" != *"UNSAFE_WORKLOAD_OVERRIDE"* ]]
}

@test "extends the default Go and external deadlines for a long soak" {
  run "$RUNNER" --soak --soak-duration=24h -run '^TestRead$'

  [ "$status" -eq 0 ]
  go_log="$(<"$FAKE_GO_LOG")"
  [[ "$go_log" == *"go -test.count=1 -test.timeout=86460s"* ]]
  timeout_log="$(<"$FAKE_TIMEOUT_LOG")"
  [[ "$timeout_log" == *" 86520s "*"/integration.test "* ]]
}

@test "rejects a requested Go timeout shorter than the soak duration" {
  run "$RUNNER" --soak --soak-duration=120s -timeout=119s -run '^TestRead$'

  [ "$status" -eq 2 ]
  [[ "$output" == *"-timeout"*"soak duration"* ]]
  [ ! -s "$FAKE_DOCKER_LOG" ]
}

@test "fails closed for an uncomparable subsecond soak timeout" {
  run "$RUNNER" --soak --soak-duration=120s -timeout=500ms -run '^TestRead$'

  [ "$status" -eq 2 ]
  [[ "$output" == *"soak duration"* ]]
  [ ! -s "$FAKE_DOCKER_LOG" ]
}

@test "rejects invalid workload duration and worker values before Docker" {
  run "$RUNNER" --soak --soak-duration=0s
  [ "$status" -eq 2 ]
  [[ "$output" == *"positive integer duration"* ]]
  [ ! -s "$FAKE_DOCKER_LOG" ]

  run "$RUNNER" --soak --soak-duration=25h
  [ "$status" -eq 2 ]
  [[ "$output" == *"24h"* ]]
  [ ! -s "$FAKE_DOCKER_LOG" ]

  run "$RUNNER" --soak --soak-workers=0
  [ "$status" -eq 2 ]
  [[ "$output" == *"between 1 and 64"* ]]
  [ ! -s "$FAKE_DOCKER_LOG" ]

  run "$RUNNER" --soak --soak-workers=65
  [ "$status" -eq 2 ]
  [[ "$output" == *"between 1 and 64"* ]]
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

@test "quotes whitespace-containing SDK and bind paths for compiled binaries" {
  spaced_tmp="${TEST_SANDBOX}/tmp with spaces"
  mkdir -p -- "$spaced_tmp/include"
  printf '%s\n' '/* controlled SDK header */' > "$spaced_tmp/include/ibase.h"
  export TMPDIR="$spaced_tmp"
  export INTERBASE_INCLUDE="$spaced_tmp/include"
  export FAKE_GO_REQUIRE_QUOTED_CGO_PATHS=1

  run "$RUNNER" -run '^TestRead$'

  [ "$status" -eq 0 ]
  root_path="$(runner_root_from_log)"
  go_log="$(<"$FAKE_GO_LOG")"
  [[ "$go_log" == *"build-cflags=\"-I${spaced_tmp}/include\""* ]]
  [[ "$go_log" == *"build-ldflags=\"-L${root_path}\" \"-Wl,-rpath,${root_path}\""* ]]
  [ ! -d "$root_path" ]
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
  root_path="$(runner_root_from_log)"
  [[ "$docker_log" == *"${root_path}/integration.test -test.run"* ]]
  [[ "$docker_log" == *"${root_path}/services.test -test.run"* ]]
  [[ "$go_log" == *"database= user= test-isql=present"* ]]
  [[ "$go_log" == *"build-cflags=-I${TEST_TMP}/include"* ]]
  [[ "$go_log" == *"build-ldflags=-L${root_path} -Wl,-rpath,${root_path}"* ]]
  [[ "$go_log" == *"server=localhost/3050"* ]]
  [[ "$go_log" == *"test-isql-path=owned"* ]]
  [[ "$go_log" == *"password-state=default"* ]]
  [[ "$go_log" == *"password-state=wrong"* ]]
  [[ "$go_log" == *'-test.run \^TestRead\$'* ]]
  [ ! -d "$root_path" ]
}

@test "does not put credentials into the timeout command arguments" {
  run "$RUNNER" -run '^TestRead$'

  [ "$status" -eq 0 ]
  timeout_log="$(<"$FAKE_TIMEOUT_LOG")"
  [[ "$timeout_log" != *"masterkey"* ]]
  [[ "$timeout_log" != *"operational-password"* ]]
  [[ "$timeout_log" != *" env -i "* ]]
  [[ "$timeout_log" == *"/docker exec "* ]]
}

@test "retries only readiness and never retries requested tests" {
  export FAKE_GO_READINESS_FAILURES=2

  run "$RUNNER" -run '^TestRead$'

  [ "$status" -eq 0 ]
  [ "$(grep -c '^readiness-default$' "$FAKE_GO_PHASE_LOG")" -eq 3 ]
  [ "$(grep -c '^readiness-wrong$' "$FAKE_GO_PHASE_LOG")" -eq 1 ]
  [ "$(grep -c '^service-precheck$' "$FAKE_GO_PHASE_LOG")" -eq 1 ]
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
