# AcreSync API — frontend guide

The contract is [`openapi.yaml`](./openapi.yaml). This file explains how to work against it.

## Read this first

**No backend exists yet.** There is no HTTP server in this repository. The domain logic does exist —
26 Go packages, 796 passing tests — but nothing listens on a port. This contract is published so the
UI can be built against a mock while the handlers are written behind it.

Consequences for you:

- Every shape here is derived from a Go type that already exists and passes tests, so field names and
  types are **not going to change underneath you**. That is the whole reason the contract came first.
- Anything marked `simulated` or `MOCK` in a response is genuinely mocked in the backend too, not just
  in your mock server. Payouts and ASBA fund-blocking are mocked pending corporate banking KYC.

## Running the mock

```bash
npx @stoplight/prism-cli mock docs/api/openapi.yaml --port 4010
```

**Base URL for the mock is `http://127.0.0.1:4010`, with no `/v1`.** Prism serves the paths exactly as
written in the spec and does not apply the `/v1` from the `servers` block. The real backend will serve
`http://127.0.0.1:8080/v1`. Put the whole base in one environment variable and the difference costs you
nothing:

```
VITE_API_BASE=http://127.0.0.1:4010          # mock
VITE_API_BASE=http://127.0.0.1:8080/v1       # real, later
```

Verified working: public endpoints return populated example bodies, and protected endpoints return 401
until you send a bearer token. **Any** token works against the mock:

```
Authorization: Bearer mock-token
```

Generate a typed client if you want one:

```bash
npx openapi-typescript docs/api/openapi.yaml -o src/api/schema.d.ts
```

## Conventions you have to get right

**Money is integer paise.** Every monetary field ends in `Paise`. 100 paise is one rupee, so a ₹10 lakh
unit is `100000000`. Never parse these as decimals. Format for display only:

```ts
const rupees = (paise: number) =>
  new Intl.NumberFormat('en-IN', { style: 'currency', currency: 'INR' })
    .format(paise / 100);
```

A plain JSON number is safe. The largest figure in the system is the scheme's ₹50 crore, 5e10 paise,
far inside the 2^53 exact-integer range of a JS number. Do not do arithmetic that multiplies two paise
values together.

**Timestamps** are RFC 3339 UTC with a `Z`. **Dates** like a record date are `YYYY-MM-DD` and are
calendar dates, not instants — do not run them through a timezone conversion or they will move.

**Digests and addresses** are `0x`-prefixed lowercase hex. Compare them as lowercase strings.

**Statuses are enums, never free text.** Switch on them exhaustively. They mirror the database and the
Solidity enums, and for `AllocationOutcome` the declaration order is significant because the numeric
position is part of a hashed leaf.

## Errors

One envelope everywhere:

```json
{
  "error": {
    "code": "anchor_not_confirmed",
    "message": "the bid book anchor needs 5 confirmations before the draw is bound to it; a reorg afterwards would leave the commitment bound to a book the chain no longer records",
    "requestId": "req_01HQ8Z3M"
  }
}
```

Switch on `code`. **Show `message` to operators verbatim.** These strings come from the domain layer and
usually explain both what is wrong and why the rule exists; replacing them with your own copy makes the
product worse. For investor-facing screens, map `code` to friendlier wording.

`409` is the interesting one: it means the action is legitimate but not permitted yet. Codes are
`precondition_failed`, `not_feasible`, `anchor_not_confirmed`, `ceremony_order`, `units_issued`,
`settlement_incomplete`, `idempotency_conflict`. `423` means the scheme is paused, where everything
except an abort is refused.

## Idempotency

Every `POST` requires an `Idempotency-Key` header. Generate one per user intent — per button press, not
per retry.

```ts
const key = crypto.randomUUID();
// reuse the SAME key for every retry of this action
```

Same key with the same body replays the original response and sets `Idempotency-Replayed: true`. Same
key with a **different** body is `409 idempotency_conflict`, which means you reused a key for a changed
request.

This matters more than usual here. A retry without a key could place a second bank block on an
investor's account or anchor a second transaction on-chain.

## Screens, and the endpoints behind them

### Investor: browse an offer

`GET /offers/{offerId}` gives terms, status and subscription progress. `status` drives the whole screen:
`OPEN` accepts bids, `CLOSED` onward does not, and the eleven states after that are the allotment
process running. `subscription.oversubscriptionNumerator / Denominator` is the demand ratio.

### Investor: place a bid

`POST /offers/{offerId}/bids` with units, price, demat account and bank account.

Rules worth encoding in the form: units between `terms.minBidUnits` and `terms.maxBidUnits`, price
within the band, and one bid per investor per offer. The response includes `block`, the ASBA reservation.

**Explain the money model in the UI.** Funds are blocked in the investor's own bank account, not
collected. They are debited only to the extent units are allotted, and the rest is released. `block.status`
is `BLOCKED` while reserved, `DEBITED` once partly taken, `UNBLOCKED` if the bid won nothing.

A block is all or nothing. Insufficient funds produce `FAILED` with a `failureCode`, never a partial
reservation.

### Investor: bid outcome

`GET /me/bids`. Before the draw, `allotment` is null. After it:

- `FULL` — every unit requested
- `PARTIAL` — some units, the rest refunded
- `NIL_BALLOT` — lost the draw
- `NIL_TECHNICAL` — rejected for a KYC, demat or funds reason

`amountPayablePaise` plus `refundAmountPaise` always equals the amount blocked. If your UI ever shows
otherwise, you have a bug.

A losing bid keeps status `REJECTED_BALLOT` with its `rejectionReason` even after the money is released;
the release shows on `block.status`, not by moving the bid out of a state that carries the reason.

### Investor: portfolio and income

`GET /me/holdings` for units. `GET /me/entitlements` for income per period: `grossPaise`, `tax`,
`netPayablePaise`. `GET /me/payouts` for settlement state.

Two things to handle honestly:

`payout.simulated` is true whenever the provider is `MOCK`. **Surface that.** A demo must never look like
a real payment.

`payout.status` of `SETTLED` is not final — a bank can still reverse it, which is why `REVERSED` is a
separate state. Do not render `SETTLED` as "complete, nothing further".

`entitlement.payable` can be false for a tiny amount below the provider's minimum, which carries forward
rather than being dropped.

### Investor: prove it

This is the differentiator and deserves real design attention rather than a buried link.

`GET /offers/{offerId}/proofs/bids/{bidRef}` returns a Merkle proof that the bid was in the frozen book.
`GET /periods/{periodId}/proofs/entitlements/{walletAddress}` does the same for an entitlement.

Both are **public**, no auth. Anyone holding the reference can check. The response carries `leaf`,
`proof`, `root`, `anchoredTx` and the `preimage` fields so a third party can rebuild the leaf without
guessing. `verified` is our own recomputation and is a convenience, not evidence — the point is that
someone else can check it against the root on-chain. Link `anchoredTx` to Etherscan.

### Public: the transparency surface

`GET /offers/{offerId}/ballot` is the commit-reveal ceremony. Before the reveal `seedPlaintext` and
`finalSeed` are absent, and that absence is the security property: the secret was committed before the
target block existed. After the reveal both appear, because there is nothing left to influence and the
draw must be reproducible.

A good screen here tells the story in order: book frozen and anchored → commitment anchored → target
block chosen by the contract → block mined → seed revealed → draw run → result anchored.

`GET /offers/{offerId}/allotments` is every bid's outcome including the losers, identified only by leaf
index and anchor. Paginated; a book runs to hundreds of rows.

`GET /periods/{periodId}/ndcf` is the distribution statement. `distributionBps` against `floorBps` is the
95% test. Note the floor applies to NDCF, not to gross rent.

### Operator: the lifecycle console

Build this around `GET /admin/offers/{offerId}/readiness`, not around a row of buttons.

```json
{
  "currentStatus": "CLOSED",
  "nextExpected": "BOOK_FROZEN",
  "canAdvance": false,
  "blockers": [
    "3 bid(s) are still awaiting validation; freezing now would fix a book that is still changing"
  ],
  "paused": false
}
```

Render `nextExpected` as the single primary action, disabled when `canAdvance` is false, with `blockers`
shown as the reason. That is strictly better than a button that fails, and the strings are already
written for you.

The ballot steps are `POST .../ballot/commit`, `/reveal`, `/recommit`, `/draw`. `recommit` only becomes
available once the reveal window has lapsed, and only up to `maxAttempts`; after that a trustee must
escalate. Show `attempt` of `maxAttempts` prominently, because burning attempts is public and matters.

### Operator: settlement

`GET /admin/offers/{offerId}/settlement` drives it. `creditedHolders` is the cursor, and the next batch
must declare exactly that as `cursorFrom`. Render it as a progress bar over `expectedHolders`.

`finalisation.failures` lists **every** unmet invariant, not just the first. Show all of them. The
commonest is a missing manager subscription, and the message says so by name.

Note `expectedUnits` is the public side only — 475 of 500 for the reference scheme. The manager's 25
units are credited outside the settlement cursor, so a progress display against 500 will look wrong.

## Things that will trip you up

**Three counts, all different, all correct.** For the reference scheme: `totalUnits` 500 is the
entitlement denominator, `distinctHolders` 475 is the statutory count excluding the manager, and
register lines are 476 including the manager. Do not reconcile them against each other.

**The manager is excluded from the holder count, not from the distribution.** `excludedFromHolderCount`
means it does not count toward the statutory 200. It still receives its share.

**Bids are one per investor per offer.** There is no basket.

**`investorAnchor` is safe to display.** It is an HMAC under a key held in KMS, not a hash of a PAN.
Internal identifiers such as `investorId` and `bidId` are not published on public endpoints and should
not be shown to other users.

## Open questions for us to settle

Worth agreeing before you build the relevant screens, rather than after:

1. **Auth handshake.** Web3Auth is in the config but the token exchange is not designed. Affects login.
2. **Realtime.** A ballot reveal and a settlement batch both change state while an operator watches.
   Polling is assumed for now; websockets or SSE would be nicer and would change your data layer.
3. **File uploads.** Valuation reports and KYC documents have no endpoint yet.
4. **Operator roles.** `operatorAuth` is one scheme today; manager, trustee and compliance probably need
   different permissions, which affects which controls you render.

Raise anything that blocks a screen and it gets decided rather than guessed.
