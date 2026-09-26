-- AcreSync M0 :: 0008 :: invariants that a CHECK constraint cannot express
--
-- Everything here guards a cross-row or cross-table property. The rule applied
-- throughout: if violating an invariant would cost money or break a regulatory
-- floor, it is enforced by the database, not only by the orchestrator. The
-- orchestrator is where bugs live; the database is where they get stopped.


-- ---------------------------------------------------------------------------
-- The simulated clock moves forward only
-- ---------------------------------------------------------------------------

-- Rewinding business time would invalidate every simulated_clock_value already
-- recorded against a ledger entry, which is the audit trail's only means of
-- reconstructing the order in which things happened.
CREATE OR REPLACE FUNCTION enforce_clock_monotonic()
RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.current_value < OLD.current_value THEN
        RAISE EXCEPTION
            'simulated clock cannot move backward: % -> %',
            OLD.current_value, NEW.current_value
            USING ERRCODE = 'check_violation';
    END IF;
    IF OLD.is_frozen AND NEW.current_value <> OLD.current_value THEN
        RAISE EXCEPTION 'simulated clock is frozen; unfreeze before advancing'
            USING ERRCODE = 'check_violation';
    END IF;
    NEW.last_advanced_at := now();
    RETURN NEW;
END;
$$;

CREATE TRIGGER simulated_clock_monotonic
    BEFORE UPDATE ON simulated_clock
    FOR EACH ROW EXECUTE FUNCTION enforce_clock_monotonic();

-- ---------------------------------------------------------------------------
-- holding_ledger is append-only
-- ---------------------------------------------------------------------------

-- An audit trail that can quietly edit its own history is not an audit trail.
-- Corrections are new rows carrying reversal_of_entry_id; this trigger removes
-- the alternative.
CREATE OR REPLACE FUNCTION forbid_mutation()
RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION
        '% is append-only: use a compensating entry instead of % ',
        TG_TABLE_NAME, TG_OP
        USING ERRCODE = 'restrict_violation';
END;
$$;

-- holding_ledger needs one narrow exception: anchor_status and anchored_tx have
-- to be settable as the transaction that anchors an entry confirms.
--
-- The exception is expressed as a column-level comparison rather than by
-- disabling the trigger. Disabling would require table ownership, would take an
-- ACCESS EXCLUSIVE lock, and would leave a window in which any update at all is
-- permitted. Comparing the rows instead means the substantive columns are
-- provably untouched on every path.
CREATE OR REPLACE FUNCTION forbid_ledger_mutation()
RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'holding_ledger is append-only: entries cannot be deleted'
            USING ERRCODE = 'restrict_violation';
    END IF;

    IF NEW.id                       IS DISTINCT FROM OLD.id
       OR NEW.scheme_id             IS DISTINCT FROM OLD.scheme_id
       OR NEW.investor_id           IS DISTINCT FROM OLD.investor_id
       OR NEW.entry_type            IS DISTINCT FROM OLD.entry_type
       OR NEW.units_delta           IS DISTINCT FROM OLD.units_delta
       OR NEW.balance_after         IS DISTINCT FROM OLD.balance_after
       OR NEW.depository_ref        IS DISTINCT FROM OLD.depository_ref
       OR NEW.counterparty_investor_id IS DISTINCT FROM OLD.counterparty_investor_id
       OR NEW.source                IS DISTINCT FROM OLD.source
       OR NEW.reversal_of_entry_id  IS DISTINCT FROM OLD.reversal_of_entry_id
       OR NEW.reason_code           IS DISTINCT FROM OLD.reason_code
       OR NEW.narrative_sha256      IS DISTINCT FROM OLD.narrative_sha256
       OR NEW.occurred_at           IS DISTINCT FROM OLD.occurred_at
       OR NEW.simulated_clock_value IS DISTINCT FROM OLD.simulated_clock_value
       OR NEW.idempotency_key       IS DISTINCT FROM OLD.idempotency_key
       OR NEW.created_at            IS DISTINCT FROM OLD.created_at
    THEN
        RAISE EXCEPTION
            'holding_ledger is append-only: only anchor_status and anchored_tx may be updated. Record a compensating entry instead.'
            USING ERRCODE = 'restrict_violation';
    END IF;

    RETURN NEW;
END;
$$;

CREATE TRIGGER holding_ledger_append_only
    BEFORE UPDATE OR DELETE ON holding_ledger
    FOR EACH ROW EXECUTE FUNCTION forbid_ledger_mutation();

-- register_snapshot_lines are equally immutable: they are the record-date freeze
-- that a published Merkle proof verifies against.
CREATE TRIGGER snapshot_lines_append_only
    BEFORE UPDATE OR DELETE ON register_snapshot_lines
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

CREATE TRIGGER admin_actions_append_only
    BEFORE UPDATE OR DELETE ON admin_actions
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

-- ---------------------------------------------------------------------------
-- Ledger balance continuity :: invariant 4
-- ---------------------------------------------------------------------------

-- balance_after must equal the previous balance plus this delta. Without this,
-- the ledger and the live mirror can drift apart silently and the first symptom
-- is a failed settlement finalisation with no way to tell which entry was wrong.
CREATE OR REPLACE FUNCTION enforce_ledger_continuity()
RETURNS TRIGGER LANGUAGE plpgsql AS $$
DECLARE
    prev_balance INTEGER;
BEGIN
    SELECT balance_after INTO prev_balance
      FROM holding_ledger
     WHERE scheme_id = NEW.scheme_id
       AND investor_id = NEW.investor_id
     ORDER BY created_at DESC, id DESC
     LIMIT 1;

    prev_balance := COALESCE(prev_balance, 0);

    IF NEW.balance_after <> prev_balance + NEW.units_delta THEN
        RAISE EXCEPTION
            'ledger discontinuity for investor %: previous balance %, delta %, claimed balance_after %',
            NEW.investor_id, prev_balance, NEW.units_delta, NEW.balance_after
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER holding_ledger_continuity
    BEFORE INSERT ON holding_ledger
    FOR EACH ROW EXECUTE FUNCTION enforce_ledger_continuity();

-- ---------------------------------------------------------------------------
-- Bid bounds against the offer :: cross-row
-- ---------------------------------------------------------------------------

CREATE OR REPLACE FUNCTION enforce_bid_bounds()
RETURNS TRIGGER LANGUAGE plpgsql AS $$
DECLARE
    o RECORD;
BEGIN
    SELECT min_bid_units, max_bid_units, price_band_lower_paise, price_band_upper_paise
      INTO o
      FROM offers WHERE id = NEW.offer_id;

    IF NEW.units_bid < o.min_bid_units THEN
        RAISE EXCEPTION 'bid of % units is below the offer minimum of %',
            NEW.units_bid, o.min_bid_units USING ERRCODE = 'check_violation';
    END IF;

    -- The whale cap. Exceeding it does not merely favour one bidder; it can make
    -- the 200-unitholder floor arithmetically unreachable, which would render the
    -- scheme unlistable.
    IF NEW.units_bid > o.max_bid_units THEN
        RAISE EXCEPTION 'bid of % units exceeds the offer maximum of %',
            NEW.units_bid, o.max_bid_units USING ERRCODE = 'check_violation';
    END IF;

    IF NEW.price_per_unit_paise < o.price_band_lower_paise
       OR NEW.price_per_unit_paise > o.price_band_upper_paise THEN
        RAISE EXCEPTION 'bid price % paise is outside the band [%, %]',
            NEW.price_per_unit_paise, o.price_band_lower_paise, o.price_band_upper_paise
            USING ERRCODE = 'check_violation';
    END IF;

    RETURN NEW;
END;
$$;

CREATE TRIGGER bids_within_offer_bounds
    BEFORE INSERT OR UPDATE OF units_bid, price_per_unit_paise ON bids
    FOR EACH ROW EXECUTE FUNCTION enforce_bid_bounds();

-- ---------------------------------------------------------------------------
-- Commit-reveal ordering :: the ballot's security property
-- ---------------------------------------------------------------------------

-- The seed plaintext must not exist anywhere until the commitment is confirmed
-- on-chain. If it can be written earlier, an operator who dislikes a draw can
-- claim a different secret was always intended, and the commitment proves
-- nothing.
CREATE OR REPLACE FUNCTION enforce_reveal_order()
RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.seed_plaintext IS NOT NULL THEN
        IF NEW.seed_commitment IS NULL THEN
            RAISE EXCEPTION 'seed plaintext written without a commitment'
                USING ERRCODE = 'check_violation';
        END IF;
        IF NEW.commitment_anchored_tx IS NULL THEN
            RAISE EXCEPTION 'seed plaintext written before the commitment was anchored on-chain'
                USING ERRCODE = 'check_violation';
        END IF;
        IF NEW.target_block IS NULL THEN
            RAISE EXCEPTION 'seed plaintext written without a target block'
                USING ERRCODE = 'check_violation';
        END IF;
    END IF;

    -- The bid book must be anchored before a commitment exists, so that the draw
    -- is bound to a bid set nobody can still change.
    IF NEW.seed_commitment IS NOT NULL AND NEW.bidbook_merkle_root IS NULL THEN
        RAISE EXCEPTION 'seed committed before the bid book was anchored'
            USING ERRCODE = 'check_violation';
    END IF;

    -- On a recommit the stored commitment must be reused unchanged. Allowing a
    -- new secret would let an operator reroll the outcome by abandoning the
    -- reveal window, which is the grinding vector the three-attempt cap exists
    -- to bound.
    IF TG_OP = 'UPDATE'
       AND OLD.seed_commitment IS NOT NULL
       AND NEW.seed_commitment IS DISTINCT FROM OLD.seed_commitment THEN
        RAISE EXCEPTION 'seed commitment is immutable once set; a recommit may only change the target block'
            USING ERRCODE = 'check_violation';
    END IF;

    RETURN NEW;
END;
$$;

CREATE TRIGGER ballot_runs_enforce_reveal_order
    BEFORE INSERT OR UPDATE ON ballot_runs
    FOR EACH ROW EXECUTE FUNCTION enforce_reveal_order();

-- ---------------------------------------------------------------------------
-- Fiat cannot move on an unconfirmed anchor :: invariant 8
-- ---------------------------------------------------------------------------

-- The single most dangerous link in the architecture is the orchestrator reading
-- chain state and then irreversibly moving money. A reorg after the transfer has
-- cleared is unrecoverable, so a payout instruction may only be created against
-- an outbox row that has already reached CONFIRMED with at least five
-- confirmations.
CREATE OR REPLACE FUNCTION enforce_payout_anchor_confirmed()
RETURNS TRIGGER LANGUAGE plpgsql AS $$
DECLARE
    a RECORD;
BEGIN
    SELECT status, confirmations, tx_hash
      INTO a
      FROM chain_outbox WHERE id = NEW.gated_on_anchor_tx;

    IF NOT FOUND THEN
        RAISE EXCEPTION 'payout references a non-existent anchor %', NEW.gated_on_anchor_tx
            USING ERRCODE = 'foreign_key_violation';
    END IF;

    IF a.status <> 'CONFIRMED' THEN
        RAISE EXCEPTION
            'payout blocked: anchor % is in state %, not CONFIRMED',
            NEW.gated_on_anchor_tx, a.status
            USING ERRCODE = 'check_violation';
    END IF;

    IF a.confirmations < 5 THEN
        RAISE EXCEPTION
            'payout blocked: anchor % has only % confirmations, 5 required',
            NEW.gated_on_anchor_tx, a.confirmations
            USING ERRCODE = 'check_violation';
    END IF;

    RETURN NEW;
END;
$$;

CREATE TRIGGER payouts_require_confirmed_anchor
    BEFORE INSERT OR UPDATE OF gated_on_anchor_tx ON payout_instructions
    FOR EACH ROW EXECUTE FUNCTION enforce_payout_anchor_confirmed();

-- ---------------------------------------------------------------------------
-- A stale chain blocks distribution
-- ---------------------------------------------------------------------------

CREATE OR REPLACE FUNCTION enforce_no_payout_while_diverged()
RETURNS TRIGGER LANGUAGE plpgsql AS $$
DECLARE
    v_scheme_id UUID;
    v_blocking  INTEGER;
BEGIN
    SELECT dp.scheme_id INTO v_scheme_id
      FROM entitlements e
      JOIN distribution_periods dp ON dp.id = e.distribution_period_id
     WHERE e.id = NEW.entitlement_id;

    SELECT count(*) INTO v_blocking
      FROM reconciliation_runs
     WHERE scheme_id = v_scheme_id
       AND blocks_payout
       AND resolved_at IS NULL;

    IF v_blocking > 0 THEN
        RAISE EXCEPTION
            'payout blocked: % unresolved reconciliation divergence(s) for scheme %. The depository is the legal register; resolve the divergence before distributing.',
            v_blocking, v_scheme_id
            USING ERRCODE = 'check_violation';
    END IF;

    RETURN NEW;
END;
$$;

CREATE TRIGGER payouts_blocked_while_chain_stale
    BEFORE INSERT ON payout_instructions
    FOR EACH ROW EXECUTE FUNCTION enforce_no_payout_while_diverged();

-- ---------------------------------------------------------------------------
-- Entitlements must sum exactly :: invariant 7
-- ---------------------------------------------------------------------------

-- Not "within a tolerance". A single stray paisa in a regulated distribution is
-- an audit finding, and the largest-remainder allocation exists precisely so
-- that this equality holds.
CREATE OR REPLACE FUNCTION assert_period_entitlements_exact(p_period_id UUID)
RETURNS VOID LANGUAGE plpgsql AS $$
DECLARE
    v_distributed BIGINT;
    v_sum         BIGINT;
    v_units       BIGINT;
    v_snapshot    INTEGER;
BEGIN
    SELECT distributed_paise INTO v_distributed
      FROM distribution_periods WHERE id = p_period_id;

    IF v_distributed IS NULL THEN
        RAISE EXCEPTION 'period % has no distributed amount', p_period_id
            USING ERRCODE = 'check_violation';
    END IF;

    SELECT COALESCE(sum(gross_entitlement_paise + residue_paise_awarded), 0),
           COALESCE(sum(units), 0),
           max(snapshot_total_units)
      INTO v_sum, v_units, v_snapshot
      FROM entitlements WHERE distribution_period_id = p_period_id;

    IF v_sum <> v_distributed THEN
        RAISE EXCEPTION
            'entitlement sum % does not equal distributed amount % for period % (difference % paise)',
            v_sum, v_distributed, p_period_id, v_sum - v_distributed
            USING ERRCODE = 'check_violation';
    END IF;

    IF v_units <> v_snapshot THEN
        RAISE EXCEPTION
            'entitlement units % do not equal snapshot total units % for period %',
            v_units, v_snapshot, p_period_id
            USING ERRCODE = 'check_violation';
    END IF;
END;
$$;

CREATE OR REPLACE FUNCTION enforce_entitlements_exact_on_finalise()
RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.status = 'ENTITLEMENTS_ANCHORED' AND OLD.status <> 'ENTITLEMENTS_ANCHORED' THEN
        PERFORM assert_period_entitlements_exact(NEW.id);
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER periods_entitlements_exact
    BEFORE UPDATE OF status ON distribution_periods
    FOR EACH ROW EXECUTE FUNCTION enforce_entitlements_exact_on_finalise();

-- ---------------------------------------------------------------------------
-- A period may not be reversed once fiat has settled :: the Class 2/3 boundary
-- ---------------------------------------------------------------------------

CREATE OR REPLACE FUNCTION enforce_reversal_window()
RETURNS TRIGGER LANGUAGE plpgsql AS $$
DECLARE
    v_settled INTEGER;
BEGIN
    IF NEW.status = 'REVERSED' AND OLD.status <> 'REVERSED' THEN
        IF OLD.status IN ('PAYOUTS_CONFIRMED', 'CLOSED') THEN
            RAISE EXCEPTION
                'period % cannot be reversed from state %: fiat has settled. Use a carry-forward adjustment instead.',
                NEW.id, OLD.status
                USING ERRCODE = 'check_violation';
        END IF;

        SELECT count(*) INTO v_settled
          FROM payout_instructions pi
          JOIN entitlements e ON e.id = pi.entitlement_id
         WHERE e.distribution_period_id = NEW.id
           AND pi.status IN ('SETTLED', 'PROCESSING', 'SUBMITTED');

        IF v_settled > 0 THEN
            RAISE EXCEPTION
                'period % cannot be reversed: % payout(s) are in flight or settled. Use a carry-forward adjustment instead.',
                NEW.id, v_settled
                USING ERRCODE = 'check_violation';
        END IF;
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER periods_reversal_window
    BEFORE UPDATE OF status ON distribution_periods
    FOR EACH ROW EXECUTE FUNCTION enforce_reversal_window();

-- ---------------------------------------------------------------------------
-- Settlement invariants :: the permissionless finalisation gate
-- ---------------------------------------------------------------------------

-- The off-chain mirror of the contract's finaliseSettlement checks. Exposed as a
-- function returning the individual results rather than a single boolean,
-- because "settlement failed" is not actionable whereas "197 holders against a
-- floor of 200" is.
CREATE OR REPLACE FUNCTION check_settlement_invariants(p_scheme_id UUID)
RETURNS TABLE (
    invariant TEXT,
    expected  BIGINT,
    actual    BIGINT,
    satisfied BOOLEAN
) LANGUAGE plpgsql AS $$
DECLARE
    s RECORD;
BEGIN
    SELECT total_units, im_units, min_public_holders INTO s
      FROM schemes WHERE id = p_scheme_id;

    RETURN QUERY
    SELECT 'total_units_issued'::TEXT,
           s.total_units::BIGINT,
           COALESCE(sum(units), 0)::BIGINT,
           COALESCE(sum(units), 0) = s.total_units
      FROM unit_holdings WHERE scheme_id = p_scheme_id;

    RETURN QUERY
    SELECT 'distinct_public_holders'::TEXT,
           s.min_public_holders::BIGINT,
           count(*)::BIGINT,
           count(*) >= s.min_public_holders
      FROM unit_holdings
     WHERE scheme_id = p_scheme_id
       AND units > 0
       AND NOT is_excluded_from_holder_count;

    RETURN QUERY
    SELECT 'im_holding'::TEXT,
           s.im_units::BIGINT,
           COALESCE(sum(units), 0)::BIGINT,
           COALESCE(sum(units), 0) = s.im_units
      FROM unit_holdings
     WHERE scheme_id = p_scheme_id
       AND is_excluded_from_holder_count;

    RETURN QUERY
    SELECT 'ledger_matches_mirror'::TEXT,
           COALESCE((SELECT sum(units) FROM unit_holdings WHERE scheme_id = p_scheme_id), 0)::BIGINT,
           COALESCE((SELECT sum(units_delta) FROM holding_ledger WHERE scheme_id = p_scheme_id), 0)::BIGINT,
           COALESCE((SELECT sum(units) FROM unit_holdings WHERE scheme_id = p_scheme_id), 0)
             = COALESCE((SELECT sum(units_delta) FROM holding_ledger WHERE scheme_id = p_scheme_id), 0);
END;
$$;

COMMENT ON FUNCTION check_settlement_invariants IS
    'Off-chain mirror of the contract finaliseSettlement gate. Returns one row per invariant so a failure names the specific number that is wrong.';

