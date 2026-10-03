# Product

<!-- impeccable:product-schema 1 -->

## Platform

web

## Stack

Vite + React + TypeScript (user chose explicitly; react-router-dom; typed client from docs/api/openapi.yaml via openapi-typescript)

## Users

- Retail investors (India, SEBI SM-REIT): browse offers, place ASBA bids (₹10L/unit reference), track bid outcome, holdings, entitlements, payouts. Often first-time fractional-ownership buyers; money anxiety is the dominant emotion.
- Operators with separation of duties: MANAGER drives the lifecycle (create offer, freeze, ballot commit/reveal/draw, settle), TRUSTEE approves/escalates (second-party sign-off, never "permission denied"), COMPLIANCE reads everything. One token with a `role` claim, not three login systems.
- Public verifiers / diligence: anyone can check bid proofs, ballot ceremony, allotments, NDCF without auth. No account, no PII.

## Product Purpose

AcreSync is a SEBI-compliant SM-REIT platform: traditional escrow/bank rails move rupees; the chain is an on-chain shadow ledger that attests and never custodies. Reference scheme: ₹50 crore across 500 units at ₹10 lakh each (manager 25, public 475, ≥200 distinct unitholders, ≥95% of NDCF distributed). Success = an investor can verify their own allotment and entitlement against the chain without access to anything internal, and an operator can run offer → ballot → settlement without ever touching money movement.

## Positioning

The chain attests, never custodies — there is deliberately no house balance anywhere. Every bid gets a published outcome including losers; the anchored commitment is the document digest (not the CID); verification is fetch-bytes-hash-compare with one hash (SHA-256) everywhere. Neighboring fractional platforms custody funds and publish only winners; AcreSync models ASBA blocking in the investor's own account and makes the draw reproducible by third parties.

## Operating Context

- Web app against `VITE_API_BASE` (Prism mock `http://127.0.0.1:4010` with no `/v1` now; real backend `http://127.0.0.1:8080/v1` later). Same env var for both.
- Auth: mock session for this build — any bearer token works against Prism; `X-Mock-Role: MANAGER|TRUSTEE|COMPLIANCE` switches operator persona. Real Web3Auth (`POST /auth/session` → own token) comes later; identities stay separate (login says who, investor record says what they hold).
- Polling, not sockets: 3s while an operation is in flight; conditional requests (`If-None-Match`/`ETag`, 304 = unchanged). Watch `/readiness` during ballot/settlement.
- Every POST needs an `Idempotency-Key` (16–128 chars, no spaces), one per button-press intent; same key+body replays with `Idempotency-Replayed: true`, same key+different body is `409 idempotency_conflict`.
- Money is integer paise (₹10L = 100000000); format with `en-IN`/`INR` for display only; never multiply two paise values. Timestamps RFC3339 UTC (`Z`); record dates (`YYYY-MM-DD`) are calendar dates, no timezone conversion. Digests/addresses `0x` lowercase, compare as lowercase strings.
- Statuses are exhaustive enums (offer has 13+ states; `AllocationOutcome` order is hashed — never reorder). Three counts differ by design: 500 total units, 475 statutory holders (excl. manager), 476 register lines (incl. manager).
- Uploads: `POST /documents/presign` → `PUT` bytes to `uploadUrl` (no Authorization header, 15-min single-use) → pass `documentId` to the business call; send client-computed SHA-256 `publicHash` when possible.
- Simulated honesty: payouts/ASBA are MOCK until corporate banking KYC; `payout.simulated=true` must be surfaced, `SETTLED` is reversible (never "complete"), `entitlement.payable=false` carries forward. 409s (`anchor_not_confirmed` ~5s waits in LOCAL with devconfirm) are waiting states, not errors; show backend `message` verbatim to operators, friendlier copy to investors.

## Capabilities and Constraints

- Live (mock serves from contract): public scheme/offer/period reads; investor `/me*` behind token; operator readiness + all 12 writes (placeBid, presignDocument, createOffer, advanceOffer, freezeBook, commitSeed, revealSeed, recommitSeed, drawBallot, beginSettlement, submitSettlementBatch, finaliseSettlement); outbox list.
- Not served yet — render as graceful honest placeholders, never fabricate: six public verification reads (offer documents, ballot, allotments, bid proofs, NDCF, entitlement proofs) and `GET /admin/periods/{id}/reconciliation` (schema/contract mismatch; blocks payout honest flag).
- Ceremony ordering enforced server-side (commit → reveal → draw; `recommit` only after reveal-window lapse, `attempt` of `maxAttempts`, then trustee escalation). Settlement replays the draw (both anchored values must reproduce); batches advance a cursor (`cursorFrom` = `creditedHolders` over 475 public units, manager's 25 credited outside the cursor).
- One bid per investor per offer; units within `terms.minBidUnits/maxBidUnits`, price within band. `amountPayablePaise + refundAmountPaise` always equals blocked amount. Losing bids keep `REJECTED_BALLOT` + reason; money release shows on `block.status`.
- `investorAnchor` (HMAC, KMS-peppered) is safe to display; `investorId`/`bidId` are not public and must not leak across users. No PII on-chain, ever.

## Brand Commitments

Name: AcreSync. No logo, palette, type, or other visual assets exist; no binding visual constraints. Do not invent testimonials, customers, or deployment claims.

## Evidence on Hand

- Contract: `docs/api/openapi.yaml` (+ typed client `frontend/src/api/schema.d.ts` generated via openapi-typescript). Field names/types will not change underneath the UI (conformance-tested).
- Guide: `docs/api/README.md` (screens ↔ endpoints, seed stages `open`/`settled`/`paid`, devseed/devtoken/devconfirm walkthrough).
- Domain facts: repo `README.md` (floors: 200 holders, 95% NDCF; immutable non-proxy contracts; Sepolia addresses; M8 frontend not started).
- No real copy deck, no imagery, no user research to preserve. Absences must not be fabricated.

## Product Principles

1. Verify, don't trust: the proof surface is a first-class citizen, not a buried link — every claim links toward its evidence (Etherscan `anchoredTx`, recomputable leaf/preimage).
2. Honest about simulation: anything mocked says so on the screen where the decision is made, in plain words.
3. Operators act on readiness, not buttons: `nextExpected` + `canAdvance` + `blockers` drive the console; failures list every unmet invariant.
4. Money reads calmly: paise-exact, `en-IN` formatted, blocked/debited/released states explained where the money is, never only as a status code.
5. Separation of duties is legible: "awaiting trustee" (a second party), never "you lack permission".
