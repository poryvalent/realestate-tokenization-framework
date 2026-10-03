import { Link, useParams } from 'react-router-dom';
import { usePoll } from '../api/usePoll';
import { useSession } from '../api/session';
import type { Offer } from '../api/types';
import { inr, int, demandRatio, dateTime, offerStatusLabel } from '../lib/format';
import { Card, LoadingCard, ErrorBox, StatusPill, Empty } from '../components/ui';

const LATE_STATES = new Set(['CLOSED', 'BOOK_FROZEN', 'SEED_COMMITTED', 'SEED_REVEALED', 'BALLOT_DRAWN', 'ALLOTMENT_FINALISED', 'SETTLEMENT_STARTED', 'SETTLED', 'PAYOUTS_DUE', 'DISTRIBUTED', 'ABORTED']);

export default function Offer() {
  const { id } = useParams();
  const { signedIn } = useSession();
  const { data, loading, error, refresh } = usePoll<Offer>(id ? `/offers/${id}` : null, {});

  if (loading) return <LoadingCard label="Loading offer" />;
  if (error) return <ErrorBox error={error} retry={refresh} />;
  if (!data) return <Empty title="Offer not found" />;
  const o = data;
  const sub = o.subscription;
  const acceptsBids = o.status === 'OPEN';

  return (
    <>
      <p style={{ marginTop: 24 }}><Link to={o.schemeId ? `/schemes/${o.schemeId}` : '/'}>← Back</Link></p>
      <h1>{o.offerType === 'FOLLOW_ON' ? 'Follow-on offer' : 'Initial offer'} · {inr(o.terms?.priceBandLowerPaise)}–{inr(o.terms?.priceBandUpperPaise)} <span style={{ fontWeight: 400, fontSize: '1.1rem' }}>per unit</span></h1>
      <p><StatusPill kind="offer" status={o.status} /></p>

      {!acceptsBids && LATE_STATES.has(o.status) && o.status !== 'OPEN' && (
        <div className="alert info" role="note"><p>This offer is {offerStatusLabel(o.status).toLowerCase()} and no longer accepts bids. The ballot and allotment below show exactly what happened.</p></div>
      )}

      <div className="grid2">
        <Card>
          <h3 style={{ marginTop: 0 }}>Terms</h3>
          <dl className="kv">
            <dt>Units on offer</dt><dd className="num">{int(o.terms?.unitsOnOffer)}</dd>
            <dt>Bid size</dt><dd className="num">{int(o.terms?.minBidUnits)}–{int(o.terms?.maxBidUnits)} units</dd>
            <dt>Price band</dt><dd className="num">{inr(o.terms?.priceBandLowerPaise)} – {inr(o.terms?.priceBandUpperPaise)}</dd>
            <dt>Opens</dt><dd>{dateTime(o.terms?.opensAt)}</dd>
            <dt>Closes</dt><dd>{dateTime(o.terms?.closesAt)}</dd>
            <dt>Allotment due</dt><dd>{dateTime(o.terms?.allotmentDueAt)}</dd>
            <dt>Min subscription</dt><dd className="num">{int(o.terms?.minSubscriptionUnits)} units · {int(o.terms?.minDistinctHolders)} holders</dd>
          </dl>
        </Card>
        <Card>
          <h3 style={{ marginTop: 0 }}>Subscription</h3>
          {sub ? (
            <dl className="kv">
              <dt>Bids</dt><dd className="num">{int(sub.bidCount)} from {int(sub.distinctBidders)} investors</dd>
              <dt>Units bid</dt><dd className="num">{int(sub.unitsBid)}</dd>
              <dt>Demand</dt><dd className="num">{demandRatio(sub.oversubscriptionNumerator, sub.oversubscriptionDenominator)}</dd>
              <dt>Funds blocked</dt><dd className="num">{int(sub.fundsBlockedCount)}</dd>
            </dl>
          ) : (
            <p>Bidding has not begun.</p>
          )}
          <hr className="rule" />
          {acceptsBids ? (
            signedIn ? (
              <Link className="btn primary" to={`/offers/${o.id}/bid`}>Place a bid</Link>
            ) : (
              <p>To bid you need an investor session. <Link to="/login">Sign in →</Link></p>
            )
          ) : (
            <p>Outcome: <Link to={`/offers/${o.id}/allotments`}>every bid's outcome, winners and losers →</Link></p>
          )}
        </Card>
      </div>

      <h2>Verify</h2>
      <Card flat>
        <p>
          <Link to={`/offers/${o.id}/ballot`}>Ballot ceremony →</Link> ·{' '}
          <Link to={`/offers/${o.id}/allotments`}>Allotments →</Link> ·{' '}
          <Link to={`/offers/${o.id}/documents`}>Offer documents →</Link> ·{' '}
          <Link to={`/offers/${o.id}/proofs`}>Bid proof checker →</Link>
        </p>
        <p style={{ marginBottom: 0 }}>Public. No sign-in. Anyone holding a bid reference can check it against the root on-chain.</p>
      </Card>

      <h2>Operators</h2>
      <Card flat>
        <p style={{ marginBottom: 0 }}>
          <Link to={`/admin/${o.id}`}>Open in operator console →</Link> (MANAGER / TRUSTEE / COMPLIANCE persona)
        </p>
      </Card>
    </>
  );
}
