#!/bin/sh
set -eu

runtime_user=gbaselite
runtime_group=gbaselite

permission_error() {
  path=$1
  owner=$(stat -c '%u:%g' "$path" 2>/dev/null || echo unknown)
  mode=$(stat -c '%a' "$path" 2>/dev/null || echo unknown)
  cat >&2 <<EOF
error: $path is not writable by GBaseLite uid:gid 10001:10001 (owner $owner, mode $mode)
the image normally repairs bind-mount ownership before dropping privileges.
check for a read-only mount or filesystem restriction; when SELinux is enforcing, add :Z to the bind mount
EOF
  exit 1
}

prepare_directory() {
  path=$1
  if [ "$(id -u)" = "0" ]; then
    mkdir -p "$path" || permission_error "$path"
    find "$path" \( ! -user "$runtime_user" -o ! -group "$runtime_group" \) \
      -exec chown "$runtime_user:$runtime_group" {} + || permission_error "$path"
    su-exec "$runtime_user:$runtime_group" test -w "$path" || permission_error "$path"
  elif [ ! -d "$path" ] || [ ! -w "$path" ]; then
    permission_error "$path"
  fi
}

if [ "${1:-}" = "server" ]; then
  prepare_directory "${DB_DATA_PATH:-/app/data}"
  prepare_directory /app/logs
fi

if [ "$(id -u)" = "0" ]; then
  exec su-exec "$runtime_user:$runtime_group" /app/gbaselite "$@"
fi

exec /app/gbaselite "$@"
