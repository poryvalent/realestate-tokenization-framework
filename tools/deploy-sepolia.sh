#!/usr/bin/env bash
# Deploys the AcreSync contracts to Sepolia.
#
# Runs the preflight first and refuses to continue if it fails. Deploying with a wrong
# environment tag or an underfunded relayer is not correctable afterwards, because the
# contracts are immutable.
#
# Usage:
#   deploy-sepolia.sh            dry run, simulates without broadcasting
#   deploy-sepolia.sh --send     broadcasts for real
set -uo pipefail
source "$(dirname "$0")/wsl-env.sh"

BROADCAST=0
VERIFY=0
for arg in "$@"; do
  case "$arg" in
    --send) BROADCAST=1 ;;
    --verify) VERIFY=1 ;;
    *) echo "unknown argument: $arg"; exit 2 ;;
  esac
done

echo "=== preflight ==="
if ! bash "$(dirname "$0")/preflight-sepolia.sh"; then
  echo
  echo "preflight failed; not deploying"
  exit 1
fi

ENV_FILE="$ACRESYNC_ROOT/.env"
getval() {
  grep -E "^$1=" "$ENV_FILE" | tail -1 | cut -d= -f2- | tr -d '"' | tr -d "'" | tr -d '\r'
}

RPC="$(getval ACRESYNC_CHAIN_RPC_URL)"
ENVIRONMENT="$(getval ACRESYNC_ENVIRONMENT)"

if [ "$ENVIRONMENT" != "SEPOLIA_SIM" ]; then
  echo
  echo "refusing to deploy: ACRESYNC_ENVIRONMENT is '$ENVIRONMENT', expected SEPOLIA_SIM."
  echo "The tag is immutable and stamped into every anchor, so a mislabelled deployment"
  echo "cannot be corrected without redeploying the whole scheme."
  exit 1
fi

# The script reads credentials from the process environment via vm.envUint. Exported here
# rather than passed on the command line, because a command line ends up in shell history.
set -a
# shellcheck disable=SC2046
export ACRESYNC_RELAYER_PRIVATE_KEY="$(getval ACRESYNC_RELAYER_PRIVATE_KEY)"
export ACRESYNC_ENVIRONMENT="$ENVIRONMENT"
set +a

cd "$CONTRACTS_DIR"

ARGS=(script script/Deploy.s.sol:Deploy --rpc-url "$RPC")

if [ "$BROADCAST" -eq 1 ]; then
  ARGS+=(--broadcast)
  # A deployment that lands but is not recorded is worse than one that fails outright, so the
  # run is slow-and-verified rather than fast.
  ARGS+=(--slow)
  echo
  echo "=== BROADCASTING to Sepolia ==="
else
  echo
  echo "=== DRY RUN (add --send to broadcast) ==="
fi

if [ "$VERIFY" -eq 1 ]; then
  ETHERSCAN_KEY="$(getval ACRESYNC_ETHERSCAN_API_KEY)"
  if [ -z "$ETHERSCAN_KEY" ]; then
    echo "warn: --verify requested but ACRESYNC_ETHERSCAN_API_KEY is empty; skipping verification"
  else
    export ETHERSCAN_API_KEY="$ETHERSCAN_KEY"
    ARGS+=(--verify)
  fi
fi

forge "${ARGS[@]}"
STATUS=$?

echo
if [ "$STATUS" -ne 0 ]; then
  echo "deployment failed with status $STATUS"
  exit "$STATUS"
fi

if [ "$BROADCAST" -eq 1 ]; then
  echo "Deployment complete. Copy the three addresses printed above into .env, then run:"
  echo "  tools/ceremony-sepolia.sh --reveals 3"
else
  echo "Dry run complete. Re-run with --send to broadcast."
fi
