#!/usr/bin/env bash
set -euo pipefail
source "$(dirname "$0")/wsl-env.sh"

# forge-std is vendored with a shallow clone rather than `forge install`.
#
# forge install creates a git submodule, which requires the parent directory to be a
# git repository. The AcreSync checkout is not one yet, and making it one as a side
# effect of installing a test dependency would be a surprising thing for this script
# to do. A pinned shallow clone gives the same result with no assumptions about the
# surrounding repo.
FORGE_STD_TAG="v1.11.0"
LIB_DIR="$CONTRACTS_DIR/lib"
mkdir -p "$LIB_DIR"

if [ -d "$LIB_DIR/forge-std/src" ]; then
  echo "forge-std already present at $FORGE_STD_TAG"
else
  echo "cloning forge-std $FORGE_STD_TAG ..."
  rm -rf "$LIB_DIR/forge-std"
  git clone --depth 1 --branch "$FORGE_STD_TAG" \
    https://github.com/foundry-rs/forge-std "$LIB_DIR/forge-std"
  # The clone's own git metadata is not wanted inside our tree.
  rm -rf "$LIB_DIR/forge-std/.git"
fi

echo
echo "forge-std contents:"
ls "$LIB_DIR/forge-std/src" | head -20

echo
cd "$CONTRACTS_DIR"
echo "=== forge build ==="
forge build
