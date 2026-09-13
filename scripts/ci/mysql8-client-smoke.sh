#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
REPOSITORY_ROOT=$(CDPATH= cd -- "$SCRIPT_DIR/../.." && pwd)
TEMPORARY_ROOT="$REPOSITORY_ROOT/.tmp"
MYSQL_IMAGE=${MYSQL_IMAGE:-mysql:8.4}
PORT=${GBASELITE_SMOKE_PORT:-13307}
DATABASE=gbaselite-ci-export
USERNAME=ci_admin
PASSWORD=ci-only-password

mkdir -p "$TEMPORARY_ROOT"
WORK_DIRECTORY=$(mktemp -d "$TEMPORARY_ROOT/mysql8-client-smoke-XXXXXXXX")
case "$WORK_DIRECTORY" in
  "$TEMPORARY_ROOT"/*) ;;
  *) echo "Unsafe temporary directory: $WORK_DIRECTORY" >&2; exit 1 ;;
esac

BINARY="$WORK_DIRECTORY/gbaselite"
CONFIG="$WORK_DIRECTORY/config.yaml"
DUMP="$WORK_DIRECTORY/export.sql"
SERVER_LOG="$WORK_DIRECTORY/server.log"
SERVER_PID=

mysql_client() {
  docker run --rm --interactive --network host \
    -e "MYSQL_PWD=$PASSWORD" \
    "$MYSQL_IMAGE" mysql \
    --protocol=TCP --host=127.0.0.1 --port="$PORT" --user="$USERNAME" \
    --default-character-set=utf8mb4 "$@"
}

workflow_error() {
  local message=$1
  message=${message//'%'/'%25'}
  message=${message//$'\r'/'%0D'}
  message=${message//$'\n'/'%0A'}
  printf '::error title=MySQL 8 smoke test::%s\n' "$message" >&2
}

drop_temporary_database() {
  mysql_client --execute='DROP DATABASE IF EXISTS `gbaselite-ci-export`;' >/dev/null 2>&1
}

cleanup() {
  status=$1
  trap - EXIT
  set +e
  if [ -n "$SERVER_PID" ] && kill -0 "$SERVER_PID" 2>/dev/null; then
    drop_temporary_database
    kill "$SERVER_PID"
    wait "$SERVER_PID"
  fi
  if [ "$status" -ne 0 ] && [ -f "$SERVER_LOG" ]; then
    echo "GBaseLite smoke-test log:" >&2
    cat "$SERVER_LOG" >&2
  fi
  case "$WORK_DIRECTORY" in
    "$TEMPORARY_ROOT"/*) rm -rf -- "$WORK_DIRECTORY" ;;
  esac
  exit "$status"
}
trap 'cleanup $?' EXIT

cat >"$CONFIG" <<EOF
server:
  host: 127.0.0.1
  port: $PORT
storage:
  path: '$WORK_DIRECTORY/data'
auth:
  username: $USERNAME
  password: '$PASSWORD'
log:
  path: '$WORK_DIRECTORY/logs'
audit:
  enabled: false
binlog:
  enabled: false
EOF

(cd "$REPOSITORY_ROOT" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
  go build -trimpath -o "$BINARY" ./cmd/gbaselite)
"$BINARY" server --config "$CONFIG" >"$SERVER_LOG" 2>&1 &
SERVER_PID=$!

ready=0
attempt=1
while [ "$attempt" -le 50 ]; do
  if ! kill -0 "$SERVER_PID" 2>/dev/null; then
    break
  fi
  if "$BINARY" healthcheck --host 127.0.0.1 --port "$PORT" >/dev/null 2>&1; then
    ready=1
    break
  fi
  attempt=$((attempt + 1))
  sleep 0.2
done
if [ "$ready" -ne 1 ]; then
  echo "GBaseLite did not become healthy on port $PORT" >&2
  exit 1
fi

mysql_client <<'SQL'
CREATE DATABASE `gbaselite-ci-export`;
USE `gbaselite-ci-export`;
CREATE TABLE `order-items` (
  `id` BIGINT NOT NULL AUTO_INCREMENT,
  `sku` VARCHAR(32) NOT NULL,
  `qty` INT NOT NULL DEFAULT 0,
  PRIMARY KEY (`id`),
  UNIQUE KEY `uq_sku` (`sku`),
  KEY `idx_qty` (`qty`)
);
INSERT INTO `order-items` (`sku`, `qty`) VALUES ('SKU-001', 2), ('SKU-002', 0);
BEGIN;
INSERT INTO `order-items` (`sku`, `qty`) VALUES ('ROLLED-BACK', 1);
ROLLBACK;
SQL

SHOW_DATABASES_ERROR="$WORK_DIRECTORY/show-databases.err"
if ! database_list=$(mysql_client --batch --raw --skip-column-names --execute='SHOW DATABASES;' 2>"$SHOW_DATABASES_ERROR"); then
  database_error=$(<"$SHOW_DATABASES_ERROR")
  workflow_error "SHOW DATABASES failed through the MySQL 8 client: ${database_error:-no client error output}"
  cat "$SHOW_DATABASES_ERROR" >&2
  exit 1
fi

database_found=0
while IFS= read -r database_name; do
  case "$database_name" in
    "$DATABASE") database_found=1 ;;
    information_schema|mysql)
      message="SHOW DATABASES exposed non-persistent compatibility database: $database_name"
      workflow_error "$message"
      echo "$message" >&2
      exit 1
      ;;
  esac
done <<<"$database_list"
if [ "$database_found" -ne 1 ]; then
  message="SHOW DATABASES did not return $DATABASE; actual rows: $database_list"
  workflow_error "$message"
  echo "SHOW DATABASES did not return $DATABASE; actual rows:" >&2
  printf '%s\n' "$database_list" >&2
  exit 1
fi

docker run --rm --network host \
  -e "MYSQL_PWD=$PASSWORD" \
  "$MYSQL_IMAGE" mysqldump \
  --protocol=TCP --host=127.0.0.1 --port="$PORT" --user="$USERNAME" \
  --default-character-set=utf8mb4 --column-statistics=0 --skip-lock-tables \
  --skip-add-locks --skip-disable-keys \
  --no-tablespaces --skip-triggers --set-gtid-purged=OFF \
  --databases "$DATABASE" >"$DUMP"

grep -Eiq 'CREATE[[:space:]]+DATABASE' "$DUMP"
grep -Fq 'USE `gbaselite-ci-export`' "$DUMP"
grep -Eiq 'CREATE[[:space:]]+TABLE[[:space:]]+`order-items`' "$DUMP"
grep -Eiq 'INSERT[[:space:]]+INTO[[:space:]]+`order-items`' "$DUMP"

drop_temporary_database
mysql_client <"$DUMP"

counts=$(mysql_client --batch --raw --skip-column-names <<'SQL'
SELECT COUNT(*) FROM `gbaselite-ci-export`.`order-items`;
SELECT COUNT(*) FROM `gbaselite-ci-export`.`order-items` WHERE qty > 0;
SQL
)
if [ "$counts" != $'2\n1' ]; then
  echo "Unexpected restored table/filter counts: $counts" >&2
  exit 1
fi

indexes=$(mysql_client --batch --raw --skip-column-names --execute="SELECT INDEX_NAME FROM information_schema.STATISTICS WHERE TABLE_SCHEMA='$DATABASE' AND TABLE_NAME='order-items' ORDER BY INDEX_NAME;")
for index_name in PRIMARY idx_qty uq_sku; do
  printf '%s\n' "$indexes" | grep -Fx "$index_name" >/dev/null
done

# The MVCC runtime supports views, so the smoke test exercises the full view
# lifecycle through the real MySQL 8 client instead of asserting that views are
# rejected.
run_view_statement() {
  local description=$1
  shift
  local error_file="$WORK_DIRECTORY/view-statement.err"
  if ! "$@" >"$error_file" 2>&1; then
    local detail
    detail=$(<"$error_file")
    workflow_error "$description failed through the MySQL 8 client: ${detail:-no client error output}"
    cat "$error_file" >&2
    exit 1
  fi
}

run_view_statement "CREATE VIEW" mysql_client --execute='CREATE VIEW `gbaselite-ci-export`.`active-items` AS SELECT `id`, `sku`, `qty` FROM `gbaselite-ci-export`.`order-items` WHERE `qty` > 0;'

view_count=$(mysql_client --batch --raw --skip-column-names --execute='SELECT COUNT(*) FROM `gbaselite-ci-export`.`active-items`;')
if [ "$view_count" != "1" ]; then
  message="Unexpected active-items view count: $view_count"
  workflow_error "$message"
  echo "$message" >&2
  exit 1
fi

show_create_error="$WORK_DIRECTORY/show-create-view.err"
if ! show_create=$(mysql_client --batch --raw --skip-column-names --execute='SHOW CREATE VIEW `gbaselite-ci-export`.`active-items`;' 2>"$show_create_error"); then
  detail=$(<"$show_create_error")
  workflow_error "SHOW CREATE VIEW failed through the MySQL 8 client: ${detail:-no client error output}"
  cat "$show_create_error" >&2
  exit 1
fi
if ! printf '%s\n' "$show_create" | grep -F 'active-items' >/dev/null; then
  message="SHOW CREATE VIEW output is missing the view name: $show_create"
  workflow_error "$message"
  echo "$message" >&2
  exit 1
fi
if ! printf '%s\n' "$show_create" | grep -Eiq 'CREATE[[:space:]]+VIEW'; then
  message="SHOW CREATE VIEW output is missing CREATE VIEW: $show_create"
  workflow_error "$message"
  echo "$message" >&2
  exit 1
fi

full_tables=$(mysql_client --database="$DATABASE" --batch --raw --skip-column-names --execute='SHOW FULL TABLES;')
if ! printf '%s\n' "$full_tables" | grep -Eq '^order-items[[:space:]]+BASE TABLE$'; then
  message="SHOW FULL TABLES did not report order-items as a BASE TABLE: $full_tables"
  workflow_error "$message"
  echo "$message" >&2
  exit 1
fi
if ! printf '%s\n' "$full_tables" | grep -Eq '^active-items[[:space:]]+VIEW$'; then
  message="SHOW FULL TABLES did not report active-items as a VIEW: $full_tables"
  workflow_error "$message"
  echo "$message" >&2
  exit 1
fi

run_view_statement "DROP VIEW" mysql_client --execute='DROP VIEW `gbaselite-ci-export`.`active-items`;'
if mysql_client --execute='SELECT * FROM `gbaselite-ci-export`.`active-items`;' >"$WORK_DIRECTORY/dropped-view.out" 2>&1; then
  message="A dropped view remained queryable through the MySQL 8 client"
  workflow_error "$message"
  echo "$message" >&2
  exit 1
fi

storage_mode=$(mysql_client --batch --raw --skip-column-names --execute="SHOW STATUS LIKE 'Gbaselite_storage_mode';")
printf '%s\n' "$storage_mode" | grep -Eq '[[:space:]]mvcc$'

# DROP DATABASE must clear the table and view catalog entries of the database, so a
# recreated database starts empty and the old view name is free for a table.
run_view_statement "recreated CREATE VIEW" mysql_client --execute='CREATE VIEW `gbaselite-ci-export`.`active-items` AS SELECT `id`, `sku`, `qty` FROM `gbaselite-ci-export`.`order-items` WHERE `qty` > 0;'
run_view_statement "DROP DATABASE" mysql_client --execute='DROP DATABASE `gbaselite-ci-export`;'
run_view_statement "recreated CREATE DATABASE" mysql_client --execute='CREATE DATABASE `gbaselite-ci-export`;'
remaining_relations=$(mysql_client --database="$DATABASE" --batch --raw --skip-column-names --execute='SHOW FULL TABLES;')
if [ -n "$remaining_relations" ]; then
  message="DROP DATABASE left relations behind in $DATABASE: $remaining_relations"
  workflow_error "$message"
  echo "$message" >&2
  exit 1
fi
run_view_statement "CREATE TABLE reusing the dropped view name" mysql_client --database="$DATABASE" --execute='CREATE TABLE `active-items` (`id` INT);'

drop_temporary_database
echo "MySQL 8 client dump/import smoke test passed."