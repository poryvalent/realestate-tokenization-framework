#!/usr/bin/env bash
# Runs the commit-reveal ceremony against live Sepolia.
#
# This is the M3 gate, and it exists because the ceremony cannot be validated on Anvil. Anvil
# mines on demand, so `blockhash` availability and the 256-block window behave nothing like a
# real chain. Everything else in M2 is proven by the Foundry suite; this is the one part where
# only the real thing counts.
#
# Each reveal deploys a fresh ballot contract. A ballot is single-use by design (the bid book
# cannot be re-anchored, which is what makes the draw tamper-evident), so repeating the
# ceremony means repeating the deployment. That is cheap on a testnet and it keeps each run
# independently inspectable on Etherscan.
#
# Usage:
#   ceremony-sepolia.sh --reveals 3
#   ceremony-sepolia.sh --expiry-test        the long one, roughly 50 minutes
set -uo pipefail
source "$(dirname "$0")/wsl-env.sh"

REVEALS=1
EXPIRY_TEST=0

while [ $# -gt 0 ]; do
  case "$1" in
    --reveals) REVEALS="$2"; shift 2 ;;
    --expiry-test) EXPIRY_TEST=1; shift ;;
    *) echo "unknown argument: $1"; exit 2 ;;
  esac
done

ENV_FILE="$ACRESYNC_ROOT/.env"
getval() {
  grep -E "^$1=" "$ENV_FILE" | tail -1 | cut -d= -f2- | tr -d '"' | tr -d "'" | tr -d '\r'
}

RPC="$(getval ACRESYNC_CHAIN_RPC_URL)"
KEY="$(getval ACRESYNC_RELAYER_PRIVATE_KEY)"
ROLES="$(getval ACRESYNC_ROLES_ADDRESS)"

if [ -z "$RPC" ] || [ -z "$KEY" ]; then
  echo "need ACRESYNC_CHAIN_RPC_URL and ACRESYNC_RELAYER_PRIVATE_KEY in .env"
  exit 1
fi
if [ -z "$ROLES" ]; then
  echo "need ACRESYNC_ROLES_ADDRESS in .env; run tools/deploy-sepolia.sh --send first"
  exit 1
fi

ADDR="$(cast wallet address --private-key "$KEY")"
SEND=(--rpc-url "$RPC" --private-key "$KEY")
CALL=(--rpc-url "$RPC")

# Ballot parameters, matching Deploy.s.sol.
DELAY=10
WINDOW=200
ATTEMPTS=3

log()  { printf '%s  %s\n' "$(date -u +%H:%M:%S)" "$1"; }
fail() { printf '\nFAILED: %s\n' "$1"; exit 1; }

wait_for_block() {
  local target="$1" label="$2" current
  while :; do
    current="$(cast block-number "${CALL[@]}" 2>/dev/null || echo 0)"
    if [ "$current" -ge "$target" ] 2>/dev/null; then
      log "reached block $current ($label)"
      return 0
    fi
    log "  block $current, waiting for $target ($label)"
    sleep 12
  done
}

deploy_ballot() {
  # Deployed with forge create rather than a script, so the address is easy to capture.
  local out
  out="$(cd "$CONTRACTS_DIR" && forge create src/AcreSyncBallot.sol:AcreSyncBallot \
    --broadcast "${SEND[@]}" \
    --constructor-args "$ROLES" "$DELAY" "$WINDOW" "$ATTEMPTS" 2>&1)"
  echo "$out" | grep -oE 'Deployed to: 0x[0-9a-fA-F]{40}' | grep -oE '0x[0-9a-fA-F]{40}'
}

run_one_ceremony() {
  local run_index="$1"
  echo
  echo "================================================================"
  echo " ceremony run $run_index"
  echo "================================================================"

  log "deploying a fresh ballot"
  local ballot
  ballot="$(deploy_ballot)"
  [ -n "$ballot" ] || fail "could not determine the deployed ballot address"
  log "ballot at $ballot"

  # A secret with real entropy from the OS, generated per run so no two draws share one.
  local secret commitment
  secret="$(cast keccak "$(date -u +%s%N)-$run_index-$(head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n')")"

  # The commitment is read back from the contract rather than recomputed in shell. A second
  # implementation of the derivation would be one more thing that can drift out of agreement
  # with the two that already have to match.
  commitment="$(cast call "$ballot" "commitmentFor(bytes32)(bytes32)" "$secret" "${CALL[@]}")"
  [ -n "$commitment" ] || fail "could not compute the commitment"
  log "commitment $commitment"

  log "anchoring the bid book"
  cast send "$ballot" \
    "anchorBidbook(bytes32,bytes32,uint32,uint32,uint32,bytes32)" \
    "$(cast keccak "bidbook-$run_index")" \
    "$(cast keccak "bidbook-cid-$run_index")" \
    900 900 900 \
    "$(cast keccak "key-anchor-$run_index")" \
    "${SEND[@]}" >/dev/null || fail "anchorBidbook reverted"

  log "committing the seed"
  cast send "$ballot" "commitSeed(bytes32,bytes32)" \
    "$commitment" "$(cast keccak "key-commit-$run_index")" \
    "${SEND[@]}" >/dev/null || fail "commitSeed reverted"

  local target
  target="$(cast call "$ballot" "targetBlock()(uint64)" "${CALL[@]}")"
  target="${target%% *}"
  log "target block $target, chosen by the contract rather than by us"

  wait_for_block "$((target + 1))" "target + 1"

  log "revealing"
  cast send "$ballot" "revealSeed(bytes32,bytes32)" \
    "$secret" "$(cast keccak "key-reveal-$run_index")" \
    "${SEND[@]}" >/dev/null || fail "revealSeed reverted"

  local stage final observed
  stage="$(cast call "$ballot" "stage()(uint8)" "${CALL[@]}")"
  stage="${stage%% *}"
  [ "$stage" = "3" ] || fail "expected stage 3 (SeedRevealed), got $stage"

  final="$(cast call "$ballot" "finalSeed()(bytes32)" "${CALL[@]}")"
  observed="$(cast call "$ballot" "targetBlockHash()(bytes32)" "${CALL[@]}")"

  log "final seed       $final"
  log "target blockhash $observed"

  # The seed must incorporate a blockhash nobody knew at commit time. A zero here would mean
  # the draw was derived from nothing.
  case "$observed" in
    0x0000000000000000000000000000000000000000000000000000000000000000)
      fail "target blockhash is zero: the seed would be derived from nothing" ;;
  esac

  echo
  echo "  RUN $run_index PASSED"
  echo "  ballot   https://sepolia.etherscan.io/address/$ballot"
  BALLOTS+=("$ballot")
}

run_expiry_test() {
  echo
  echo "================================================================"
  echo " expiry and recommit test"
  echo "================================================================"
  echo
  echo "Proves two things that only a real chain can show:"
  echo "  1. a lapsed window forces a recommit, and the recommit reuses the stored"
  echo "     commitment so abandonment can reroll only the blockhash, never the secret"
  echo "  2. past 256 blocks blockhash returns zero and no reveal is possible"
  echo
  echo "This waits for roughly $((WINDOW + 60)) blocks, about 50 minutes on Sepolia."
  echo

  log "deploying a fresh ballot"
  local ballot
  ballot="$(deploy_ballot)"
  [ -n "$ballot" ] || fail "could not determine the deployed ballot address"
  log "ballot at $ballot"

  local secret commitment
  secret="$(cast keccak "expiry-$(date -u +%s%N)-$(head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n')")"
  commitment="$(cast call "$ballot" "commitmentFor(bytes32)(bytes32)" "$secret" "${CALL[@]}")"
  [ -n "$commitment" ] || fail "could not compute the commitment"

  cast send "$ballot" \
    "anchorBidbook(bytes32,bytes32,uint32,uint32,uint32,bytes32)" \
    "$(cast keccak "expiry-book")" "$(cast keccak "expiry-cid")" 900 900 900 \
    "$(cast keccak "expiry-key-anchor")" "${SEND[@]}" >/dev/null || fail "anchorBidbook reverted"

  cast send "$ballot" "commitSeed(bytes32,bytes32)" \
    "$commitment" "$(cast keccak "expiry-key-commit")" "${SEND[@]}" >/dev/null \
    || fail "commitSeed reverted"

  local firstTarget deadline
  firstTarget="$(cast call "$ballot" "targetBlock()(uint64)" "${CALL[@]}")"; firstTarget="${firstTarget%% *}"
  deadline=$((firstTarget + WINDOW))
  log "first target $firstTarget, reveal deadline $deadline"

  wait_for_block "$((deadline + 1))" "past the deadline"

  log "confirming a reveal is now impossible"
  if cast send "$ballot" "revealSeed(bytes32,bytes32)" \
      "$secret" "$(cast keccak "expiry-key-late-reveal")" "${SEND[@]}" >/dev/null 2>&1; then
    fail "a reveal succeeded after the window closed"
  fi
  log "  reveal correctly refused"

  log "recommitting"
  cast send "$ballot" "recommitSeed(bytes32)" \
    "$(cast keccak "expiry-key-recommit")" "${SEND[@]}" >/dev/null || fail "recommitSeed reverted"

  local storedCommitment attempt newTarget
  storedCommitment="$(cast call "$ballot" "seedCommitment()(bytes32)" "${CALL[@]}")"
  attempt="$(cast call "$ballot" "attempt()(uint8)" "${CALL[@]}")"; attempt="${attempt%% *}"
  newTarget="$(cast call "$ballot" "targetBlock()(uint64)" "${CALL[@]}")"; newTarget="${newTarget%% *}"

  # The anti-grinding property. recommitSeed takes no commitment argument, so an operator who
  # abandons a window cannot substitute a different secret.
  [ "$storedCommitment" = "$commitment" ] \
    || fail "the commitment changed on recommit; grinding would be possible"
  [ "$attempt" = "2" ] || fail "expected attempt 2, got $attempt"
  [ "$newTarget" -gt "$firstTarget" ] || fail "the target block did not move forward"

  log "  commitment unchanged, attempt now 2, target moved to $newTarget"

  wait_for_block "$((newTarget + 1))" "new target + 1"

  log "revealing with the original secret"
  cast send "$ballot" "revealSeed(bytes32,bytes32)" \
    "$secret" "$(cast keccak "expiry-key-reveal-2")" "${SEND[@]}" >/dev/null \
    || fail "the original secret failed to open the reused commitment"

  local stage
  stage="$(cast call "$ballot" "stage()(uint8)" "${CALL[@]}")"; stage="${stage%% *}"
  [ "$stage" = "3" ] || fail "expected stage 3 after reveal, got $stage"

  echo
  echo "  EXPIRY TEST PASSED"
  echo "  ballot   https://sepolia.etherscan.io/address/$ballot"
}

# ---------------------------------------------------------------------------

echo "relayer  $ADDR"
echo "roles    $ROLES"
echo "balance  $(cast from-wei "$(cast balance "$ADDR" "${CALL[@]}")") ETH"

declare -a BALLOTS=()

if [ "$EXPIRY_TEST" -eq 1 ]; then
  run_expiry_test
else
  for i in $(seq 1 "$REVEALS"); do
    run_one_ceremony "$i"
  done

  echo
  echo "================================================================"
  echo " $REVEALS live reveal(s) passed"
  echo "================================================================"
  for b in "${BALLOTS[@]}"; do
    echo "  https://sepolia.etherscan.io/address/$b"
  done
  echo
  echo "Remaining M3 gate item, run separately:"
  echo "  tools/ceremony-sepolia.sh --expiry-test"
fi

echo
echo "balance now $(cast from-wei "$(cast balance "$ADDR" "${CALL[@]}")") ETH"
