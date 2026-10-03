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
| M0 | Canonical JSON, money/paise, idempotency, config, clock, SQL migrations | Complete |
| M1 | Merkle, ballot allocation engine, distribution maths | Complete |
| M2 | Solidity: roles, ballot, scheme, encodings | Complete |
| M3 | Deployed and verified on Sepolia, live commit–reveal ceremony | Complete |
| M4 | Transactional outbox, chain client, relayer, Anvil chaos tests | Complete |
| M5 | IPFS pinning, NDCF, snapshots, entitlements, payouts | Complete |
| M6 | Primary market: offer → ASBA → bid book → ballot → settled cap table | Complete |
| M7 | Divergence resolution, period reversal, carry-forward adjustments | Complete |
| — | HTTP API | 27 of 34 contract operations live, including all 12 writes. Six public verification reads and reconciliation remain: see below |
| M8 | Frontend | Complete — dark cinematic rebuild (Tailwind + Framer Motion): all 14 routes against the mock and the real API, honest placeholders for the 6 unbuilt verification reads |
| M9 | Full Sepolia rehearsal | Not started |

**1414 Go tests (subtests included, 0 skipped against Postgres), 131 Solidity tests, `go vet` clean.**
The domain layer is complete: issuance, distribution and correction are all built and tested end to end
against Postgres.

### What the HTTP API does and does not serve

**Live against real Postgres:** the public scheme, offer and period reads; an investor's own records
behind a session token; the chain outbox and offer readiness for operators; and all **12 writes**:

| Investor | Operator |
|---|---|
| `placeBid`, `presignDocument` | `createOffer`, `advanceOffer`, `freezeBook`, `commitSeed`, `revealSeed`, `recommitSeed`, `drawBallot`, `beginSettlement`, `submitSettlementBatch`, `finaliseSettlement` |

Each write does its database change, its chain call enqueue, its audit row and its HTTP idempotency
record in one transaction, so a retry with the same `Idempotency-Key` gets the original response back
and a crash leaves nothing half-done. Each ceremony step is refused until the previous chain call is
confirmed to depth, read from the outbox row that carries it. Settlement replays the draw and requires
it to reproduce both anchored values before building the plan, so there is no stored plan to drift.

**What that does not yet mean:**

- **Queued is not sent.** The writes queue chain calls in `chain_outbox`; the relayer that signs and
  submits them against Sepolia is M9. Until then a queued call confirms only in tests.
- **Funds blocks are the in-memory ASBA sandbox.** A restart of the API forgets them. No bank is involved.
- **The ballot seed pepper is the LOCAL development pepper.** Outside LOCAL it must come from a KMS, which
  is not integrated, so the ceremony and settlement endpoints are not served there.
- **Operator sign-in does not exist.** `cmd/devtoken` mints tokens under the session secret in LOCAL only.

**Not served yet** (listed in a test that fails if this goes stale):

- Six public verification reads: offer documents, ballot, allotments, bid proofs, NDCF, entitlement proofs.
- `GET /admin/periods/{id}/reconciliation`, which cannot be served as specified. `reconciliation_runs` is
  keyed by `scheme_id` and `run_at` with no period reference, so linking a run to a period would be a
  guess — and that guess sets `blocksPayout`, the flag the database uses to refuse paying against a
  register known to be wrong. The fix belongs in the schema or the contract, not in a handler.

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
db/migrations/  15 forward-only SQL migrations
docs/api/       HTTP API contract (OpenAPI) and the frontend guide
docs/           Runbooks
orchestrator/   Go services: 31 internal packages plus cmd tools
tools/          WSL wrappers for Foundry
```

## The API

[`docs/api/openapi.yaml`](docs/api/openapi.yaml) is the HTTP contract, with
[`docs/api/README.md`](docs/api/README.md) as the guide for whoever builds the UI.

`orchestrator/cmd/api` serves it (see "Running it"). For the operations not served yet, or to build UI
without a database, the contract runs as a mock:

```bash
npx @stoplight/prism-cli mock docs/api/openapi.yaml --port 4010
curl http://127.0.0.1:4010/schemes
```

The server's responses are validated against the contract by the conformance tests, in addition to the
check below.

Every schema is derived from a Go type that already passes tests, and
`internal/apicontract` fails the build if the two ever diverge — including a same-set reordering of
`AllocationOutcome`, whose positions are part of a hashed leaf.

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
go build -o bin/migrate ./cmd/migrate && ./bin/migrate -action up   # expect: applied 15

export ACRESYNC_TEST_DATABASE_URL="$ACRESYNC_DATABASE_URL"
go test ./... -count=1 -v | grep -c -- '--- PASS'
```

Without a database the suite still passes, but tests that need one skip rather than fail, so check
the count: **1414 passing, 0 skipped** is a complete run.

The API, LOCAL only (the ballot and settlement need the development pepper):

```bash
export ACRESYNC_ENVIRONMENT=LOCAL
export ACRESYNC_API_SESSION_SECRET=$(openssl rand -base64 32)
export ACRESYNC_ANCHOR_PEPPER_DEV=$(openssl rand -base64 32)
go run ./cmd/api                          # http://127.0.0.1:8080/v1
go run ./cmd/devtoken -role MANAGER       # an operator token, LOCAL only
go run ./cmd/devseed                      # demo scheme, investors, an open offer with 240 bids
go run ./cmd/devseed -stage paid          # ...settled, plus a closed quarter with MOCK payouts
go run ./cmd/devconfirm -watch            # SIMULATED confirmations for LOCAL schemes only
```

`ACRESYNC_CHAIN_SIMULATED=true` replaces the RPC with a predictable simulated chain, for demos without an
RPC key. `docs/api/README.md` has the full walkthrough.

With `ACRESYNC_CHAIN_RPC_URL` set, the API dials it read-only (no relayer key) for the block head and
block hashes the reveal needs. Mock IPFS pins and uploads are written under `var/`.

Configuration is by environment. Copy `.env.example` to `.env` and fill it in; `.env` is gitignored, and
the loader refuses to start outside `LOCAL` if a development pepper is present, because a pepper in an env
file is a pepper in every backup of that file.

## Licence

No licence granted. All rights reserved.
