#!/bin/sh
set -eu

BASE_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
if [ ! -f "$BASE_DIR/config.yaml" ]; then
  cp "$BASE_DIR/config.example.yaml" "$BASE_DIR/config.yaml"
  echo "Created config.yaml. Set a strong auth.password, then run restart.sh again." >&2
  exit 2
fi
exec "$BASE_DIR/gbaselite" restart --config "$BASE_DIR/config.yaml"
