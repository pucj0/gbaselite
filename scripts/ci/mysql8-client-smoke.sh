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
  docker run --rm --network host \
    -e "MYSQL_PWD=$PASSWORD" \
    "$MYSQL_IMAGE" mysql \
    --protocol=TCP --host=127.0.0.1 --port="$PORT" --user="$USERNAME" \
    --default-character-set=utf8mb4 "$@"
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
CREATE VIEW `active-items` AS
  SELECT `id`, `sku`, `qty` FROM `order-items` WHERE `qty` > 0;
SQL

if ! database_list=$(mysql_client --batch --raw --skip-column-names --execute='SHOW DATABASES;'); then
  echo "SHOW DATABASES failed through the MySQL 8 client" >&2
  exit 1
fi

database_found=0
while IFS= read -r database_name; do
  case "$database_name" in
    "$DATABASE") database_found=1 ;;
    information_schema|mysql)
      echo "SHOW DATABASES exposed non-persistent compatibility database: $database_name" >&2
      exit 1
      ;;
  esac
done <<<"$database_list"
if [ "$database_found" -ne 1 ]; then
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
grep -Eiq 'active-items' "$DUMP"
grep -Eiq '/\*![0-9]+.*VIEW' "$DUMP"

drop_temporary_database
mysql_client <"$DUMP"

counts=$(mysql_client --batch --raw --skip-column-names <<'SQL'
SELECT COUNT(*) FROM `gbaselite-ci-export`.`order-items`;
SELECT COUNT(*) FROM `gbaselite-ci-export`.`active-items`;
SQL
)
if [ "$counts" != $'2\n1' ]; then
  echo "Unexpected restored table/view counts: $counts" >&2
  exit 1
fi

indexes=$(mysql_client --batch --raw --skip-column-names --execute="SELECT INDEX_NAME FROM information_schema.STATISTICS WHERE TABLE_SCHEMA='$DATABASE' AND TABLE_NAME='order-items' ORDER BY INDEX_NAME;")
for index_name in PRIMARY idx_qty uq_sku; do
  printf '%s\n' "$indexes" | grep -Fx "$index_name" >/dev/null
done

view_definition=$(mysql_client --batch --raw --skip-column-names --execute='SHOW CREATE VIEW `gbaselite-ci-export`.`active-items`;')
printf '%s\n' "$view_definition" | grep -Fq 'CREATE VIEW `active-items`'

drop_temporary_database
echo "MySQL 8 client dump/import smoke test passed."
