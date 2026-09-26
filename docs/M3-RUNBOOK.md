# M3 Runbook: Sepolia deployment and the commit-reveal ceremony

## Why this milestone exists separately

Everything else in the contracts is proven by 118 Foundry tests against Anvil. The ceremony is
the exception, and it cannot be validated there.

Anvil mines on demand. `blockhash` availability, the 256-block window past which it returns
zero, and the real gap between committing and a block being produced all behave differently on
a chain with actual block times and actual proposers. A ceremony that passes on Anvil tells you
the state machine is right; it tells you nothing about whether the timing assumptions hold.

So M3 puts only Roles and Ballot on Sepolia and exercises the ceremony for real, before the
orchestrator exists to complicate the picture.

## Gate

M3 is complete when all four hold:

1. Three live reveals succeed, each with a non-zero target blockhash folded into the seed.
2. One lapsed reveal window forces a recommit, and the recommit provably reuses the stored
   commitment.
3. The original secret still opens the reused commitment after the recommit.
4. Wall-clock timings are recorded, so the M9 rehearsal can be planned rather than guessed.

## Before you start

```bash
wsl -d Ubuntu bash tools/preflight-sepolia.sh
```

Must print `PREFLIGHT PASSED`. It checks the key format, RPC reachability, that the chain id is
actually Sepolia, the relayer balance, and that `.env` has no duplicate keys.

`ACRESYNC_ENVIRONMENT` must be `SEPOLIA_SIM`. The deploy script refuses otherwise, and the
refusal is not pedantry: `environmentTag` and `isSimulation` are immutable and stamped into
every anchor, so a deployment mislabelled `LOCAL` misdescribes itself permanently and the only
remedy is redeploying the whole scheme.

Budget roughly 0.02 ETH to deploy and 0.02 for the ceremony. 0.05 is comfortable.

## Step 1: deploy

Dry run first. It exercises the full script including the environment-tag guard and the
post-deployment assertions, and spends nothing.

```bash
wsl -d Ubuntu bash tools/deploy-sepolia.sh
```

Then broadcast:

```bash
wsl -d Ubuntu bash tools/deploy-sepolia.sh --send
```

Deployment order is forced by the one-directional references: Roles, then Ballot, then Scheme.
Nothing points backwards, so there is no initialisation step afterwards and no window in which
a contract exists but is not yet wired.

Copy the three printed addresses into `.env`.

The script reads back what was actually deployed rather than trusting the constructor
arguments, and asserts that units times unit price lands on fifty crore rupees. That check
exists because a scheme whose asset value falls outside the SM-REIT band cannot legally exist,
and deploying one immutably is not a mistake you can walk back.

## Step 2: three live reveals

```bash
wsl -d Ubuntu bash tools/ceremony-sepolia.sh --reveals 3
```

Each run deploys a fresh ballot. That is not waste: a ballot is single-use by design, because
the bid book cannot be re-anchored, and that immutability is what makes the draw
tamper-evident. Repeating the ceremony means repeating the deployment, which is cheap here and
leaves each run independently inspectable on Etherscan.

Per run the script anchors a bid book, commits a secret with OS entropy, waits for the
contract-chosen target block, reveals, and asserts the resulting stage and a non-zero target
blockhash.

Expect roughly two to three minutes per run: ten blocks of seed delay at about twelve seconds a
block, plus confirmation time.

What to watch for: the target block is chosen by the contract, never passed in. If a caller
could choose it, they would pick one whose hash they had reason to prefer, and the ceremony
would prove nothing.

## Step 3: the expiry test

Run this separately. It is the long one.

```bash
wsl -d Ubuntu bash tools/ceremony-sepolia.sh --expiry-test
```

Roughly 50 minutes, because it deliberately waits out a 200-block reveal window to prove two
things a shorter test cannot:

A lapsed window makes a reveal impossible, and the operator must recommit. Without a recommit
path a single missed transaction would strand an offer permanently.

The recommit reuses the stored commitment. `recommitSeed` takes no commitment argument at all,
so an operator who dislikes a draw and lets the window lapse can reroll the blockhash but never
the secret. The script asserts the stored commitment is byte-identical after the recommit, that
the attempt counter moved to 2, that the target block moved forward, and that the original
secret still opens it.

### The residual risk, stated plainly

This does not reduce manipulation to zero. An operator colluding with a block proposer who
withholds at the target block retains some influence, and abandonment grinding is bounded
rather than eliminated: three public attempts, each emitted on-chain, then trustee escalation.

On a testnet demo that is negligible. On mainnet with real money it would warrant a VRF or a
substantially longer delay. Saying so is better than claiming bulletproof and being asked the
question in a diligence session.

## Recording the timings

Note the wall-clock duration of each reveal. M9 needs them to plan the live rehearsal, and the
constraint they encode is real: business time compresses a thirty-day offer to a second, but
five confirmations and ten blocks of seed delay do not compress at all. Roughly eight anchors
across a full demo means eight to ten minutes of unavoidable waiting, which is why the
rehearsal pre-anchors setup and performs only the ballot and the period anchor live.

## If something goes wrong

**Reveal reverts `TargetBlockNotReached`** — the target block has not been mined yet. The script
polls, so this only appears if invoked manually. Check `revealWindow()`.

**Reveal reverts `RevealWindowExpired`** — more than 200 blocks passed. Use `recommitSeed`. This
is the normal path the expiry test exercises deliberately.

**Reveal reverts `BlockhashUnavailable`** — more than 256 blocks passed, so the EVM no longer
retains the hash. The window cap of 200 exists to keep this unreachable in practice; seeing it
means the window configuration drifted above the EVM limit.

**`CommitmentMismatch`** — the secret does not open the stored commitment. Check with
`secretMatchesCommitment(bytes32)` before spending gas on a reveal.

**RPC rate limiting** — the expiry test polls for the better part of an hour. A free Alchemy or
Infura key handles it; a public endpoint often will not.

**Transaction stuck pending** — Sepolia base fee spikes. `cast send` waits for a receipt. If it
hangs, check the tx on Etherscan and resubmit with a higher priority fee. The ceremony is
idempotent per step via its idempotency keys, so a resubmitted step that already landed reverts
`KeyAlreadyUsed` rather than applying twice.

## A note on the demo key configuration

All four roles default to the deployer, and the script says so in its output. That is fine for a
demo and wrong for production: the asymmetry in `AcreSyncRoles` between timelocked grants and
immediate relayer revocation only protects anything when admin and relayer are held separately.
A single key holding both can, given the timelock, grant itself anything.

The role timelock is one hour here rather than the 48 hours a production deployment would use.
It runs on real `block.timestamp` and cannot be compressed by the simulated business clock, so
a production value would be undemonstrable. Better to use a short one openly than to have
someone find it in the constructor arguments.
