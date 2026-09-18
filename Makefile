.PHONY: build test test-native test-runner test-integration-docker test-faults test-soak test-native-lifecycle test-fuzz bench bench-live vet

BATS ?= bats
FUZZ_TIME ?= 10s
FUZZ_TIMEOUT ?= 2m
BENCH_TIME ?= 1s
SOAK_DURATION ?= 120s
SOAK_WORKERS ?= 4
SOAK_SAMPLE_INTERVAL ?= 1s
INTERBASE_INCLUDE ?= /opt/interbase/include
INTERBASE_LIB ?= /opt/interbase/lib
NATIVE_CFLAGS ?= -I$(INTERBASE_INCLUDE)
NATIVE_LDFLAGS ?= -L$(INTERBASE_LIB) -Wl,-rpath,$(INTERBASE_LIB) -lgds
CGO_CFLAGS ?= $(NATIVE_CFLAGS)
CGO_LDFLAGS ?= $(NATIVE_LDFLAGS)

export CGO_CFLAGS CGO_LDFLAGS

build:
	mkdir -p bin
	go build -o bin/ibprobe ./cmd/ibprobe

test:
	go test ./... -count=1 -timeout=60s
	$(MAKE) test-native

test-native:
	mkdir -p bin
	$(CC) -std=c11 -Wall -Wextra -g -O1 -fsanitize=address -fno-omit-frame-pointer $(NATIVE_CFLAGS) tests/native_status_test.c $(NATIVE_LDFLAGS) -o bin/native_status_test
	ASAN_OPTIONS=detect_leaks=1 ./bin/native_status_test
	$(CC) -std=c11 -Wall -Wextra -g -O1 -fsanitize=address -fno-omit-frame-pointer $(NATIVE_CFLAGS) tests/native_values_test.c $(NATIVE_LDFLAGS) -o bin/native_values_test
	ASAN_OPTIONS=detect_leaks=1 ./bin/native_values_test
	$(CC) -std=c11 -Wall -Wextra -g -O1 -fsanitize=address -fno-omit-frame-pointer $(NATIVE_CFLAGS) tests/native_direct_test.c $(NATIVE_LDFLAGS) -o bin/native_direct_test
	ASAN_OPTIONS=detect_leaks=1 ./bin/native_direct_test
	$(CC) -std=c11 -Wall -Wextra -g -O1 -fsanitize=address -fno-omit-frame-pointer $(NATIVE_CFLAGS) tests/native_lifecycle_test.c $(NATIVE_LDFLAGS) -o bin/native_lifecycle_test
	ASAN_OPTIONS=detect_leaks=1 ./bin/native_lifecycle_test
	$(CC) -std=c11 -Wall -Wextra -g -O1 -fsanitize=address -fno-omit-frame-pointer $(NATIVE_CFLAGS) tests/native_plan_test.c $(NATIVE_LDFLAGS) -o bin/native_plan_test
	ASAN_OPTIONS=detect_leaks=1 ./bin/native_plan_test
	$(CC) -std=c11 -Wall -Wextra -g -O1 -fsanitize=address -fno-omit-frame-pointer $(NATIVE_CFLAGS) tests/native_blob_test.c $(NATIVE_LDFLAGS) -o bin/native_blob_test
	ASAN_OPTIONS=detect_leaks=1 ./bin/native_blob_test
	$(CC) -std=c11 -Wall -Wextra -g -O1 -fsanitize=address -fno-omit-frame-pointer $(NATIVE_CFLAGS) tests/native_prepared_test.c $(NATIVE_LDFLAGS) -o bin/native_prepared_test
	ASAN_OPTIONS=detect_leaks=1 ./bin/native_prepared_test
	$(CC) -std=c11 -Wall -Wextra -g -O1 -fsanitize=address -fno-omit-frame-pointer $(NATIVE_CFLAGS) tests/native_distributed_test.c $(NATIVE_LDFLAGS) -o bin/native_distributed_test
	ASAN_OPTIONS=detect_leaks=1 ./bin/native_distributed_test
	$(CC) -std=c11 -Wall -Wextra -g -O1 -fsanitize=address -fno-omit-frame-pointer $(NATIVE_CFLAGS) tests/native_services_test.c $(NATIVE_LDFLAGS) -o bin/native_services_test
	ASAN_OPTIONS=detect_leaks=1 ./bin/native_services_test
	$(CC) -std=c11 -Wall -Wextra -g -O1 -fsanitize=address -fno-omit-frame-pointer $(NATIVE_CFLAGS) events/native.c tests/native_events_test.c $(NATIVE_LDFLAGS) -pthread -o bin/native_events_test
	ASAN_OPTIONS=detect_leaks=1 ./bin/native_events_test

test-runner:
	@command -v "$(BATS)" >/dev/null 2>&1 || { printf '%s\n' 'bats-core is required for runner tests' >&2; exit 2; }
	"$(BATS)" tests/runner

test-integration-docker:
	./scripts/test-integration-docker.sh $(GO_TEST_ARGS)

test-faults:
	IMAGE="$(IMAGE)" INTERBASE_INCLUDE="$(INTERBASE_INCLUDE)" bash scripts/test-faults-docker.sh

test-soak:
	IMAGE="$(IMAGE)" INTERBASE_INCLUDE="$(INTERBASE_INCLUDE)" bash scripts/test-integration-docker.sh --soak --soak-duration="$(SOAK_DURATION)" --soak-workers="$(SOAK_WORKERS)" --soak-sample-interval="$(SOAK_SAMPLE_INTERVAL)" -run '^TestSoakConcurrentWorkload$$' -v

test-native-lifecycle:
	IMAGE="$(IMAGE)" INTERBASE_INCLUDE="$(INTERBASE_INCLUDE)" bash scripts/test-integration-docker.sh --native-lifecycle -run '^TestNativeLifecycleRace$$' -v

test-fuzz:
	@set -eu; for target in FuzzConfigAndAttachmentParsing FuzzSQLParameterHandling FuzzNamedValueOrdinalHandling FuzzTypedValueConversions FuzzScaledIntegerFormatting FuzzDirectArrayNormalization FuzzInformationResponseParsing; do \
		go test . -run '^$$' -fuzz "^$$target$$" -fuzztime="$(FUZZ_TIME)" -timeout="$(FUZZ_TIMEOUT)"; \
	done
	@set -eu; for target in FuzzServiceRequestBuilders FuzzServiceResponseFraming FuzzServiceStructuredResponseFraming; do \
		go test ./services -run '^$$' -fuzz "^$$target$$" -fuzztime="$(FUZZ_TIME)" -timeout="$(FUZZ_TIMEOUT)"; \
	done

bench:
	go test . -run '^$$' -bench . -benchmem -benchtime="$(BENCH_TIME)"

bench-live:
	IMAGE="$(IMAGE)" INTERBASE_INCLUDE="$(INTERBASE_INCLUDE)" bash scripts/test-integration-docker.sh --performance -run '^$$' -bench '^BenchmarkLive' -benchmem -benchtime="$(BENCH_TIME)"

vet:
	go vet ./...
