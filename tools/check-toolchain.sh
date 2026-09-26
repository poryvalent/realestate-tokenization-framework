#!/usr/bin/env bash
source "$(dirname "$0")/wsl-env.sh"

echo "=== versions ==="
forge --version | head -1
anvil --version | head -1

echo
echo "=== supporting tools ==="
for t in git curl wget tar unzip; do
  printf '%-8s ' "$t"
  if command -v "$t" >/dev/null 2>&1; then
    command -v "$t"
  else
    echo MISSING
  fi
done

echo
echo "=== checkout visible from WSL ==="
if [ -d "$ACRESYNC_ROOT" ]; then
  echo "ok: $ACRESYNC_ROOT"
  ls "$ACRESYNC_ROOT"
else
  echo "MISSING: $ACRESYNC_ROOT"
fi
