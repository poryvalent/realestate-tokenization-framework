#!/usr/bin/env bash
# Reads the deployed contracts back from chain and checks them against the locked v1 parameters.
#
# Separate from the deploy script's own assertions on purpose. Those run inside the same process
# that did the deploying; this one starts from nothing but the addresses in .env, which is how an
# auditor or a colleague would check the work.
set -uo pipefail
source "$(dirname "$0")/wsl-env.sh"

ENV_FILE="$ACRESYNC_ROOT/.env"
getval() { grep -E "^$1=" "$ENV_FILE" | tail -1 | cut -d= -f2- | tr -d '"' | tr -d "'" | tr -d '\r'; }

RPC="$(getval ACRESYNC_CHAIN_RPC_URL)"
ROLES="$(getval ACRESYNC_ROLES_ADDRESS)"
BALLOT="$(getval ACRESYNC_BALLOT_ADDRESS)"
SCHEME="$(getval ACRESYNC_SCHEME_ADDRESS)"

FAIL=0
check() {
  local label="$1" expected="$2" actual="$3"
  actual="${actual%% *}"
  if [ "$actual" = "$expected" ]; then
    printf '  ok      %-28s %s\n' "$label" "$actual"
  else
    printf '  MISMATCH %-27s got %s, expected %s\n' "$label" "$actual" "$expected"
    FAIL=1
  fi
}

if [ -z "$ROLES" ] || [ -z "$BALLOT" ] || [ -z "$SCHEME" ]; then
  echo "addresses missing from .env; deploy first"
  exit 1
fi

echo "roles   $ROLES"
echo "ballot  $BALLOT"
echo "scheme  $SCHEME"
echo

echo "=== roles ==="
# A Sepolia deployment must identify itself as a simulation, and the tag is immutable.
check "environmentTag (SEPOLIA_SIM)" "1" "$(cast call "$ROLES" 'environmentTag()(uint8)' --rpc-url "$RPC")"
check "isSimulation" "true" "$(cast call "$ROLES" 'isSimulation()(bool)' --rpc-url "$RPC")"
check "roleTimelockSeconds" "3600" "$(cast call "$ROLES" 'roleTimelockSeconds()(uint32)' --rpc-url "$RPC")"

echo
echo "=== scheme: the locked SM-REIT parameters ==="
# 500 units at ten lakh rupees is fifty crore, the band floor. 25 is the 5% an unleveraged
# scheme's manager holds, leaving 475 public. At least 200 distinct unitholders, and at least
# 95% of NDCF distributed.
check "unitPricePaise" "100000000" "$(cast call "$SCHEME" 'unitPricePaise()(uint64)' --rpc-url "$RPC")"
check "totalUnits" "500" "$(cast call "$SCHEME" 'totalUnits()(uint32)' --rpc-url "$RPC")"
check "imUnits" "25" "$(cast call "$SCHEME" 'imUnits()(uint32)' --rpc-url "$RPC")"
check "publicUnits" "475" "$(cast call "$SCHEME" 'publicUnits()(uint32)' --rpc-url "$RPC")"
check "minPublicHolders" "200" "$(cast call "$SCHEME" 'minPublicHolders()(uint16)' --rpc-url "$RPC")"
check "distributionFloorBps" "9500" "$(cast call "$SCHEME" 'distributionFloorBps()(uint16)' --rpc-url "$RPC")"
# Units are indivisible: a Demat account cannot hold a fraction.
check "decimals" "0" "$(cast call "$SCHEME" 'decimals()(uint8)' --rpc-url "$RPC")"
check "maxBatchSize" "100" "$(cast call "$SCHEME" 'maxBatchSize()(uint32)' --rpc-url "$RPC")"

echo
echo "=== scheme: wiring ==="
WIRED_ROLES="$(cast call "$SCHEME" 'roles()(address)' --rpc-url "$RPC")"
WIRED_BALLOT="$(cast call "$SCHEME" 'ballot()(address)' --rpc-url "$RPC")"
check "roles pointer" "$(echo "$ROLES" | tr 'A-Z' 'a-z')" "$(echo "${WIRED_ROLES%% *}" | tr 'A-Z' 'a-z')"
check "ballot pointer" "$(echo "$BALLOT" | tr 'A-Z' 'a-z')" "$(echo "${WIRED_BALLOT%% *}" | tr 'A-Z' 'a-z')"

echo
echo "=== ballot ==="
check "seedDelayBlocks" "10" "$(cast call "$BALLOT" 'seedDelayBlocks()(uint32)' --rpc-url "$RPC")"
# Must stay below the EVM's 256-block blockhash horizon, or no reveal could ever succeed.
check "seedRevealWindowBlocks" "200" "$(cast call "$BALLOT" 'seedRevealWindowBlocks()(uint32)' --rpc-url "$RPC")"
check "maxSeedAttempts" "3" "$(cast call "$BALLOT" 'maxSeedAttempts()(uint8)' --rpc-url "$RPC")"

echo
echo "=== the absent ERC20 write surface ==="
# transfer, approve and transferFrom are omitted rather than implemented as reverting stubs, so
# there is no function selector for a wallet or an aggregator to call. A user-initiated movement
# would desync this mirror from the depository, and the depository is the register that legally
# counts.
#
# Checked by scanning the deployed bytecode for each selector rather than by attempting a call.
# A failed call is weak evidence: it could fail from a revert, a gas issue, or an RPC quirk.
# Absence of the selector from the runtime code is conclusive, because the dispatcher cannot
# route to a function whose selector it does not contain.
CODE="$(cast code "$SCHEME" --rpc-url "$RPC" 2>/dev/null)"
if [ -z "$CODE" ] || [ "$CODE" = "0x" ]; then
  echo "  MISMATCH no bytecode at the scheme address"
  FAIL=1
else
  printf '  code size %s bytes\n' "$(( (${#CODE} - 2) / 2 ))"

  selector_absent() {
    local sig="$1" sel
    sel="$(cast sig "$sig" | sed 's/^0x//')"
    if printf '%s' "$CODE" | grep -qi "$sel"; then
      printf '  MISMATCH %-34s selector %s present in bytecode\n' "$sig" "0x$sel"
      FAIL=1
    else
      printf '  ok      %-35s selector %s absent from bytecode\n' "$sig" "0x$sel"
    fi
  }

  selector_absent 'transfer(address,uint256)'
  selector_absent 'approve(address,uint256)'
  selector_absent 'transferFrom(address,address,uint256)'
  selector_absent 'allowance(address,address)'

  # The read surface must be present, so wallets and explorers still render holdings.
  selector_present() {
    local sig="$1" sel
    sel="$(cast sig "$sig" | sed 's/^0x//')"
    if printf '%s' "$CODE" | grep -qi "$sel"; then
      printf '  ok      %-35s selector %s present, as intended\n' "$sig" "0x$sel"
    else
      printf '  MISMATCH %-34s selector %s missing; wallets will not show balances\n' "$sig" "0x$sel"
      FAIL=1
    fi
  }

  selector_present 'balanceOf(address)'
  selector_present 'totalSupply()'
fi

echo
echo "=== derived ==="
echo "  asset value  500 units x 100000000 paise = 50,00,00,00,000 paise (Rs 50 crore, the band floor)"
echo "  explorer     https://sepolia.etherscan.io/address/$SCHEME"

echo
if [ "$FAIL" -eq 0 ]; then
  echo "DEPLOYMENT VERIFIED"
else
  echo "DEPLOYMENT MISMATCH — investigate before proceeding"
fi
exit "$FAIL"
