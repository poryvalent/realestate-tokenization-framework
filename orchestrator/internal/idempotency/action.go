package idempotency

import "fmt"

// Action is the closed vocabulary of mutating operations in AcreSync.
//
// Every admin API call and every relayer submission names one of these. The
// vocabulary is closed on purpose: an unrecognised action cannot produce an
// idempotency key, so a new operation cannot be shipped without being
// deliberately registered here and in the corresponding Postgres enum.
//
// INVARIANT: this list must remain identical to the `admin_action` enum in
// db/migrations/0001_extensions_and_enums.sql. Drift between them means an
// action that derives a key in Go but cannot be persisted, which surfaces as a
// runtime insert failure rather than a compile error. TestActionsMatchDDL in
// action_test.go parses the migration and enforces the match.
type Action string

const (
	// Clock control
	ActionAdvanceClock Action = "ADVANCE_CLOCK"
	ActionFreezeClock  Action = "FREEZE_CLOCK"
	ActionUnfreezeClk  Action = "UNFREEZE_CLOCK"

	// Scheme and asset setup
	ActionCreateScheme        Action = "CREATE_SCHEME"
	ActionCreateSPV           Action = "CREATE_SPV"
	ActionCreateProperty      Action = "CREATE_PROPERTY"
	ActionCreateLease         Action = "CREATE_LEASE"
	ActionUploadDocument      Action = "UPLOAD_DOCUMENT"
	ActionAnchorDocument      Action = "ANCHOR_DOCUMENT"
	ActionSupersedeDocument   Action = "SUPERSEDE_DOCUMENT"
	ActionDeployContract      Action = "DEPLOY_CONTRACT"
	ActionAdvanceSchemeStatus Action = "ADVANCE_SCHEME_STATUS"
	ActionIMSubscription      Action = "IM_SUBSCRIPTION"
	ActionSetLockIn           Action = "SET_LOCK_IN"

	// Primary market
	ActionCreateOffer        Action = "CREATE_OFFER"
	ActionOpenOffer          Action = "OPEN_OFFER"
	ActionSeedBids           Action = "SEED_BIDS"
	ActionSubmitBid          Action = "SUBMIT_BID"
	ActionCloseOffer         Action = "CLOSE_OFFER"
	ActionFreezeBook         Action = "FREEZE_BOOK"
	ActionFeasibilityCheck   Action = "FEASIBILITY_CHECK"
	ActionAnchorBidbook      Action = "ANCHOR_BIDBOOK"
	ActionCommitSeed         Action = "COMMIT_SEED"
	ActionRevealSeed         Action = "REVEAL_SEED"
	ActionRecommitSeed       Action = "RECOMMIT_SEED"
	ActionEscalateBallot     Action = "ESCALATE_BALLOT"
	ActionRunBallot          Action = "RUN_BALLOT"
	ActionFinaliseAllotment  Action = "FINALISE_ALLOTMENT"
	ActionAbortOffer         Action = "ABORT_OFFER"
	ActionSimulateASBAResult Action = "SIMULATE_ASBA_RESULT"

	// ASBA fund movement. Added by migration 0012.
	//
	// Three actions rather than one because their idempotency requirements differ. A block is placed
	// once per bid and must never repeat, so its key needs only the bid. A settlement carries the
	// allotted amount and has to produce a different key if that amount is corrected. A release carries
	// no amount at all, so there is nothing for its key to disagree about.
	ActionRequestASBABlock Action = "REQUEST_ASBA_BLOCK"
	ActionSettleASBABlock  Action = "SETTLE_ASBA_BLOCK"
	ActionReleaseASBABlock Action = "RELEASE_ASBA_BLOCK"

	// Settlement
	ActionBeginSettlement    Action = "BEGIN_SETTLEMENT"
	ActionSettleBatch        Action = "SETTLE_BATCH"
	ActionFinaliseSettlement Action = "FINALISE_SETTLEMENT"

	// Distribution
	ActionCreatePeriod             Action = "CREATE_PERIOD"
	ActionInjectRent               Action = "INJECT_RENT"
	ActionAddNDCFLineItem          Action = "ADD_NDCF_LINE_ITEM"
	ActionDraftNDCF                Action = "DRAFT_NDCF"
	ActionApproveNDCFIM            Action = "APPROVE_NDCF_IM"
	ActionApproveNDCFTrustee       Action = "APPROVE_NDCF_TRUSTEE"
	ActionDeclareRecordDate        Action = "DECLARE_RECORD_DATE"
	ActionTakeSnapshot             Action = "TAKE_SNAPSHOT"
	ActionAnchorPeriod             Action = "ANCHOR_PERIOD"
	ActionAnchorEntitlementsBatch  Action = "ANCHOR_ENTITLEMENTS_BATCH"
	ActionFinaliseEntitlements     Action = "FINALISE_ENTITLEMENTS"
	ActionInstructPayout           Action = "INSTRUCT_PAYOUT"
	ActionSimulatePayoutResult     Action = "SIMULATE_PAYOUT_RESULT"
	ActionConfirmPayouts           Action = "CONFIRM_PAYOUTS"
	ActionClosePeriod              Action = "CLOSE_PERIOD"
	ActionApproveReversal          Action = "APPROVE_REVERSAL"
	ActionReversePeriod            Action = "REVERSE_PERIOD"
	ActionRecordPayoutAdjustment   Action = "RECORD_PAYOUT_ADJUSTMENT"

	// Register and reconciliation
	ActionDepositoryTransfer     Action = "DEPOSITORY_TRANSFER"
	ActionDepositoryTransmission Action = "DEPOSITORY_TRANSMISSION"
	ActionRunReconciliation      Action = "RUN_RECONCILIATION"
	ActionPushReconciliation     Action = "PUSH_RECONCILIATION"
	ActionCorrectHolding         Action = "CORRECT_HOLDING"

	// Outbox operations
	ActionRetryOutbox      Action = "RETRY_OUTBOX"
	ActionCancelOutbox     Action = "CANCEL_OUTBOX"
	ActionDeadLetterOutbox Action = "DEAD_LETTER_OUTBOX"

	// Role and pause management
	ActionProposeRoleChange Action = "PROPOSE_ROLE_CHANGE"
	ActionExecuteRoleChange Action = "EXECUTE_ROLE_CHANGE"
	ActionCancelRoleChange  Action = "CANCEL_ROLE_CHANGE"
	ActionRevokeRelayer     Action = "REVOKE_RELAYER"
	ActionPause             Action = "PAUSE"
	ActionUnpause           Action = "UNPAUSE"
)

// AllActions is the authoritative registry. Order is irrelevant to key
// derivation but is kept grouped for readability.
var AllActions = []Action{
	ActionAdvanceClock, ActionFreezeClock, ActionUnfreezeClk,

	ActionCreateScheme, ActionCreateSPV, ActionCreateProperty, ActionCreateLease,
	ActionUploadDocument, ActionAnchorDocument, ActionSupersedeDocument,
	ActionDeployContract, ActionAdvanceSchemeStatus, ActionIMSubscription,
	ActionSetLockIn,

	ActionCreateOffer, ActionOpenOffer, ActionSeedBids, ActionSubmitBid,
	ActionCloseOffer, ActionFreezeBook, ActionFeasibilityCheck,
	ActionAnchorBidbook, ActionCommitSeed, ActionRevealSeed, ActionRecommitSeed,
	ActionEscalateBallot, ActionRunBallot, ActionFinaliseAllotment,
	ActionAbortOffer, ActionSimulateASBAResult,
	ActionRequestASBABlock, ActionSettleASBABlock, ActionReleaseASBABlock,

	ActionBeginSettlement, ActionSettleBatch, ActionFinaliseSettlement,

	ActionCreatePeriod, ActionInjectRent, ActionAddNDCFLineItem, ActionDraftNDCF,
	ActionApproveNDCFIM, ActionApproveNDCFTrustee, ActionDeclareRecordDate,
	ActionTakeSnapshot, ActionAnchorPeriod, ActionAnchorEntitlementsBatch,
	ActionFinaliseEntitlements, ActionInstructPayout, ActionSimulatePayoutResult,
	ActionConfirmPayouts, ActionClosePeriod, ActionApproveReversal,
	ActionReversePeriod, ActionRecordPayoutAdjustment,

	ActionDepositoryTransfer, ActionDepositoryTransmission,
	ActionRunReconciliation, ActionPushReconciliation, ActionCorrectHolding,

	ActionRetryOutbox, ActionCancelOutbox, ActionDeadLetterOutbox,

	ActionProposeRoleChange, ActionExecuteRoleChange, ActionCancelRoleChange,
	ActionRevokeRelayer, ActionPause, ActionUnpause,
}

var actionSet = func() map[Action]struct{} {
	m := make(map[Action]struct{}, len(AllActions))
	for _, a := range AllActions {
		if _, dup := m[a]; dup {
			panic(fmt.Sprintf("idempotency: duplicate action registered: %s", a))
		}
		m[a] = struct{}{}
	}
	return m
}()

// Valid reports whether a is a registered action.
func (a Action) Valid() bool {
	_, ok := actionSet[a]
	return ok
}

func (a Action) String() string { return string(a) }
