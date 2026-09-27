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

## Decided

These were open. They are not any more.

### Login

`POST /auth/session` with `{ idToken }`, the Web3Auth JWT. The backend verifies it against Web3Auth's
JWKS, maps the subject to an investor, and returns its own token:

```json
{ "accessToken": "...", "expiresAt": "2026-05-09T12:00:00Z", "investorId": "1e7d4c6a-...", "kycStatus": "VERIFIED" }
```

Send that as `Authorization: Bearer <accessToken>` on investor endpoints. Against the mock, any non-empty
`idToken` returns a fixed test session.

Two identities stay separate on purpose: Web3Auth says who is calling, the investor record says what they
hold. If they were one thing, changing login provider would rewrite the register.

### Polling, not sockets

Poll every 3 seconds while an operation is in flight. Sepolia blocks are ~12 seconds and these are
sequential state-machine transitions, so sockets would add reconnect logic and proxy overhead for no
visible gain.

Use conditional requests so a poll is nearly free:

```ts
const res = await fetch(url, { headers: etag ? { 'If-None-Match': etag } : {} });
if (res.status === 304) return prev;          // nothing changed
etag = res.headers.get('ETag');
return res.json();
```

**A `304` only happens if you send `If-None-Match`.** Without it you get the full body every time — the
polling is still correct, just wasteful. `ETag` is returned on the pollable endpoints; `/readiness` is the
one to watch during the ballot and settlement.

### Uploads

Two steps, and binary never touches the API.

1. `POST /documents/presign` with `{ filename, mimeType, purpose }` → `{ documentId, uploadUrl, expiresAt }`
2. `PUT` the bytes straight to `uploadUrl`, then pass `documentId` into the business call that needs it.

Send `publicHash`, SHA-256 of the bytes, if you can compute it client-side. It is re-checked on receipt and
it is the value that gets anchored, so a mismatch rejects the upload rather than anchoring a digest of
something else.

Locally `uploadUrl` points at a small multipart handler rather than object storage, so the flow is the same
in development.

### Operator roles

One token with a `role` claim of `MANAGER`, `TRUSTEE` or `COMPLIANCE`, enforced in middleware. Not three
security schemes, because there is one credential and pretending otherwise would have you juggling key
sets to represent something the backend does not have.

Against the mock, `X-Mock-Role: TRUSTEE` switches persona.

Worth understanding for the UI: some boundaries are **separation of duties, not privilege**. Trustee
approval of a distribution is a second party signing, and the manager cannot satisfy it by being more
senior. So render those as "awaiting trustee", never as "you lack permission".

Rough split: `MANAGER` drives the lifecycle (create offers, freeze, ballot, settle), `TRUSTEE` approves and
can escalate an abandoned ballot, `COMPLIANCE` reads everything and signs off. Exact per-endpoint mapping
lands with the handlers; if a screen needs it sooner, ask.

## Still to settle

Nothing blocking. Raise anything that stops a screen and it gets decided rather than guessed.
