#!/usr/bin/env sh
set -eu

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
REPOSITORY_ROOT=$(CDPATH= cd -- "$SCRIPT_DIR/.." && pwd)
VERSION=${VERSION:-$(sed -n 's/.*const Version = "\([^"]*\)".*/\1/p' "$REPOSITORY_ROOT/executor/executor.go")}
OUTPUT_DIRECTORY=${OUTPUT_DIRECTORY:-$REPOSITORY_ROOT/dist}
GO_EXECUTABLE=${GO_EXECUTABLE:-go}

case "$VERSION" in
  [0-9]*.[0-9]*.[0-9]*) ;;
  *) echo "Invalid VERSION: $VERSION" >&2; exit 1 ;;
esac

mkdir -p "$REPOSITORY_ROOT/.tmp" "$OUTPUT_DIRECTORY"
STAGE_ROOT=$(mktemp -d "$REPOSITORY_ROOT/.tmp/release-XXXXXXXX")
cleanup() {
  case "$STAGE_ROOT" in
    "$REPOSITORY_ROOT/.tmp/"*) rm -rf -- "$STAGE_ROOT" ;;
  esac
}
trap cleanup EXIT INT TERM

if [ "${SKIP_CHECKS:-0}" != "1" ]; then
  UNFORMATTED=$(find "$REPOSITORY_ROOT" -type f -name '*.go' \
    ! -path "$REPOSITORY_ROOT/.tmp/*" \
    ! -path "$REPOSITORY_ROOT/data/*" \
    ! -path "$REPOSITORY_ROOT/dist/*" \
    -exec gofmt -l {} +)
  if [ -n "$UNFORMATTED" ]; then
    echo "The following Go files require gofmt:" >&2
    echo "$UNFORMATTED" >&2
    exit 1
  fi
  (cd "$REPOSITORY_ROOT" && "$GO_EXECUTABLE" test ./... -count=1)
  (cd "$REPOSITORY_ROOT" && "$GO_EXECUTABLE" vet ./...)
fi

build_target() {
  target_os=$1
  target_arch=$2
  output=$3
  (cd "$REPOSITORY_ROOT" && CGO_ENABLED=0 GOOS="$target_os" GOARCH="$target_arch" \
    "$GO_EXECUTABLE" build -trimpath -ldflags="-s -w" -o "$output" ./cmd/gbaselite)
}

copy_portable_files() {
  destination=$1
  platform=$2
  cp "$REPOSITORY_ROOT/README.md" "$REPOSITORY_ROOT/LICENSE" "$destination/"
  cp "$REPOSITORY_ROOT/docker/config.example.yaml" "$destination/config.example.yaml"
  if [ "$platform" = "windows" ]; then
    cp "$REPOSITORY_ROOT"/scripts/windows/*.bat "$destination/"
  else
    cp "$REPOSITORY_ROOT"/scripts/linux/*.sh "$destination/"
  fi
}

WINDOWS_STAGE="$STAGE_ROOT/windows-amd64"
LINUX_AMD64_STAGE="$STAGE_ROOT/linux-amd64"
LINUX_ARM64_STAGE="$STAGE_ROOT/linux-arm64"
mkdir -p "$WINDOWS_STAGE" "$LINUX_AMD64_STAGE" "$LINUX_ARM64_STAGE"

build_target windows amd64 "$WINDOWS_STAGE/gbaselite.exe"
build_target linux amd64 "$LINUX_AMD64_STAGE/gbaselite"
build_target linux arm64 "$LINUX_ARM64_STAGE/gbaselite"
(cd "$REPOSITORY_ROOT" && "$GO_EXECUTABLE" run ./scripts/internal/inspectelf -file "$LINUX_AMD64_STAGE/gbaselite" -machine amd64)
(cd "$REPOSITORY_ROOT" && "$GO_EXECUTABLE" run ./scripts/internal/inspectelf -file "$LINUX_ARM64_STAGE/gbaselite" -machine arm64)
copy_portable_files "$WINDOWS_STAGE" windows
copy_portable_files "$LINUX_AMD64_STAGE" linux
copy_portable_files "$LINUX_ARM64_STAGE" linux

cp "$LINUX_AMD64_STAGE/gbaselite" "$OUTPUT_DIRECTORY/gbaselite-linux-amd64"
cp "$LINUX_ARM64_STAGE/gbaselite" "$OUTPUT_DIRECTORY/gbaselite-linux-arm64"
chmod 0755 "$OUTPUT_DIRECTORY/gbaselite-linux-amd64" "$OUTPUT_DIRECTORY/gbaselite-linux-arm64"

WINDOWS_ARCHIVE="$OUTPUT_DIRECTORY/gbaselite-windows-amd64.zip"
LINUX_AMD64_ARCHIVE="$OUTPUT_DIRECTORY/gbaselite-linux-amd64.tar.gz"
LINUX_ARM64_ARCHIVE="$OUTPUT_DIRECTORY/gbaselite-linux-arm64.tar.gz"
rm -f -- "$WINDOWS_ARCHIVE" "$LINUX_AMD64_ARCHIVE" "$LINUX_ARM64_ARCHIVE"
(cd "$REPOSITORY_ROOT" && "$GO_EXECUTABLE" run ./scripts/internal/archive -format zip -source "$WINDOWS_STAGE" -output "$WINDOWS_ARCHIVE")
(cd "$REPOSITORY_ROOT" && "$GO_EXECUTABLE" run ./scripts/internal/archive -format tar.gz -source "$LINUX_AMD64_STAGE" -output "$LINUX_AMD64_ARCHIVE")
(cd "$REPOSITORY_ROOT" && "$GO_EXECUTABLE" run ./scripts/internal/archive -format tar.gz -source "$LINUX_ARM64_STAGE" -output "$LINUX_ARM64_ARCHIVE")

SBOM_PATH="$OUTPUT_DIRECTORY/sbom.spdx.json"
if command -v syft >/dev/null 2>&1; then
  syft "dir:$REPOSITORY_ROOT" -o "spdx-json=$SBOM_PATH"
else
  rm -f -- "$SBOM_PATH"
fi

(
  cd "$OUTPUT_DIRECTORY"
  files="gbaselite-windows-amd64.zip gbaselite-linux-amd64.tar.gz gbaselite-linux-arm64.tar.gz gbaselite-linux-amd64 gbaselite-linux-arm64"
  if [ -f "sbom.spdx.json" ]; then files="$files sbom.spdx.json"; fi
  sha256sum $files > checksums.txt
)

echo "Created GBaseLite $VERSION release artifacts in $OUTPUT_DIRECTORY"
