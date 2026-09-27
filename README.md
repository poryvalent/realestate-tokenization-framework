# AcreSync

A SEBI-compliant SM-REIT platform: traditional escrow for money, an on-chain shadow ledger for evidence.

The chain **attests and never custodies**. Rupees move through regulated bank rails; what goes on-chain is a
commitment to what happened, so an investor can verify their own allotment and entitlement without being
given access to anything of ours.

Target scheme: ₹50 crore across **500 units** at ₹10 lakh each. The investment manager holds 25 and the
public holds 475. At least **200 distinct unitholders** and at least **95% of NDCF** distributed each period.

---

## Status

| Milestone | Scope | State |
|---|---|---|
| M0 | Canonical JSON, money/paise, idempotency, config, clock, 12 SQL migrations | Complete |
| M1 | Merkle, ballot allocation engine, distribution maths | Complete |
| M2 | Solidity: roles, ballot, scheme, encodings | Complete |
| M3 | Deployed and verified on Sepolia, live commit–reveal ceremony | Complete |
| M4 | Transactional outbox, chain client, relayer, Anvil chaos tests | Complete |
| M5 | IPFS pinning, NDCF, snapshots, entitlements, payouts | Complete |
| M6 | Primary market: offer → ASBA → bid book → ballot → settled cap table | Complete |
| M7 | Divergence detection and reversal | Not started |
| M8 | Frontend | Not started |
| M9 | Full Sepolia rehearsal | Not started |

**796 Go tests, 131 Solidity tests, `go vet` clean.**

## Deployed on Sepolia (chain 11155111)

| Contract | Address |
|---|---|
| `AcreSyncRoles` | [`0x21d409cb5470fd3bcda344731d945c34cb13b53f`](https://sepolia.etherscan.io/address/0x21d409cb5470fd3bcda344731d945c34cb13b53f) |
| `AcreSyncBallot` | [`0x63615652d8ff9190454229c2add1f173d28b32d5`](https://sepolia.etherscan.io/address/0x63615652d8ff9190454229c2add1f173d28b32d5) |
| `AcreSyncScheme` | [`0xa656a42974b40cf64f32e758abb0689a2a178391`](https://sepolia.etherscan.io/address/0xa656a42974b40cf64f32e758abb0689a2a178391) |

Source-verified. The ballot's commit–reveal ceremony has been exercised live, including a 200-block
reveal-window expiry.

## What is real and what is simulated

Stated plainly, because the distinction is the first thing anyone doing diligence should ask about.

**Live and verifiable by a third party**
- The contracts on Sepolia, source-verified, immutable, with no proxy.
- The commit–reveal ballot: the bid book root is anchored before a seed exists, and the seed mixes a
  future block hash nobody can predict at commit time.
- The 95%-of-NDCF floor and the 200-unitholder floor, enforced in Solidity.
- Merkle inclusion proofs for bids, allotments and entitlements.

**Simulated**
- **The final fiat hop.** RazorpayX provisions the gateway identity but gates the banking API behind
  corporate KYC, so `POST /v1/payouts` returns 404 for our sandbox keys. Everything up to that call is
  validated against the live API: authentication, request encoding, response parsing, field validation,
  beneficiary registration and content-idempotency. The payout call itself is unproven and the provider
  runs as `MOCK` until there is a corporate entity to register.
- **ASBA fund blocking**, for the same reason. Money is modelled as blocked in the investor's own account
  and never held by us, because that is what ASBA actually is — there is deliberately no house balance
  anywhere in the code, since modelling one would model a custody arrangement we are not licensed for.
- **IPFS and KYC** default to mock providers; a Pinata adapter exists and is cross-checked against
  `go-cid`.

## Design decisions worth knowing

- **SHA-256 everywhere, not keccak256.** One hash function across Merkle trees, document digests,
  idempotency keys and the seed commitment, because a third party reimplementing verification should
  need one primitive from their standard library. The cost is that `sha256` is an EVM precompile, so a
  staticcall to it consumes a `vm.prank` — a trap the contract tests document.
- **Immutable contracts, no proxy.** An upgradeable audit trail is a contradiction. A bug means deploying
  v2 and anchoring the migration.
- **Ranking by a per-bid key, not a shuffle.** Verifying one bid's position requires recomputing one
  hash, not replaying a permutation and reimplementing a PRNG.
- **The anchored commitment is the document digest, not the CID.** A CID is a locator; if it stopped
  resolving it would take the commitment with it. Verification is: fetch bytes, hash them, compare.
- **Every bid gets a published outcome, including the losers.** A book of 490 bids for 475 units produces
  490 allocations. Publishing only winners would make the draw unfalsifiable for exactly the people with
  the strongest reason to check it.
- **No PII on-chain.** Investors appear as an HMAC anchor under a key held in KMS. Destroying that key
  severs every linkage at once, which is the only way a DPDP erasure request can be honoured against
  data already written to an immutable ledger.

## Layout

```
contracts/      Solidity sources, tests, deploy script (Foundry)
db/migrations/  12 forward-only SQL migrations
orchestrator/   Go services: 26 internal packages plus cmd tools
docs/           Runbooks
tools/          WSL wrappers for Foundry
```

The Go orchestrator is the sole authorised relayer. Every chain call goes through a transactional outbox
with an idempotency key, so a dropped connection cannot double-anchor.

## Running it

**Prerequisites:** Go 1.26+, Docker, Foundry.

Clone with submodules — `forge-std` is pinned as a submodule, so the contract tests need it:

```bash
git clone --recurse-submodules https://github.com/poryvalent/realestate-tokenization-framework.git
# already cloned?
git submodule update --init --recursive
```

Contract tests (131):

```bash
cd contracts && forge test
```

Go tests. Most are pure and need nothing; the end-to-end suite needs Postgres:

```bash
cd orchestrator
go test ./...          # skips the e2e suite without a database
```

With Postgres, which also runs the end-to-end offer-to-cap-table test:

```bash
docker run -d --name acresync-db -p 55432:5432 \
  -e POSTGRES_PASSWORD=postgres -e POSTGRES_DB=acresync postgres:16

# Create the three roles migration 0009 expects. Required once on plain Postgres;
# unnecessary on Supabase, where they already exist. Skipping this fails at 0009
# with 'role "anon" does not exist'.
docker cp db/bootstrap-roles.sql acresync-db:/tmp/
docker exec acresync-db psql -U postgres -d acresync -v ON_ERROR_STOP=1 -f /tmp/bootstrap-roles.sql

cd orchestrator
export ACRESYNC_DATABASE_URL="postgres://postgres:postgres@127.0.0.1:55432/acresync"
go build -o bin/migrate ./cmd/migrate && ./bin/migrate -action up   # expect: applied 12

export ACRESYNC_TEST_DATABASE_URL="$ACRESYNC_DATABASE_URL"
go test ./... -count=1
```

Without a database the suite still passes, but 26 tests skip rather than fail, so check the
count: **796 passing, 0 skipped** is a complete run.

Configuration is by environment. Copy `.env.example` to `.env` and fill it in; `.env` is gitignored, and
the loader refuses to start outside `LOCAL` if a development pepper is present, because a pepper in an env
file is a pepper in every backup of that file.

## Licence

No licence granted. All rights reserved.
