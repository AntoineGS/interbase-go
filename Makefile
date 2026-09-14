.PHONY: build test test-native test-runner test-integration-docker vet

BATS ?= bats

build:
	mkdir -p bin
	go build -o bin/ibprobe ./cmd/ibprobe

test:
	go test ./... -count=1 -timeout=60s
	$(MAKE) test-native

test-native:
	mkdir -p bin
	$(CC) -std=c11 -Wall -Wextra -g -O1 -fsanitize=address -fno-omit-frame-pointer -I/opt/interbase/include tests/native_status_test.c -L/opt/interbase/lib -Wl,-rpath,/opt/interbase/lib -lgds -o bin/native_status_test
	ASAN_OPTIONS=detect_leaks=1 ./bin/native_status_test
	$(CC) -std=c11 -Wall -Wextra -g -O1 -fsanitize=address -fno-omit-frame-pointer -I/opt/interbase/include tests/native_values_test.c -L/opt/interbase/lib -Wl,-rpath,/opt/interbase/lib -lgds -o bin/native_values_test
	ASAN_OPTIONS=detect_leaks=1 ./bin/native_values_test
	$(CC) -std=c11 -Wall -Wextra -g -O1 -fsanitize=address -fno-omit-frame-pointer -I/opt/interbase/include tests/native_lifecycle_test.c -L/opt/interbase/lib -Wl,-rpath,/opt/interbase/lib -lgds -o bin/native_lifecycle_test
	ASAN_OPTIONS=detect_leaks=1 ./bin/native_lifecycle_test
	$(CC) -std=c11 -Wall -Wextra -g -O1 -fsanitize=address -fno-omit-frame-pointer -I/opt/interbase/include tests/native_blob_test.c -L/opt/interbase/lib -Wl,-rpath,/opt/interbase/lib -lgds -o bin/native_blob_test
	ASAN_OPTIONS=detect_leaks=1 ./bin/native_blob_test
	$(CC) -std=c11 -Wall -Wextra -g -O1 -fsanitize=address -fno-omit-frame-pointer -I/opt/interbase/include tests/native_prepared_test.c -L/opt/interbase/lib -Wl,-rpath,/opt/interbase/lib -lgds -o bin/native_prepared_test
	ASAN_OPTIONS=detect_leaks=1 ./bin/native_prepared_test

test-runner:
	@command -v "$(BATS)" >/dev/null 2>&1 || { printf '%s\n' 'bats-core is required for runner tests' >&2; exit 2; }
	"$(BATS)" tests/runner

test-integration-docker:
	./scripts/test-integration-docker.sh $(GO_TEST_ARGS)

vet:
	go vet ./...
