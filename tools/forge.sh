#!/usr/bin/env bash
# Runs a forge subcommand in the contracts directory.
#
# Usage from PowerShell:
#   wsl -d Ubuntu bash "/mnt/c/.../tools/forge.sh" test -vv
set -euo pipefail
source "$(dirname "$0")/wsl-env.sh"
cd "$CONTRACTS_DIR"
exec forge "$@"
