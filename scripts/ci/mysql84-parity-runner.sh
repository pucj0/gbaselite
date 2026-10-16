#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
REPOSITORY_ROOT=$(CDPATH= cd -- "$SCRIPT_DIR/../.." && pwd)
TEMPORARY_ROOT="$REPOSITORY_ROOT/.tmp"

mkdir -p "$TEMPORARY_ROOT"
WORK_DIRECTORY=$(mktemp -d "$TEMPORARY_ROOT/mysql84-parity-XXXXXXXX")
case "$WORK_DIRECTORY" in
  "$TEMPORARY_ROOT"/*) ;;
  *) echo "Unsafe temporary directory: $WORK_DIRECTORY" >&2; exit 1 ;;
esac

BINARY="$WORK_DIRECTORY/gbaselite"
CONFIG="$WORK_DIRECTORY/config.yaml"
SERVER_LOG="$WORK_DIRECTORY/server.log"
SERVER_PID=
GB_PORT=23307
MY_PORT=${MYSQL_PORT:-33066}
MY_PWD=${MYSQL_ROOT_PASSWORD:-parity_ci_password}

cleanup() {
  local status=$?
  trap - EXIT
  set +e
  if [ -n "$SERVER_PID" ] && kill -0 "$SERVER_PID" 2>/dev/null; then
    kill "$SERVER_PID" 2>/dev/null || true
    wait "$SERVER_PID" 2>/dev/null || true
  fi
  if [ "$status" -ne 0 ] && [ -f "$SERVER_LOG" ]; then
    echo "=== GBaseLite server.log ===" >&2
    cat "$SERVER_LOG" >&2
  fi
  rm -rf "$WORK_DIRECTORY"
  exit "$status"
}
trap cleanup EXIT INT TERM

# 1. Build GBaseLite binary
echo "[1/4] Building GBaseLite..."
(cd "$REPOSITORY_ROOT" && CGO_ENABLED=0 go build -trimpath -o "$BINARY" ./cmd/gbaselite)

# 2. Prepare GBaseLite configuration
cat <<EOF >"$CONFIG"
server:
  host: 127.0.0.1
  port: $GB_PORT
storage:
  path: '$WORK_DIRECTORY/data'
auth:
  username: testuser
  password: 'testpassword'
log:
  path: '$WORK_DIRECTORY/logs'
audit:
  enabled: false
binlog:
  enabled: false
EOF

mkdir -p "$WORK_DIRECTORY/data" "$WORK_DIRECTORY/logs"

# 3. Start GBaseLite server
echo "[2/4] Starting GBaseLite server on port $GB_PORT..."
"$BINARY" server --config "$CONFIG" >"$SERVER_LOG" 2>&1 &
SERVER_PID=$!

ready=0
attempt=1
while [ "$attempt" -le 50 ]; do
  if ! kill -0 "$SERVER_PID" 2>/dev/null; then
    break
  fi
  if "$BINARY" healthcheck --host 127.0.0.1 --port "$GB_PORT" >/dev/null 2>&1; then
    ready=1
    break
  fi
  attempt=$((attempt + 1))
  sleep 0.2
done

if [ "$ready" -ne 1 ]; then
  echo "GBaseLite failed to start on port $GB_PORT" >&2
  exit 1
fi
echo "GBaseLite is healthy on port $GB_PORT (PID: $SERVER_PID)"

MYSQL_DSN="root:${MY_PWD}@tcp(127.0.0.1:${MY_PORT})/?charset=utf8mb4&timeout=5s"
GBASE_DSN="testuser:testpassword@tcp(127.0.0.1:${GB_PORT})/?charset=utf8mb4&timeout=5s"

# 4. FIX-04: Negative Fault Injection Verification
echo "[3/4] Running FIX-04 Negative Verification (--inject-fault)..."
FAULT_REPORT="$WORK_DIRECTORY/b03-parity-fault.md"
set +e
(cd "$REPOSITORY_ROOT" && go run ./scripts/parity \
  -expected-mysql-version=8.4 \
  -mysql-dsn="$MYSQL_DSN" \
  -gbase-dsn="$GBASE_DSN" \
  -output="$FAULT_REPORT" \
  -inject-fault >"$WORK_DIRECTORY/fault_injection.log" 2>&1)
fault_code=$?
set -e

if [ "$fault_code" -eq 0 ]; then
  echo "ERROR: --inject-fault unexpectedly exited with 0; runner failed to detect injected fault!" >&2
  cat "$WORK_DIRECTORY/fault_injection.log" >&2
  exit 1
fi

# Verify FIX-04 detailed criteria:
# 1. Connected to databases successfully (not infrastructure startup failure)
if ! grep -q "Connected to MySQL" "$WORK_DIRECTORY/fault_injection.log" || ! grep -q "Connected to GBaseLite" "$WORK_DIRECTORY/fault_injection.log"; then
  echo "ERROR: Fault injection exited due to connection failure, not injected test failure!" >&2
  cat "$WORK_DIRECTORY/fault_injection.log" >&2
  exit 1
fi

# 2. Report was generated
if [ ! -f "$FAULT_REPORT" ]; then
  echo "ERROR: Fault report was not generated at $FAULT_REPORT" >&2
  exit 1
fi

# 3. M08 classified as FAIL and contains INJECTED FAULT marker
if ! grep -q "INJECTED FAULT" "$FAULT_REPORT"; then
  echo "ERROR: Fault report does not contain INJECTED FAULT marker!" >&2
  cat "$FAULT_REPORT" >&2
  exit 1
fi

if ! grep -q "| \*\*M08\*\* | .* | \*\*FAIL\*\* |" "$FAULT_REPORT"; then
  echo "ERROR: Fault report did not classify M08 as FAIL!" >&2
  cat "$FAULT_REPORT" >&2
  exit 1
fi

echo "FIX-04 verified: --inject-fault exited with non-zero code ($fault_code), connected to both DBs, generated report, and classified M08 as FAIL."

# 5. FIX-01..03: Full Parity Run against MySQL 8.4
echo "[4/4] Running Full M01-M44 Parity Test against MySQL 8.4..."
(cd "$REPOSITORY_ROOT" && go run ./scripts/parity \
  -expected-mysql-version=8.4 \
  -mysql-dsn="$MYSQL_DSN" \
  -gbase-dsn="$GBASE_DSN" \
  -output="$TEMPORARY_ROOT/b03-parity.md")

echo "Parity run completed successfully. Deliverable generated at $TEMPORARY_ROOT/b03-parity.md"
