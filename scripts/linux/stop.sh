#!/bin/sh
set -eu

BASE_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
if [ ! -f "$BASE_DIR/config.yaml" ]; then
  echo "config.yaml does not exist." >&2
  exit 2
fi
exec "$BASE_DIR/gbaselite" stop --config "$BASE_DIR/config.yaml"
