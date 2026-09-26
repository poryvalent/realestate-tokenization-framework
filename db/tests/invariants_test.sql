-- AcreSync M0 :: schema invariant tests
--
-- Applying a migration proves only that the SQL parses. These tests prove the
-- constraints reject what they exist to reject. Every case here corresponds to a
-- specific way the system could lose money or breach a regulatory floor.
--
-- Run against a freshly migrated database:
--   psql -U postgres -d acresync -v ON_ERROR_STOP=1 -f /db/tests/invariants_test.sql

\set ON_ERROR_STOP on
\timing off
SET client_min_messages TO NOTICE;

-- ---------------------------------------------------------------------------
-- Assertion helpers
-- ---------------------------------------------------------------------------

-- expect_reject prints the SQLSTATE and message of the rejection it observed.
--
-- That detail is not decoration. A test that only asserts "something failed"
-- passes when the statement fails for an unintended reason, such as a typo in a
-- column name, and then silently stops testing the invariant it was written for.
-- Printing the reason makes each rejection reviewable.
CREATE OR REPLACE FUNCTION expect_reject(stmt TEXT, label TEXT)
RETURNS VOID LANGUAGE plpgsql AS $fn$
DECLARE
    v_state TEXT;
    v_msg   TEXT;
BEGIN
    BEGIN
        EXECUTE stmt;
    EXCEPTION WHEN OTHERS THEN
        v_state := SQLSTATE;
        v_msg   := SQLERRM;
        RAISE NOTICE 'PASS  reject  %  [%] %', label, v_state, left(v_msg, 110);
        RETURN;
    END;
    RAISE EXCEPTION 'FAIL  reject  % :: statement was ACCEPTED but must be rejected', label;
END;
$fn$;

CREATE OR REPLACE FUNCTION expect_accept(stmt TEXT, label TEXT)
RETURNS VOID LANGUAGE plpgsql AS $fn$
BEGIN
    EXECUTE stmt;
    RAISE NOTICE 'PASS  accept  %', label;
EXCEPTION WHEN OTHERS THEN
    RAISE EXCEPTION 'FAIL  accept  % :: %', label, SQLERRM;
END;
$fn$;

-- ---------------------------------------------------------------------------
-- Fixtures :: the v1 scheme
-- ---------------------------------------------------------------------------

\set scheme_id      '''aaaaaaaa-0000-0000-0000-000000000001'''
\set spv_id         '''aaaaaaaa-0000-0000-0000-000000000002'''
\set prop_id        '''aaaaaaaa-0000-0000-0000-000000000003'''
\set lease_id       '''aaaaaaaa-0000-0000-0000-000000000004'''
\set offer_id       '''aaaaaaaa-0000-0000-0000-000000000005'''
\set im_inv         '''bbbbbbbb-0000-0000-0000-000000000001'''
\set inv_a          '''bbbbbbbb-0000-0000-0000-000000000002'''
\set inv_b          '''bbbbbbbb-0000-0000-0000-000000000003'''
\set period_id      '''cccccccc-0000-0000-0000-000000000001'''
\set snap_id        '''cccccccc-0000-0000-0000-000000000002'''
\set anchor_ok      '''dddddddd-0000-0000-0000-000000000001'''
\set anchor_pending '''dddddddd-0000-0000-0000-000000000002'''

-- 500 units at ₹10 lakh = ₹50 crore, the band floor. 25 IM + 475 public.
INSERT INTO schemes (id, sebi_scheme_ref, name, is_leveraged, asset_value_paise,
                     unit_price_paise, total_units, im_units, public_units,
                     min_public_holders, environment_tag)
VALUES (:scheme_id, 'SEBI/SM-REIT/2026/001', 'AcreSync Scheme I', FALSE,
        50000000000, 100000000, 500, 25, 475, 200, 'LOCAL');

INSERT INTO spvs (id, scheme_id, cin, name, ownership_pct, incorporation_date)
VALUES (:spv_id, :scheme_id, 'U70100KA2026PTC000001', 'AcreSync Asset I Pvt Ltd', 100.00, '2026-01-15');

INSERT INTO properties (id, spv_id, name, address_text, city, grade,
                        carpet_area_sqft, leasable_area_sqft, occupancy_bps,
                        acquisition_value_paise)
VALUES (:prop_id, :spv_id, 'Prestige Tech Park B', 'Outer Ring Road', 'Bengaluru', 'A',
        85000, 100000, 10000, 50000000000);

INSERT INTO leases (id, property_id, tenant_name, monthly_rent_paise, start_date, end_date, status)
VALUES (:lease_id, :prop_id, 'Anchor Tenant Pvt Ltd', 2750000000, '2026-01-01', '2031-12-31', 'ACTIVE');

INSERT INTO simulated_clock (scheme_id, current_value)
VALUES (:scheme_id, '2026-09-01T00:00:00Z');

INSERT INTO investors (id, full_name_enc, pan_enc, email_enc, phone_enc, investor_class, is_im_related)
VALUES (:im_inv, '\x01', '\x01', '\x01', '\x01', 'BODY_CORPORATE', TRUE),
       (:inv_a,  '\x02', '\x02', '\x02', '\x02', 'RESIDENT_IND',   FALSE),
       (:inv_b,  '\x03', '\x03', '\x03', '\x03', 'NRI',            FALSE);

INSERT INTO investor_anchors (investor_id, scheme_id, anchor_hash, pepper_key_id)
VALUES (:im_inv, :scheme_id, decode(repeat('11', 32), 'hex'), 'kms/acresync/pepper/v1'),
       (:inv_a,  :scheme_id, decode(repeat('22', 32), 'hex'), 'kms/acresync/pepper/v1'),
       (:inv_b,  :scheme_id, decode(repeat('33', 32), 'hex'), 'kms/acresync/pepper/v1');

INSERT INTO wallets (investor_id, address) VALUES
    (:im_inv, '0x00000000000000000000000000000000000000a1'),
    (:inv_a,  '0x00000000000000000000000000000000000000a2'),
    (:inv_b,  '0x00000000000000000000000000000000000000a3');

INSERT INTO demat_accounts (investor_id, depository, dp_id, client_id, verified_at) VALUES
    (:im_inv, 'CDSL', '12010600', '00000001', now()),
    (:inv_a,  'CDSL', '12010600', '00000002', now()),
    (:inv_b,  'NSDL', 'IN300100', '00000003', now());

INSERT INTO bank_accounts (investor_id, account_number_enc, ifsc, account_name_enc) VALUES
    (:im_inv, '\x0a', 'HDFC0001234', '\x0a'),
    (:inv_a,  '\x0b', 'ICIC0002345', '\x0b'),
    (:inv_b,  '\x0c', 'SBIN0003456', '\x0c');

-- ---------------------------------------------------------------------------
-- Domain 1 :: scheme parameters
-- ---------------------------------------------------------------------------

\echo '=== Domain 1 :: scheme and asset =========================================='

-- A cap table that does not add up makes the settlement invariant uncheckable.
SELECT expect_reject($$
    INSERT INTO schemes (sebi_scheme_ref, name, asset_value_paise, unit_price_paise,
                         total_units, im_units, public_units, environment_tag)
    VALUES ('X/1', 'bad', 50000000000, 100000000, 500, 25, 400, 'LOCAL')
$$, 'total_units must equal im_units + public_units');

-- Below the statutory ₹10 lakh minimum unit price.
SELECT expect_reject($$
    INSERT INTO schemes (sebi_scheme_ref, name, asset_value_paise, unit_price_paise,
                         total_units, im_units, public_units, environment_tag)
    VALUES ('X/2', 'bad', 50000000000, 99999999, 500, 25, 475, 'LOCAL')
$$, 'unit price below ₹10 lakh');

-- Outside the ₹50 crore to ₹500 crore scheme band.
SELECT expect_reject($$
    INSERT INTO schemes (sebi_scheme_ref, name, asset_value_paise, unit_price_paise,
                         total_units, im_units, public_units, environment_tag)
    VALUES ('X/3', 'bad', 49999999999, 100000000, 500, 25, 475, 'LOCAL')
$$, 'asset value below ₹50 crore');

SELECT expect_reject($$
    INSERT INTO schemes (sebi_scheme_ref, name, asset_value_paise, unit_price_paise,
                         total_units, im_units, public_units, environment_tag)
    VALUES ('X/4', 'bad', 500000000001, 100000000, 5000, 250, 4750, 'LOCAL')
$$, 'asset value above ₹500 crore');

-- units * price must reconcile to the declared asset value.
SELECT expect_reject($$
    INSERT INTO schemes (sebi_scheme_ref, name, asset_value_paise, unit_price_paise,
                         total_units, im_units, public_units, environment_tag)
    VALUES ('X/5', 'bad', 50000000000, 100000000, 400, 25, 375, 'LOCAL')
$$, 'units * unit price must equal asset value');

-- An SM-REIT scheme must hold property through a wholly owned SPV.
SELECT expect_reject($$
    INSERT INTO spvs (scheme_id, cin, name, ownership_pct, incorporation_date)
    VALUES ('aaaaaaaa-0000-0000-0000-000000000001', 'U70100KA2026PTC000099', 'partial', 99.00, '2026-01-15')
$$, 'SPV ownership below 100 percent');

-- The structural guarantee: a property has no scheme_id column at all, so it
-- cannot bypass the SPV. Verified by asking the catalogue rather than by trying
-- an insert, because the column's absence is the point.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM information_schema.columns
                WHERE table_name = 'properties' AND column_name = 'scheme_id') THEN
        RAISE EXCEPTION 'FAIL  properties.scheme_id exists; a property could bypass its SPV';
    END IF;
    RAISE NOTICE 'PASS  structure  properties cannot reference a scheme directly';
END $$;

-- Mixed-case addresses are rejected so one address has one spelling.
SELECT expect_reject($$
    INSERT INTO wallets (investor_id, address)
    VALUES ('bbbbbbbb-0000-0000-0000-000000000002', '0x00000000000000000000000000000000000000A9')
$$, 'EIP-55 mixed-case wallet address');

-- One active wallet per investor, so a rotation cannot leave two live.
SELECT expect_reject($$
    INSERT INTO wallets (investor_id, address)
    VALUES ('bbbbbbbb-0000-0000-0000-000000000002', '0x00000000000000000000000000000000000000b9')
$$, 'second active wallet for one investor');

-- ---------------------------------------------------------------------------
-- Domain 3 :: offer and bid bounds
-- ---------------------------------------------------------------------------

\echo '=== Domain 3 :: primary market ============================================'

INSERT INTO offers (id, scheme_id, offer_type, price_band_lower_paise, price_band_upper_paise,
                    units_on_offer, min_bid_units, max_bid_units, min_subscription_units,
                    min_distinct_holders, opens_at, closes_at, allotment_due_at, idempotency_key)
VALUES (:offer_id, :scheme_id, 'INITIAL', 100000000, 105000000,
        475, 1, 25, 428, 200,
        '2026-08-01T00:00:00Z', '2026-08-31T00:00:00Z', '2026-09-03T00:00:00Z',
        decode(repeat('a1', 32), 'hex'));

-- The whale cap must leave the 200-holder floor arithmetically reachable:
-- max_bid_units <= units_on_offer - (min_distinct_holders - 1) = 475 - 199 = 276.
SELECT expect_reject($$
    INSERT INTO offers (scheme_id, price_band_lower_paise, price_band_upper_paise,
                        units_on_offer, min_bid_units, max_bid_units, min_subscription_units,
                        min_distinct_holders, opens_at, closes_at, allotment_due_at, idempotency_key)
    VALUES ('aaaaaaaa-0000-0000-0000-000000000001', 100000000, 105000000,
            475, 1, 300, 428, 200,
            '2026-08-01T00:00:00Z', '2026-08-31T00:00:00Z', '2026-09-03T00:00:00Z',
            decode(repeat('a2', 32), 'hex'))
$$, 'max bid large enough to make the 200-holder floor unreachable');

-- A bid within the cap.
SELECT expect_accept($$
    INSERT INTO bids (offer_id, investor_id, investor_anchor_hash, demat_account_id,
                      bank_account_id, units_bid, price_per_unit_paise, total_amount_paise,
                      bid_reference, idempotency_key)
    SELECT 'aaaaaaaa-0000-0000-0000-000000000005', 'bbbbbbbb-0000-0000-0000-000000000002',
           decode(repeat('22', 32), 'hex'), d.id, b.id, 3, 100000000, 300000000,
           '0123456789abcdef0123456789abcdef', decode(repeat('b1', 32), 'hex')
      FROM demat_accounts d, bank_accounts b
     WHERE d.investor_id = 'bbbbbbbb-0000-0000-0000-000000000002'
       AND b.investor_id = 'bbbbbbbb-0000-0000-0000-000000000002'
$$, 'bid of 3 units inside the cap');

-- Above the whale cap.
SELECT expect_reject($$
    INSERT INTO bids (offer_id, investor_id, investor_anchor_hash, demat_account_id,
                      bank_account_id, units_bid, price_per_unit_paise, total_amount_paise,
                      bid_reference, idempotency_key)
    SELECT 'aaaaaaaa-0000-0000-0000-000000000005', 'bbbbbbbb-0000-0000-0000-000000000003',
           decode(repeat('33', 32), 'hex'), d.id, b.id, 26, 100000000, 2600000000,
           '0123456789abcdef0123456789abcde0', decode(repeat('b2', 32), 'hex')
      FROM demat_accounts d, bank_accounts b
     WHERE d.investor_id = 'bbbbbbbb-0000-0000-0000-000000000003'
       AND b.investor_id = 'bbbbbbbb-0000-0000-0000-000000000003'
$$, 'bid of 26 units exceeds the 25-unit cap');

-- Outside the price band.
SELECT expect_reject($$
    INSERT INTO bids (offer_id, investor_id, investor_anchor_hash, demat_account_id,
                      bank_account_id, units_bid, price_per_unit_paise, total_amount_paise,
                      bid_reference, idempotency_key)
    SELECT 'aaaaaaaa-0000-0000-0000-000000000005', 'bbbbbbbb-0000-0000-0000-000000000003',
           decode(repeat('33', 32), 'hex'), d.id, b.id, 1, 106000000, 106000000,
           '0123456789abcdef0123456789abcde1', decode(repeat('b3', 32), 'hex')
      FROM demat_accounts d, bank_accounts b
     WHERE d.investor_id = 'bbbbbbbb-0000-0000-0000-000000000003'
       AND b.investor_id = 'bbbbbbbb-0000-0000-0000-000000000003'
$$, 'bid price above the band ceiling');

-- A second bid would let one investor circumvent the cap and double-count toward
-- the holder floor.
SELECT expect_reject($$
    INSERT INTO bids (offer_id, investor_id, investor_anchor_hash, demat_account_id,
                      bank_account_id, units_bid, price_per_unit_paise, total_amount_paise,
                      bid_reference, idempotency_key)
    SELECT 'aaaaaaaa-0000-0000-0000-000000000005', 'bbbbbbbb-0000-0000-0000-000000000002',
           decode(repeat('22', 32), 'hex'), d.id, b.id, 1, 100000000, 100000000,
           '0123456789abcdef0123456789abcde2', decode(repeat('b4', 32), 'hex')
      FROM demat_accounts d, bank_accounts b
     WHERE d.investor_id = 'bbbbbbbb-0000-0000-0000-000000000002'
       AND b.investor_id = 'bbbbbbbb-0000-0000-0000-000000000002'
$$, 'second bid from the same investor in one offer');

-- An inconsistent derived total.
SELECT expect_reject($$
    INSERT INTO bids (offer_id, investor_id, investor_anchor_hash, demat_account_id,
                      bank_account_id, units_bid, price_per_unit_paise, total_amount_paise,
                      bid_reference, idempotency_key)
    SELECT 'aaaaaaaa-0000-0000-0000-000000000005', 'bbbbbbbb-0000-0000-0000-000000000003',
           decode(repeat('33', 32), 'hex'), d.id, b.id, 2, 100000000, 999,
           '0123456789abcdef0123456789abcde3', decode(repeat('b5', 32), 'hex')
      FROM demat_accounts d, bank_accounts b
     WHERE d.investor_id = 'bbbbbbbb-0000-0000-0000-000000000003'
       AND b.investor_id = 'bbbbbbbb-0000-0000-0000-000000000003'
$$, 'total_amount_paise inconsistent with units * price');

-- Idempotency: the same key twice is the same action twice.
SELECT expect_reject($$
    INSERT INTO offers (scheme_id, price_band_lower_paise, price_band_upper_paise,
                        units_on_offer, min_bid_units, max_bid_units, min_subscription_units,
                        min_distinct_holders, opens_at, closes_at, allotment_due_at, idempotency_key)
    VALUES ('aaaaaaaa-0000-0000-0000-000000000001', 100000000, 105000000,
            475, 1, 25, 428, 200,
            '2026-08-01T00:00:00Z', '2026-08-31T00:00:00Z', '2026-09-03T00:00:00Z',
            decode(repeat('a1', 32), 'hex'))
$$, 'duplicate idempotency key on offers');

-- ---------------------------------------------------------------------------
-- Ballot :: commit-reveal ordering
-- ---------------------------------------------------------------------------

\echo '=== Ballot :: commit-reveal ordering ======================================'

INSERT INTO ballot_runs (id, offer_id, bidbook_snapshot_at, bidbook_merkle_root,
                         bidbook_cid_digest, bid_leaf_count, total_units_bid,
                         distinct_bidders, oversubscription_num, oversubscription_den,
                         idempotency_key)
VALUES ('eeeeeeee-0000-0000-0000-000000000001', :offer_id, '2026-08-31T00:00:00Z',
        decode(repeat('c1', 32), 'hex'), decode(repeat('c2', 32), 'hex'),
        900, 2000, 900, 2000, 475, decode(repeat('c3', 32), 'hex'));

-- The seed plaintext must not exist before the commitment is anchored on-chain.
-- If it can, the commitment proves nothing about what was intended.
SELECT expect_reject($$
    UPDATE ballot_runs
       SET seed_plaintext = decode(repeat('ff', 32), 'hex')
     WHERE id = 'eeeeeeee-0000-0000-0000-000000000001'
$$, 'seed plaintext written with no commitment');

SELECT expect_accept($$
    UPDATE ballot_runs
       SET seed_commitment = decode(repeat('d1', 32), 'hex'),
           target_block = 8123456, attempt = 1, status = 'SEED_COMMITTED'
     WHERE id = 'eeeeeeee-0000-0000-0000-000000000001'
$$, 'commit seed after the bid book is anchored');

SELECT expect_reject($$
    UPDATE ballot_runs
       SET seed_plaintext = decode(repeat('ff', 32), 'hex')
     WHERE id = 'eeeeeeee-0000-0000-0000-000000000001'
$$, 'seed plaintext written before the commitment is confirmed on-chain');

SELECT expect_accept($$
    UPDATE ballot_runs
       SET commitment_anchored_tx = '0x' || repeat('1', 64)
     WHERE id = 'eeeeeeee-0000-0000-0000-000000000001'
$$, 'record the confirmed commitment transaction');

SELECT expect_accept($$
    UPDATE ballot_runs
       SET seed_plaintext = decode(repeat('ff', 32), 'hex'),
           target_block_hash = decode(repeat('ee', 32), 'hex'),
           final_seed = decode(repeat('dd', 32), 'hex'),
           status = 'SEED_REVEALED'
     WHERE id = 'eeeeeeee-0000-0000-0000-000000000001'
$$, 'reveal seed once the commitment is confirmed');

-- The abandonment-grinding guard: a recommit may change only the target block.
SELECT expect_reject($$
    UPDATE ballot_runs
       SET seed_commitment = decode(repeat('d9', 32), 'hex')
     WHERE id = 'eeeeeeee-0000-0000-0000-000000000001'
$$, 'commitment mutated on recommit (would allow outcome grinding)');

SELECT expect_reject($$
    UPDATE ballot_runs SET attempt = 4
     WHERE id = 'eeeeeeee-0000-0000-0000-000000000001'
$$, 'fourth seed attempt beyond the three-attempt cap');

-- ---------------------------------------------------------------------------
-- Domain 4 :: register
-- ---------------------------------------------------------------------------

\echo '=== Domain 4 :: register =================================================='

INSERT INTO unit_holdings (scheme_id, investor_id, wallet_address, units, is_excluded_from_holder_count)
VALUES (:scheme_id, :im_inv, '0x00000000000000000000000000000000000000a1', 25, TRUE),
       (:scheme_id, :inv_a,  '0x00000000000000000000000000000000000000a2', 3,  FALSE),
       (:scheme_id, :inv_b,  '0x00000000000000000000000000000000000000a3', 472, FALSE);

SELECT expect_accept($$
    INSERT INTO holding_ledger (scheme_id, investor_id, entry_type, units_delta, balance_after,
                                source, occurred_at, simulated_clock_value, idempotency_key)
    VALUES ('aaaaaaaa-0000-0000-0000-000000000001', 'bbbbbbbb-0000-0000-0000-000000000002',
            'ALLOTMENT', 3, 3, 'BALLOT', now(), '2026-09-03T00:00:00Z',
            decode(repeat('f1', 32), 'hex'))
$$, 'first ledger entry with balance_after = delta');

-- Invariant 4: the running balance must be continuous.
SELECT expect_reject($$
    INSERT INTO holding_ledger (scheme_id, investor_id, entry_type, units_delta, balance_after,
                                source, occurred_at, simulated_clock_value, depository_ref, idempotency_key)
    VALUES ('aaaaaaaa-0000-0000-0000-000000000001', 'bbbbbbbb-0000-0000-0000-000000000002',
            'TRANSFER_IN', 2, 99, 'DEPOSITORY_SYNC', now(), '2026-09-04T00:00:00Z', 'DEP/1',
            decode(repeat('f2', 32), 'hex'))
$$, 'ledger discontinuity: balance_after does not follow from the delta');

SELECT expect_accept($$
    INSERT INTO holding_ledger (scheme_id, investor_id, entry_type, units_delta, balance_after,
                                source, occurred_at, simulated_clock_value, depository_ref, idempotency_key)
    VALUES ('aaaaaaaa-0000-0000-0000-000000000001', 'bbbbbbbb-0000-0000-0000-000000000002',
            'TRANSFER_IN', 2, 5, 'DEPOSITORY_SYNC', now(), '2026-09-04T00:00:00Z', 'DEP/1',
            decode(repeat('f3', 32), 'hex'))
$$, 'continuous ledger entry');

-- A depository-sourced movement without the depository's own reference cannot be
-- reconciled back to the legal register.
SELECT expect_reject($$
    INSERT INTO holding_ledger (scheme_id, investor_id, entry_type, units_delta, balance_after,
                                source, occurred_at, simulated_clock_value, idempotency_key)
    VALUES ('aaaaaaaa-0000-0000-0000-000000000001', 'bbbbbbbb-0000-0000-0000-000000000002',
            'TRANSFER_OUT', -1, 4, 'DEPOSITORY_SYNC', now(), '2026-09-05T00:00:00Z',
            decode(repeat('f4', 32), 'hex'))
$$, 'depository transfer with no depository_ref');

-- Direction must agree with entry type, or an outflow could inflate a balance.
SELECT expect_reject($$
    INSERT INTO holding_ledger (scheme_id, investor_id, entry_type, units_delta, balance_after,
                                source, occurred_at, simulated_clock_value, depository_ref, idempotency_key)
    VALUES ('aaaaaaaa-0000-0000-0000-000000000001', 'bbbbbbbb-0000-0000-0000-000000000002',
            'TRANSFER_OUT', 1, 6, 'DEPOSITORY_SYNC', now(), '2026-09-05T00:00:00Z', 'DEP/2',
            decode(repeat('f5', 32), 'hex'))
$$, 'TRANSFER_OUT with a positive delta');

-- Append-only, with one narrow exception for anchor bookkeeping.
SELECT expect_reject($$
    UPDATE holding_ledger SET units_delta = 100
     WHERE idempotency_key = decode(repeat('f1', 32), 'hex')
$$, 'rewriting a ledger delta');

SELECT expect_reject($$
    DELETE FROM holding_ledger WHERE idempotency_key = decode(repeat('f1', 32), 'hex')
$$, 'deleting a ledger entry');

SELECT expect_accept($$
    UPDATE holding_ledger
       SET anchor_status = 'CONFIRMED', anchored_tx = '0x' || repeat('2', 64)
     WHERE idempotency_key = decode(repeat('f1', 32), 'hex')
$$, 'anchor-only update to a ledger entry');

-- A correction must say why and carry a narrative digest.
SELECT expect_reject($$
    INSERT INTO holding_ledger (scheme_id, investor_id, entry_type, units_delta, balance_after,
                                source, occurred_at, simulated_clock_value, idempotency_key)
    VALUES ('aaaaaaaa-0000-0000-0000-000000000001', 'bbbbbbbb-0000-0000-0000-000000000002',
            'CORRECTION', -1, 4, 'ADMIN_CORRECTION', now(), '2026-09-06T00:00:00Z',
            decode(repeat('f6', 32), 'hex'))
$$, 'correction with no reason code or narrative hash');

-- ---------------------------------------------------------------------------
-- The simulated clock
-- ---------------------------------------------------------------------------

\echo '=== Simulated clock ======================================================='

SELECT expect_accept($$
    UPDATE simulated_clock SET current_value = '2026-09-30T00:00:00Z'
     WHERE scheme_id = 'aaaaaaaa-0000-0000-0000-000000000001'
$$, 'clock advances forward');

SELECT expect_reject($$
    UPDATE simulated_clock SET current_value = '2026-09-01T00:00:00Z'
     WHERE scheme_id = 'aaaaaaaa-0000-0000-0000-000000000001'
$$, 'clock rewind');

SELECT expect_accept($$
    UPDATE simulated_clock SET is_frozen = TRUE
     WHERE scheme_id = 'aaaaaaaa-0000-0000-0000-000000000001'
$$, 'freeze the clock');

SELECT expect_reject($$
    UPDATE simulated_clock SET current_value = '2026-10-01T00:00:00Z'
     WHERE scheme_id = 'aaaaaaaa-0000-0000-0000-000000000001'
$$, 'advancing a frozen clock');

UPDATE simulated_clock SET is_frozen = FALSE WHERE scheme_id = :scheme_id;

-- ---------------------------------------------------------------------------
-- Domain 5 :: the 95 percent floor
-- ---------------------------------------------------------------------------

\echo '=== Domain 5 :: distribution floor ========================================'

INSERT INTO distribution_periods (id, scheme_id, period_seq, period_label,
                                  period_start, period_end, idempotency_key)
VALUES (:period_id, :scheme_id, 1, '2026-09', '2026-09-01', '2026-09-30',
        decode(repeat('e1', 32), 'hex'));

INSERT INTO ndcf_line_items (distribution_period_id, line_type, direction, amount_paise,
                             evidence_sha256, idempotency_key)
VALUES (:period_id, 'GROSS_RENT',   'INFLOW',  33000000000, decode(repeat('a5', 32), 'hex'), decode(repeat('e2', 32), 'hex')),
       (:period_id, 'PROPERTY_TAX', 'OUTFLOW',  2000000000, decode(repeat('a6', 32), 'hex'), decode(repeat('e3', 32), 'hex')),
       (:period_id, 'IM_FEE',       'OUTFLOW',  1000000000, decode(repeat('a7', 32), 'hex'), decode(repeat('e4', 32), 'hex'));

-- Direction is determined by line type. An expense booked as an inflow would
-- silently inflate NDCF and therefore the distribution.
SELECT expect_reject($$
    INSERT INTO ndcf_line_items (distribution_period_id, line_type, direction, amount_paise,
                                 evidence_sha256, idempotency_key)
    VALUES ('cccccccc-0000-0000-0000-000000000001', 'PROPERTY_TAX', 'INFLOW', 1,
            decode(repeat('a8', 32), 'hex'), decode(repeat('e5', 32), 'hex'))
$$, 'PROPERTY_TAX booked as an INFLOW');

-- NDCF = 33,000,000,000 - 2,000,000,000 - 1,000,000,000 = 30,000,000,000 paise.
-- 95 percent of that is 28,500,000,000.
SELECT expect_reject($$
    UPDATE distribution_periods
       SET ndcf_paise = 30000000000, distributed_paise = 28499999999, distribution_bps = 9499
     WHERE id = 'cccccccc-0000-0000-0000-000000000001'
$$, 'distribution one paisa below the 95 percent floor');

SELECT expect_accept($$
    UPDATE distribution_periods
       SET ndcf_paise = 30000000000, distributed_paise = 28500000000, distribution_bps = 9500,
           ndcf_statement_sha256 = decode(repeat('b8', 32), 'hex'),
           record_date = '2026-09-30', status = 'NDCF_DRAFTED'
     WHERE id = 'cccccccc-0000-0000-0000-000000000001'
$$, 'distribution exactly at the 95 percent floor');

SELECT expect_reject($$
    UPDATE distribution_periods
       SET distributed_paise = 30000000001
     WHERE id = 'cccccccc-0000-0000-0000-000000000001'
$$, 'distributing more than NDCF');

-- Four eyes: anchoring requires both approvals, from two different people.
SELECT expect_reject($$
    UPDATE distribution_periods SET status = 'ANCHORED'
     WHERE id = 'cccccccc-0000-0000-0000-000000000001'
$$, 'anchoring without dual approval');

SELECT expect_reject($$
    UPDATE distribution_periods
       SET im_approved_by = 'ops@acresync', im_approved_at = now(),
           trustee_approved_by = 'ops@acresync', trustee_approved_at = now()
     WHERE id = 'cccccccc-0000-0000-0000-000000000001'
$$, 'one operator approving both halves of four-eyes');

SELECT expect_accept($$
    UPDATE distribution_periods
       SET im_approved_by = 'im@acresync', im_approved_at = now(),
           trustee_approved_by = 'trustee@axis', trustee_approved_at = now(),
           status = 'NDCF_APPROVED'
     WHERE id = 'cccccccc-0000-0000-0000-000000000001'
$$, 'dual approval by two distinct actors');

-- ---------------------------------------------------------------------------
-- Snapshot, entitlements, and exactness
-- ---------------------------------------------------------------------------

\echo '=== Record date, entitlements, exactness =================================='

INSERT INTO register_snapshots (id, scheme_id, distribution_period_id, record_date,
                                simulated_clock_value, total_units, distinct_holders,
                                snapshot_merkle_root, snapshot_cid_digest, idempotency_key)
VALUES (:snap_id, :scheme_id, :period_id, '2026-09-30', '2026-09-30T18:30:00Z',
        500, 2, decode(repeat('9a', 32), 'hex'), decode(repeat('9b', 32), 'hex'),
        decode(repeat('9c', 32), 'hex'));

INSERT INTO register_snapshot_lines (id, snapshot_id, leaf_index, investor_id,
                                     investor_anchor_hash, wallet_address, units, is_excluded)
VALUES ('cccccccc-0000-0000-0000-00000000000a', :snap_id, 0, :im_inv, decode(repeat('11', 32), 'hex'), '0x00000000000000000000000000000000000000a1', 25, TRUE),
       ('cccccccc-0000-0000-0000-00000000000b', :snap_id, 1, :inv_a,  decode(repeat('22', 32), 'hex'), '0x00000000000000000000000000000000000000a2', 3,  FALSE),
       ('cccccccc-0000-0000-0000-00000000000c', :snap_id, 2, :inv_b,  decode(repeat('33', 32), 'hex'), '0x00000000000000000000000000000000000000a3', 472, FALSE);

-- Snapshot lines are the record-date freeze that a published proof verifies
-- against, so they are immutable too.
SELECT expect_reject($$
    UPDATE register_snapshot_lines SET units = 99
     WHERE id = 'cccccccc-0000-0000-0000-00000000000b'
$$, 'mutating a record-date snapshot line');

-- 28,500,000,000 paise over 500 units divides exactly: 57,000,000 per unit.
--   IM   25 units -> 1,425,000,000
--   A     3 units ->   171,000,000
--   B   472 units -> 26,904,000,000
--   total          28,500,000,000
INSERT INTO entitlements (distribution_period_id, investor_id, snapshot_line_id, units,
                          snapshot_total_units, gross_entitlement_paise, remainder_numerator)
VALUES (:period_id, :im_inv, 'cccccccc-0000-0000-0000-00000000000a', 25,  500,  1425000000, 0),
       (:period_id, :inv_a,  'cccccccc-0000-0000-0000-00000000000b', 3,   500,   171000000, 0),
       (:period_id, :inv_b,  'cccccccc-0000-0000-0000-00000000000c', 472, 500, 26904000000, 0);

SELECT expect_accept($$
    SELECT assert_period_entitlements_exact('cccccccc-0000-0000-0000-000000000001')
$$, 'entitlements sum exactly to the distributed amount');

-- Invariant 7 has no tolerance. One paisa short must fail.
--
-- Written as three plain statements rather than a DO block nested inside the
-- expect_reject argument: psql tracks dollar-quote tags, but nesting a $inner$
-- block inside a $$ argument is fragile enough to have already cost one hung
-- run, and clarity is worth more here than compactness.
UPDATE entitlements SET gross_entitlement_paise = gross_entitlement_paise - 1
 WHERE snapshot_line_id = 'cccccccc-0000-0000-0000-00000000000b';

SELECT expect_reject($$
    SELECT assert_period_entitlements_exact('cccccccc-0000-0000-0000-000000000001')
$$, 'entitlement sum one paisa short of the distributed amount');

UPDATE entitlements SET gross_entitlement_paise = gross_entitlement_paise + 1
 WHERE snapshot_line_id = 'cccccccc-0000-0000-0000-00000000000b';

SELECT expect_accept($$
    SELECT assert_period_entitlements_exact('cccccccc-0000-0000-0000-000000000001')
$$, 'exactness restored after correcting the one-paisa shortfall');

-- ---------------------------------------------------------------------------
-- The fiat gate
-- ---------------------------------------------------------------------------

\echo '=== Fiat gate :: confirmed anchor required ================================='

INSERT INTO chain_outbox (id, scheme_id, target_contract, function_name, payload_json,
                          payload_hash, idempotency_key, status, tx_hash, block_number,
                          confirmations, confirmed_at, environment_tag)
VALUES (:anchor_ok, :scheme_id, '0x00000000000000000000000000000000000000c1', 'anchorPeriod',
        '{}'::jsonb, decode(repeat('7a', 32), 'hex'), decode(repeat('7b', 32), 'hex'),
        'CONFIRMED', '0x' || repeat('3', 64), 8200000, 5, now(), 'LOCAL');

INSERT INTO chain_outbox (id, scheme_id, target_contract, function_name, payload_json,
                          payload_hash, idempotency_key, status, tx_hash, block_number,
                          confirmations, environment_tag)
VALUES (:anchor_pending, :scheme_id, '0x00000000000000000000000000000000000000c1', 'anchorPeriod',
        '{}'::jsonb, decode(repeat('7c', 32), 'hex'), decode(repeat('7d', 32), 'hex'),
        'CONFIRMING', '0x' || repeat('4', 64), 8200010, 2, 'LOCAL');

-- A CONFIRMED row must carry the evidence of its confirmation, or the gate has
-- nothing to check.
SELECT expect_reject($$
    INSERT INTO chain_outbox (scheme_id, target_contract, function_name, payload_json,
                              payload_hash, idempotency_key, status, confirmations, environment_tag)
    VALUES ('aaaaaaaa-0000-0000-0000-000000000001', '0x00000000000000000000000000000000000000c1',
            'anchorPeriod', '{}'::jsonb, decode(repeat('7e', 32), 'hex'),
            decode(repeat('7f', 32), 'hex'), 'CONFIRMED', 1, 'LOCAL')
$$, 'CONFIRMED outbox row with one confirmation and no tx hash');

-- The reorg hazard, closed: no payout against an unconfirmed anchor.
SELECT expect_reject($$
    INSERT INTO payout_instructions (entitlement_id, bank_account_id, amount_paise,
                                     gated_on_anchor_tx, idempotency_key)
    SELECT e.id, b.id, 171000000, 'dddddddd-0000-0000-0000-000000000002',
           decode(repeat('6a', 32), 'hex')
      FROM entitlements e, bank_accounts b
     WHERE e.snapshot_line_id = 'cccccccc-0000-0000-0000-00000000000b'
       AND b.investor_id = 'bbbbbbbb-0000-0000-0000-000000000002'
$$, 'payout gated on an anchor with only 2 confirmations');

SELECT expect_accept($$
    INSERT INTO payout_instructions (entitlement_id, bank_account_id, amount_paise,
                                     gated_on_anchor_tx, idempotency_key)
    SELECT e.id, b.id, 171000000, 'dddddddd-0000-0000-0000-000000000001',
           decode(repeat('6b', 32), 'hex')
      FROM entitlements e, bank_accounts b
     WHERE e.snapshot_line_id = 'cccccccc-0000-0000-0000-00000000000b'
       AND b.investor_id = 'bbbbbbbb-0000-0000-0000-000000000002'
$$, 'payout gated on a 5-confirmation anchor');

-- ---------------------------------------------------------------------------
-- Stale chain blocks distribution
-- ---------------------------------------------------------------------------

\echo '=== Stale chain blocks distribution ======================================='

-- A DIVERGED run that does not block payouts is the exact failure this design
-- exists to prevent, so the two cannot be recorded inconsistently.
SELECT expect_reject($$
    INSERT INTO reconciliation_runs (scheme_id, simulated_clock_value, depository_total_units,
                                     chain_total_units, depository_holder_count, chain_holder_count,
                                     divergence_count, status, blocks_payout, idempotency_key)
    VALUES ('aaaaaaaa-0000-0000-0000-000000000001', now(), 500, 498, 3, 3, 2,
            'DIVERGED', FALSE, decode(repeat('5a', 32), 'hex'))
$$, 'DIVERGED reconciliation that does not block payouts');

INSERT INTO reconciliation_runs (scheme_id, simulated_clock_value, depository_total_units,
                                 chain_total_units, depository_holder_count, chain_holder_count,
                                 divergence_count, status, blocks_payout, idempotency_key)
VALUES (:scheme_id, now(), 500, 498, 3, 3, 2, 'DIVERGED', TRUE, decode(repeat('5b', 32), 'hex'));

SELECT expect_reject($$
    INSERT INTO payout_instructions (entitlement_id, bank_account_id, amount_paise,
                                     gated_on_anchor_tx, idempotency_key)
    SELECT e.id, b.id, 26904000000, 'dddddddd-0000-0000-0000-000000000001',
           decode(repeat('6c', 32), 'hex')
      FROM entitlements e, bank_accounts b
     WHERE e.snapshot_line_id = 'cccccccc-0000-0000-0000-00000000000c'
       AND b.investor_id = 'bbbbbbbb-0000-0000-0000-000000000003'
$$, 'payout while a reconciliation divergence is unresolved');

UPDATE reconciliation_runs SET status = 'RESOLVED', resolved_at = now()
 WHERE idempotency_key = decode(repeat('5b', 32), 'hex');

SELECT expect_accept($$
    INSERT INTO payout_instructions (entitlement_id, bank_account_id, amount_paise,
                                     gated_on_anchor_tx, idempotency_key)
    SELECT e.id, b.id, 26904000000, 'dddddddd-0000-0000-0000-000000000001',
           decode(repeat('6d', 32), 'hex')
      FROM entitlements e, bank_accounts b
     WHERE e.snapshot_line_id = 'cccccccc-0000-0000-0000-00000000000c'
       AND b.investor_id = 'bbbbbbbb-0000-0000-0000-000000000003'
$$, 'payout resumes once the divergence is resolved');

-- ---------------------------------------------------------------------------
-- Reversal window
-- ---------------------------------------------------------------------------

\echo '=== Reversal window ======================================================='

UPDATE payout_instructions SET status = 'SETTLED', utr = 'UTR123456789', settled_at = now()
 WHERE idempotency_key = decode(repeat('6b', 32), 'hex');

SELECT expect_reject($$
    UPDATE distribution_periods
       SET status = 'REVERSED', reversal_reason = 'DATA_ENTRY_ERROR',
           reversal_narrative_sha256 = decode(repeat('4a', 32), 'hex'), reversed_at = now()
     WHERE id = 'cccccccc-0000-0000-0000-000000000001'
$$, 'reversing a period after fiat has settled');

-- ---------------------------------------------------------------------------
-- Settlement invariants
-- ---------------------------------------------------------------------------

\echo '=== Settlement invariants ================================================='

-- 25 + 3 + 472 = 500 units, IM holds exactly 25 and is excluded from the count.
-- Only 2 countable holders here, so the 200-holder floor must report unsatisfied:
-- an honest fixture rather than one arranged to pass.
DO $$
DECLARE
    r RECORD;
    total_ok BOOLEAN;
    im_ok BOOLEAN;
    holders_ok BOOLEAN;
BEGIN
    FOR r IN SELECT * FROM check_settlement_invariants('aaaaaaaa-0000-0000-0000-000000000001') LOOP
        RAISE NOTICE '      %-26s expected=%-8s actual=%-8s satisfied=%',
            r.invariant, r.expected, r.actual, r.satisfied;
        IF r.invariant = 'total_units_issued'       THEN total_ok   := r.satisfied; END IF;
        IF r.invariant = 'im_holding'               THEN im_ok      := r.satisfied; END IF;
        IF r.invariant = 'distinct_public_holders'  THEN holders_ok := r.satisfied; END IF;
    END LOOP;

    IF NOT total_ok THEN
        RAISE EXCEPTION 'FAIL  total units should reconcile to 500';
    END IF;
    IF NOT im_ok THEN
        RAISE EXCEPTION 'FAIL  IM holding should reconcile to 25';
    END IF;
    IF holders_ok THEN
        RAISE EXCEPTION 'FAIL  the holder floor should NOT be satisfied with 2 countable holders';
    END IF;

    RAISE NOTICE 'PASS  settlement  units and IM holding reconcile; holder floor correctly unsatisfied at 2 of 200';
END $$;

-- ---------------------------------------------------------------------------
-- Row level security
-- ---------------------------------------------------------------------------

\echo '=== Row level security ===================================================='

DO $$
DECLARE
    unprotected TEXT[];
    unforced    TEXT[];
BEGIN
    SELECT array_agg(c.relname ORDER BY c.relname) INTO unprotected
      FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
     WHERE n.nspname = 'public' AND c.relkind = 'r' AND NOT c.relrowsecurity;

    SELECT array_agg(c.relname ORDER BY c.relname) INTO unforced
      FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
     WHERE n.nspname = 'public' AND c.relkind = 'r' AND NOT c.relforcerowsecurity;

    IF unprotected IS NOT NULL THEN
        RAISE EXCEPTION 'FAIL  tables without RLS enabled: %', unprotected;
    END IF;
    IF unforced IS NOT NULL THEN
        RAISE EXCEPTION 'FAIL  tables without FORCE RLS (owner would bypass): %', unforced;
    END IF;

    RAISE NOTICE 'PASS  rls  every public table has RLS enabled and forced';
END $$;

DO $$
DECLARE
    permissive INTEGER;
BEGIN
    SELECT count(*) INTO permissive FROM pg_policies WHERE schemaname = 'public';
    IF permissive <> 0 THEN
        RAISE EXCEPTION 'FAIL  % policy/policies exist; M0 must be deny-by-default with none', permissive;
    END IF;
    RAISE NOTICE 'PASS  rls  no policies defined, so anon and authenticated see nothing';
END $$;

\echo ''
\echo '=========================================================================='
\echo ' ALL SCHEMA INVARIANT TESTS PASSED'
\echo '=========================================================================='

DROP FUNCTION expect_reject(TEXT, TEXT);
DROP FUNCTION expect_accept(TEXT, TEXT);
