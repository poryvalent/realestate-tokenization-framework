#!/usr/bin/env bash
# Checks everything M3 needs before a Sepolia deployment is attempted.
#
# Deliberately prints no secret values. The relayer address is derived from the key and
# shown, because an address is public and an operator needs to confirm which account is
# about to spend gas. The key itself is only ever passed to cast, never echoed.
set -uo pipefail
source "$(dirname "$0")/wsl-env.sh"

ENV_FILE="$ACRESYNC_ROOT/.env"
FAIL=0

say_ok()   { printf '  ok      %s\n' "$1"; }
say_warn() { printf '  warn    %s\n' "$1"; }
say_fail() { printf '  MISSING %s\n' "$1"; FAIL=1; }

if [ ! -f "$ENV_FILE" ]; then
  echo "no .env at $ENV_FILE"
  exit 1
fi

# Read values without exporting the whole file into the environment.
getval() {
  grep -E "^$1=" "$ENV_FILE" | tail -1 | cut -d= -f2- | tr -d '"' | tr -d "'" | tr -d '\r'
}

# A duplicated key is reported rather than silently resolved, because the Go loader treats it
# as a hard error and a preflight that quietly disagreed with the loader would be worse than
# no preflight.
check_duplicates() {
  local dupes
  dupes="$(grep -oE '^ACRESYNC_[A-Z0-9_]+' "$ENV_FILE" | sort | uniq -d)"
  if [ -n "$dupes" ]; then
    echo "  MISSING duplicate keys in .env, the loader will refuse to start:"
    printf '            %s\n' $dupes
    FAIL=1
  fi
}

RPC="$(getval ACRESYNC_CHAIN_RPC_URL)"
KEY="$(getval ACRESYNC_RELAYER_PRIVATE_KEY)"
CHAIN_ID="$(getval ACRESYNC_CHAIN_ID)"
ENVIRONMENT="$(getval ACRESYNC_ENVIRONMENT)"
DEV_PEPPER="$(getval ACRESYNC_ANCHOR_PEPPER_DEV)"
DB_URL="$(getval ACRESYNC_DATABASE_URL)"

echo "=== credentials ==="

check_duplicates

if [ -z "$RPC" ]; then
  say_fail "ACRESYNC_CHAIN_RPC_URL is empty"
else
  say_ok "RPC URL present"
fi

if [ -z "$KEY" ]; then
  say_fail "ACRESYNC_RELAYER_PRIVATE_KEY is empty"
elif ! printf '%s' "$KEY" | grep -qE '^0x[0-9a-fA-F]{64}$'; then
  say_fail "relayer key is not 0x followed by 64 hex characters"
else
  say_ok "relayer key well formed"
fi

echo
echo "=== environment ==="

# A Sepolia deployment must be tagged SEPOLIA_SIM. The tag is written into every anchor so
# a simulated record can never be mistaken for a live one, and LOCAL on a public testnet
# would defeat that.
if [ "$ENVIRONMENT" = "SEPOLIA_SIM" ]; then
  say_ok "ACRESYNC_ENVIRONMENT=SEPOLIA_SIM"
else
  say_warn "ACRESYNC_ENVIRONMENT=$ENVIRONMENT — set to SEPOLIA_SIM before deploying, so the"
  printf '          environment tag baked into every anchor matches reality\n'
fi

# The config loader refuses a development pepper outside LOCAL: erasure guarantees depend on
# the pepper living in a KMS where it can be destroyed, and a value that has sat in an env
# file cannot be proven destroyed.
if [ -n "$DEV_PEPPER" ] && [ "$ENVIRONMENT" != "LOCAL" ]; then
  say_fail "ACRESYNC_ANCHOR_PEPPER_DEV is set but ENVIRONMENT is not LOCAL; the loader will refuse to start"
else
  say_ok "anchor pepper configuration consistent"
fi

if [ -z "$DB_URL" ]; then
  say_warn "ACRESYNC_DATABASE_URL empty — not needed for M3, needed from M5"
else
  say_ok "database URL present"
fi

echo
echo "=== chain reachability ==="

if [ -z "$RPC" ]; then
  say_fail "cannot check: no RPC URL"
else
  REMOTE_ID="$(cast chain-id --rpc-url "$RPC" 2>/dev/null || true)"
  if [ -z "$REMOTE_ID" ]; then
    say_fail "RPC did not respond to chain-id; check the URL and any rate limit"
  elif [ "$REMOTE_ID" != "11155111" ]; then
    say_fail "RPC reports chain id $REMOTE_ID, expected 11155111 (Sepolia)"
  else
    say_ok "RPC reachable, chain id 11155111 (Sepolia)"
    if [ "$CHAIN_ID" != "11155111" ]; then
      say_fail "ACRESYNC_CHAIN_ID=$CHAIN_ID disagrees with the RPC"
    fi

    BLOCK="$(cast block-number --rpc-url "$RPC" 2>/dev/null || echo '?')"
    say_ok "current block $BLOCK"
  fi
fi

echo
echo "=== relayer account ==="

if [ -z "$KEY" ] || [ -z "$RPC" ]; then
  say_fail "cannot check balance without both a key and an RPC URL"
else
  ADDR="$(cast wallet address --private-key "$KEY" 2>/dev/null || true)"
  if [ -z "$ADDR" ]; then
    say_fail "could not derive an address from the key"
  else
    printf '  address %s\n' "$ADDR"

    WEI="$(cast balance "$ADDR" --rpc-url "$RPC" 2>/dev/null || echo 0)"
    ETH="$(cast from-wei "$WEI" 2>/dev/null || echo 0)"
    printf '  balance %s ETH\n' "$ETH"

    # Three deployments plus the full ceremony and a settlement run. 0.05 is comfortable;
    # below 0.02 the 256-block expiry test is likely to strand mid-way.
    ENOUGH="$(cast to-unit 20000000000000000 wei 2>/dev/null || echo 20000000000000000)"
    if [ "$(printf '%s\n%s\n' "$WEI" "$ENOUGH" | sort -g | head -1)" = "$WEI" ] && [ "$WEI" != "$ENOUGH" ]; then
      say_warn "below 0.02 ETH; top up before running the 256-block expiry test"
    else
      say_ok "balance sufficient for deployment and the full ceremony"
    fi
  fi
fi

echo
if [ "$FAIL" -eq 0 ]; then
  echo "PREFLIGHT PASSED — ready to deploy"
else
  echo "PREFLIGHT FAILED — resolve the MISSING items above"
fi
exit "$FAIL"
