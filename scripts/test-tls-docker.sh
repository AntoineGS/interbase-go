#!/usr/bin/env bash
# Native Config.TLS security contract. Requires the sibling server image's
# entrypoint, an official matching SDK, and Linux/amd64. Never pulls/pushes.
# shellcheck disable=SC2016 # SQL $ names are literal; credentials expand only inside the container.
set +x
set -Eeuo pipefail
umask 077

usage() {
  printf '%s\n' 'Usage: IMAGE=private:local INTERBASE_INCLUDE=/sdk/include bash scripts/test-tls-docker.sh' \
    'Runs only TestNativeTLSVerification in a disposable network-none container.' \
    'No host ports, host mounts, license reads, image builds, pulls or pushes.' \
    'Requires Docker, Go/cgo compiler, OpenSSL and GNU timeout; TMPDIR is optional.'
}

die() { printf 'ERROR: %s\n' "$*" >&2; exit 2; }
if [[ ${1:-} == --help || ${1:-} == -h ]]; then usage; exit 0; fi
if (($#)); then usage >&2; exit 2; fi
[[ -n ${IMAGE:-} ]] || die 'IMAGE is required (existing private local image)'
[[ $IMAGE =~ ^[[:alnum:]][[:alnum:]_.:/@-]*$ ]] || die 'invalid image reference'
[[ -n ${INTERBASE_INCLUDE:-} && -f ${INTERBASE_INCLUDE}/ibase.h ]] ||
  die 'INTERBASE_INCLUDE must contain ibase.h'
[[ $INTERBASE_INCLUDE == /* && $INTERBASE_INCLUDE != *[[:space:]?:,]* ]] ||
  die 'INTERBASE_INCLUDE must be an absolute path without whitespace or attachment delimiters'
parent=${TMPDIR:-/tmp}
[[ $parent == /* && -d $parent && -w $parent && $parent != *[[:space:]?:,]* ]] ||
  die 'TMPDIR must be an existing writable absolute path without whitespace or delimiters'
for tool in docker go openssl timeout mktemp; do
  command -v "$tool" >/dev/null || die "required command unavailable: $tool"
done
[[ $(go env GOHOSTOS)/$(go env GOHOSTARCH) == linux/amd64 ]] || die 'only Linux/amd64 is supported'
[[ $(timeout 30s docker image inspect --format '{{.Os}}/{{.Architecture}}' "$IMAGE") == linux/amd64 ]] ||
  die 'image must exist locally and be Linux/amd64'

# Ignore ambient application credentials, targets, client overrides and fixture
# opt-ins. Only the supplied header directory is relevant to this runner.
while IFS= read -r name; do
  case $name in
    INTERBASE|INTERBASE_*|IB_*|ISC_*)
      if [[ $name != INTERBASE_INCLUDE ]]; then unset "$name"; fi ;;
  esac
done < <(compgen -e)

repo=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd -P)
root=$(mktemp -d "${parent%/}/interbase-go-tls.XXXXXXXX")
container="interbase-go-tls-${root##*.}"
owner=${root##*/}
cleanup() {
  local status=$? label=''
  trap - EXIT
  trap '' INT TERM
  # Label check protects an unrelated container even if Docker creation failed
  # with a name collision. A failed inspect is NOT proof the container is gone.
  if [[ -f $root/container-attempted ]]; then
    if label=$(timeout 30s docker inspect --format '{{index .Config.Labels "interbase-go.tls-owner"}}' "$container" 2>/dev/null); then
      if [[ $label == "$owner" ]]; then
        if ! timeout 30s docker rm -f -v "$container" >/dev/null; then
          printf 'Cleanup failed; private artifacts retained: %s; container=%s\n' "$root" "$container" >&2
          exit 1
        fi
      else
        printf 'Container ownership mismatch; not removing %s\n' "$container" >&2
        status=1
      fi
    else
      printf 'Cannot verify container cleanup; private artifacts retained: %s; container=%s\n' "$root" "$container" >&2
      exit 1
    fi
  fi
  rm -rf -- "$root"
  printf 'TLS fixture cleanup complete: container=%s temporary_root=%s\n' "$container" "$root"
  exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
# Do not echo commands: they may expand credentials.
trap 'printf "TLS runner failed at line %s\n" "$LINENO" >&2' ERR

printf 'TLS fixture: container=%s temporary_root=%s\n' "$container" "$root"
for ca in ca wrong-ca; do
  timeout 30s openssl req -x509 -newkey rsa:2048 -nodes -sha256 -days 2 \
    -subj "/CN=interbase-go-tls-$ca" \
    -addext 'basicConstraints=critical,CA:TRUE' \
    -addext 'keyUsage=critical,keyCertSign,cRLSign' \
    -keyout "$root/$ca.key" -out "$root/$ca.pem" >"$root/openssl.log" 2>&1
done
timeout 30s openssl req -new -newkey rsa:2048 -nodes -sha256 -subj /CN=localhost \
  -keyout "$root/server.key" -out "$root/server.csr" >"$root/openssl.log" 2>&1
printf '%s\n' 'basicConstraints=critical,CA:FALSE' \
  'keyUsage=critical,digitalSignature,keyEncipherment' 'extendedKeyUsage=serverAuth' \
  'subjectAltName=DNS:localhost' >"$root/server.ext"
timeout 30s openssl x509 -req -in "$root/server.csr" -CA "$root/ca.pem" \
  -CAkey "$root/ca.key" -set_serial 2 -days 2 -sha256 -extfile "$root/server.ext" \
  -out "$root/server.crt" >"$root/openssl.log" 2>&1
cat "$root/server.crt" "$root/server.key" >"$root/server.pem"
# The installed ibss_config.default and OpGuide pp. 65-74 specify these names.
# Do not copy the guide's obsolete sample spellings or weaken TLS/DH settings.
printf '%s\n' 'IBSSL_SERVER_HOST_NAME=localhost' 'IBSSL_SERVER_PORT_NO=3065' \
  'IBSSL_SERVER_CERTFILE="/tmp/parity-tls/server.pem"' >"$root/ibss_config"
password=$(openssl rand -hex 4) # Eight characters, respecting legacy password limits.
printf '%s\n' 'IB_SYSDBA_USER=SYSDBA' "IB_SYSDBA_PASSWORD=$password" \
  'IB_BACKUP_DATABASES=' 'IB_LOG_DIR=/var/lib/interbase/logs' \
  'INTERBASE_TLS_TEST=1' 'INTERBASE_TLS_USER=SYSDBA' "INTERBASE_TLS_PASSWORD=$password" \
  'INTERBASE_TLS_DATABASE=/var/lib/interbase/data/tls.ib' \
  'INTERBASE_TLS_CA=/tmp/parity-tls/ca.pem' 'INTERBASE_TLS_WRONG_CA=/tmp/parity-tls/wrong-ca.pem' \
  'INTERBASE_TLS_SERVER_CERT=/tmp/parity-tls/server.crt' >"$root/runtime.env"
unset password
printf '%s\n' "CREATE DATABASE '/var/lib/interbase/data/tls.ib';" \
  'SELECT 424242 FROM RDB$DATABASE;' 'QUIT;' >"$root/create.sql"

touch "$root/container-attempted"
timeout 30s docker run --pull=never --detach --network none --hostname localhost \
  --add-host=wrong.parity.invalid=::1 --name "$container" \
  --label "interbase-go.tls-owner=$owner" --mount type=tmpfs,destination=/var/lib/interbase \
  --entrypoint /bin/sleep "$IMAGE" infinity >"$root/container.id"
printf 'Created container ID: %s\n' "$(<"$root/container.id")"
timeout 30s docker exec "$container" mkdir -m 700 /tmp/parity-tls
for file in server.pem server.crt ca.pem wrong-ca.pem create.sql; do
  timeout 30s docker cp "$root/$file" "$container:/tmp/parity-tls/$file"
done
timeout 30s docker cp "$root/ibss_config" "$container:/opt/interbase/secure/server/ibss_config"
timeout 30s docker exec --detach --env-file "$root/runtime.env" "$container" \
  sh -c '/usr/local/bin/docker-entrypoint.sh ibguard -forever >/tmp/parity-tls/startup.log 2>&1'

ready=0
for ((attempt=0; attempt<20; attempt++)); do
  # Authentication positive AND negative controls; gsec exit status is unreliable.
  if timeout 10s docker exec --env-file "$root/runtime.env" "$container" sh -c \
    'gsec -user "$IB_SYSDBA_USER" -password "$IB_SYSDBA_PASSWORD" -display' >"$root/good.log" 2>&1 &&
    grep -qE '^SYSDBA([[:space:]]|$)' "$root/good.log"; then
    timeout 10s docker exec "$container" gsec -user SYSDBA -password deliberately-wrong -display \
      >"$root/bad.log" 2>&1 || true
    if grep -qi 'Your user name and password are not defined' "$root/bad.log"; then ready=1; break; fi
  fi
  sleep 1
done
[[ $ready == 1 ]] || die 'native authentication readiness failed (raw credential-bearing output suppressed)'
printf '%s\n' 'Native authentication positive/negative controls passed.'
timeout 15s docker exec --env-file "$root/runtime.env" "$container" sh -c \
  'export ISC_USER="$IB_SYSDBA_USER" ISC_PASSWORD="$IB_SYSDBA_PASSWORD"; isql -input /tmp/parity-tls/create.sql' \
  >"$root/create.log" 2>&1 || true
grep -qE '^[[:space:]]*424242[[:space:]]*$' "$root/create.log" || die 'owned database creation did not produce completion marker'
if grep -qiE 'Statement failed|SQLCODE[[:space:]]*=' "$root/create.log"; then die 'owned database creation reported SQL errors'; fi

timeout 30s docker cp -L "$container:/opt/interbase/lib/libgds.so" "$root/libgds.so"
(
  cd -- "$repo"
  CGO_ENABLED=1 GOOS=linux GOARCH=amd64 CGO_CFLAGS="-I$INTERBASE_INCLUDE" \
    CGO_LDFLAGS="-L$root -Wl,-rpath,/opt/interbase/lib" \
    timeout 180s go test -tags=integration -c -o "$root/integration.test" ./integration
)
timeout 30s docker cp "$root/integration.test" "$container:/tmp/parity-tls/integration.test"
# A Go timeout is essential: context deadlines cannot interrupt native calls.
# Propagate failures, including the currently observed hostname verification bug.
timeout --kill-after=5s 100s docker exec --env-file "$root/runtime.env" "$container" \
  /tmp/parity-tls/integration.test -test.run '^TestNativeTLSVerification$' \
  -test.v -test.count=1 -test.timeout=90s
