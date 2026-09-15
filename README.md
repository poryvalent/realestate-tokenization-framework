# realestate-tokenization-framework

A DELTA-style land-tokenization MVP on Ethereum (Foundry + TypeScript backend).

Technical prototype exploring how Maharashtra's proposed land-tokenization framework
could be implemented as permissioned EVM tokens. **Not production code, not legal advice,
and not affiliated with the Government of Maharashtra.**

## Background: Maharashtra DELTA Act (proposed)

In July–September 2026 the Maharashtra government announced work on the
**Maharashtra Digitisation and Exchange of Land Token Asset (DELTA) Act** —
a proposed legal framework for blockchain-based tokenisation of land and other
immovable assets:

- **July 20, 2026:** CM Devendra Fadnavis directed officials to draft DELTA legislation
  to digitise property-linked value while maintaining ownership protections.
- **Sept 9, 2026, Global Fintech Fest (Mumbai):** Fadnavis said the state was putting in
  place the "architecture of the DELTA Act" to become India's first "tokenised state,"
  estimating the unlocked opportunity at ~Rs. 50 lakh crores. Framing: *"tokenization
  is not about speculation, it is about unlocking productive capital"* and
  *"UPI democratized transactions. The next generation of fintech must democratize
  credit, intelligence, ownership, and opportunity."*
- The proposal echoes Nandan Nilekani's March 2025 argument that fragmented land
  holdings cannot easily be sold or used as collateral.
- **Status as of Sept 2026:** proposed framework under committee review (expected to
  include SEBI, BSE, NSE, legal experts, researchers, startups; Urban Development +
  Law & Justice departments drafting). Key open questions flagged by experts:
  how tokens legally map to 7/12 utara / property-card titles, dispute handling,
  custody, and whether land-tokens are treated as securities, property, or both.
- Stated goals: fractional ownership, liquidity for small investors, faster
  collateral/transfer, tamper-proof title history, part of Maharashtra's
  $1-trillion-economy-by-2030 push.

References:

- MediaNama, Sept 9 2026 — "Maharashtra is working on a land tokenisation act"
  (https://www.medianama.com/2026/09/223-maharashtra-land-tokenization-devendra-fadnavis/)
- ANI via LiveMint, Sept 9 2026 — "Maharashtra to create legal framework for
  blockchain-based land tokenisation" 
- CNBC TV18, Sept 10 2026 — "Maharashtra's tokenised-state plan: What could DELTA
  Act mean for landowners, investors"

> This repo tracks the *proposal*. If DELTA is enacted or amended, the compliance
> modules here must be updated to match the final statute, SEBI/RBI circulars,
> and stamp-duty/registration rules.

## What this repo implements

| Component | File | Role |
|---|---|---|
| Identity allowlist | `src/IdentityRegistry.sol` | KYC registry. Only `authorize()`d wallets (country + expiry) pass `isVerified()`. |
| Title mirror | `src/TitleDeedNFT.sol` | 1 NFT per plot. Stores only `recordHash` (keccak256 of off-chain 7/12 + property-card bundle), never PII. |
| Fractional token | `src/FractionalToken.sol` | Permissioned ERC20 backed by one title. Every transfer checks verification, `allowedCountry` (356 = India), `maxHolding`, `paused`. |
| Asset linker | `src/DeltaRegistry.sol` | Maps `propertyId → {titleTokenId, fractional, status}`. `register()` → `markTokenized()` → `freeze()`. |
| Payout splitter | `src/PayoutDistributor.sol` | Pro-rata ETH (rent/sale) distribution via `distribute()` / `claim()`. |
| Off-chain helper | `backend/src/index.ts` | `POST /hash-record` hashes survey/district/docs into `propertyId + recordHash` for minting. Stub for MahaBhulekh / eKYC adapters. |

Design choices for a DELTA context:

- **No PII on-chain.** Title documents stay off-chain (encrypted); only hashes are anchored.
- **Compliance as pluggable rules,** since DELTA is still draft and token classification is unresolved.
- **EVM-native but L2-ready.** Mainnet L1 is used here only for local testing; a real pilot would target an L2 / permissioned Besu chain with mainnet anchoring and government observer nodes.

## Usage

### Contracts (Foundry 1.7.1+)

```shell
forge build
forge test --match-contract DeltaTest -vv
```

Local deploy:

```shell
anvil &
forge script script/Deploy.s.sol --rpc-url http://127.0.0.1:8545 --broadcast
```

### Backend (Node 20+)

```shell
cd backend
npm install --no-audit --no-fund
npm run dev
# health
curl localhost:8787/health
# hash a record bundle
curl -X POST localhost:8787/hash-record -H 'Content-Type: application/json' \
  -d '{"surveyNumber":"123/4","district":"Mumbai Suburban","areaSqM":500,"documents":["7/12 pdf hash"]}'
```

## Limitations / disclaimer

- DELTA is a **proposed** act under review, not enacted law. Token legality, stamp duty,
  registration finality, and SEBI treatment are unresolved.
- Blockchain cannot fix defective titles (oracle problem). This framework fails closed
  when off-chain records and on-chain hashes diverge, but does not validate titles itself.
- No audit, no KYC integration, no custody solution. Do not use with real assets or mainnet funds.
