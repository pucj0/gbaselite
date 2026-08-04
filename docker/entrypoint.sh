#!/bin/sh
set -eu

if [ "${1:-}" = "server" ]; then
  data_path=${DB_DATA_PATH:-/app/data}
  for path in "$data_path" /app/logs; do
    if [ ! -d "$path" ] || [ ! -w "$path" ]; then
      owner=$(stat -c '%u:%g' "$path" 2>/dev/null || echo unknown)
      mode=$(stat -c '%a' "$path" 2>/dev/null || echo unknown)
      cat >&2 <<EOF
error: $path is not writable by GBaseLite uid:gid $(id -u):$(id -g) (owner $owner, mode $mode)
fix the bind-mounted host directories before restarting:
  chown -R $(id -u):$(id -g) <data-directory> <logs-directory>
when SELinux is enforcing, also add :Z to both bind mounts
EOF
      exit 1
    fi
  done
fi

exec /app/gbaselite "$@"
