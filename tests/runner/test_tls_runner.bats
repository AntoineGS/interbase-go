#!/usr/bin/env bats

setup() {
  TLS_SCRIPT="${BATS_TEST_DIRNAME}/../../scripts/test-tls-docker.sh"
}

@test "TLS runner help requires no SDK or Docker daemon" {
  run env -u IMAGE -u INTERBASE_INCLUDE bash "$TLS_SCRIPT" --help
  [ "$status" -eq 0 ]
  [[ "$output" == *"network-none"* ]]
}

@test "TLS runner rejects an absent explicit private image" {
  run env -u IMAGE bash "$TLS_SCRIPT"
  [ "$status" -eq 2 ]
  [[ "$output" == *"IMAGE is required"* ]]
}

@test "TLS runner rejects image flags before Docker execution" {
  run env IMAGE=--privileged bash "$TLS_SCRIPT"
  [ "$status" -eq 2 ]
  [[ "$output" == *"invalid image reference"* ]]
}

@test "TLS runner rejects missing SDK before provisioning resources" {
  run env IMAGE=private:local INTERBASE_INCLUDE=/nonexistent/parity-tls-sdk bash "$TLS_SCRIPT"
  [ "$status" -eq 2 ]
  [[ "$output" == *"INTERBASE_INCLUDE must contain ibase.h"* ]]
}

@test "TLS runner rejects extra arguments instead of running arbitrary tests" {
  run bash "$TLS_SCRIPT" -test.run=TestRead
  [ "$status" -eq 2 ]
  [[ "$output" == *"Usage:"* ]]
}
