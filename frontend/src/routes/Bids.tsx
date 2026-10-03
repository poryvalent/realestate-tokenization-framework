import { Link } from 'react-router-dom';
import { useSession } from '../api/session';
import { usePoll } from '../api/usePoll';
import { pageItems } from '../api/client';
import type { Bid } from '../api/types';
import { inr, int, dateTime } from '../lib/format';
import { Card, LoadingCard, ErrorBox, StatusPill, Empty } from '../components/ui';

const OUTCOME_COPY: Record<string, string> = {
  FULL: 'Every unit requested.',
  PARTIAL: 'Some units — the rest is released back to you.',
  NIL_BALLOT: 'Lost the draw. The block is released.',
  NIL_TECHNICAL: 'Rejected for a KYC, demat or funds reason.',
};

export default function Bids() {
  const { token, signedIn } = useSession();
  const { data, loading, error, refresh } = usePoll<Bid[] | { items: Bid[] }>(signedIn ? '/me/bids' : null, { token });
  const bids = pageItems(data);

  if (!signedIn) {
    return (
      <>
        <h1>My bids</h1>
        <Empty title="Not signed in" action={<Link className="btn primary" to="/login">Sign in</Link>} />
      </>
    );
  }
  if (loading) return <LoadingCard lines={5} label="Loading bids" />;
  if (error) return <ErrorBox error={error} retry={refresh} />;

  return (
    <>
      <h1>My bids</h1>
      <p>Before the draw, the outcome is empty. After it: full, partial, lost-the-draw, or rejected — and payable plus refund always equals what was blocked. If this screen ever shows otherwise, that is our bug, not your money.</p>
      {bids.length === 0 && <Empty title="No bids yet" body="Browse an open offer and place your first bid." action={<Link className="btn primary" to="/">Browse offers</Link>} />}
      {bids.map((b) => (
        <Card key={b.id}>
          <h3 style={{ marginTop: 0 }}>
            <Link to={`/offers/${b.offerId}`}>Offer {String(b.offerId).slice(0, 8)}</Link> · <span className="num">{int(b.unitsBid)} units @ {inr(b.pricePerUnitPaise)}</span>
          </h3>
          <p>
            <StatusPill kind="bid" status={b.status} />{' '}
            {b.rejectionReason && <StatusPill kind="bid" status={b.rejectionReason} />}
          </p>
          <dl className="kv">
            <dt>Bid reference</dt><dd className="mono">{b.bidRef}</dd>
            <dt>Total blocked</dt><dd className="num">{inr(b.totalAmountPaise)}</dd>
            <dt>Funds</dt><dd>{b.block ? `${(b.block.status ?? '—').toLowerCase()}${b.block.failureCode ? ` (${b.block.failureCode})` : ''}` : '—'}</dd>
            {b.allotment && (
              <>
                <dt>Outcome</dt><dd>{b.allotment.outcome} — {OUTCOME_COPY[b.allotment.outcome ?? ''] ?? ''}</dd>
                <dt>Payable / refund</dt><dd className="num">{inr(b.allotment.amountPayablePaise)} / {inr(b.allotment.refundAmountPaise)}</dd>
              </>
            )}
            <dt>Submitted</dt><dd>{dateTime(b.submittedAt)}</dd>
          </dl>
          {b.status === 'REJECTED_BALLOT' && (
            <p>A losing bid keeps this state with its reason even after the money is released — the release shows on the funds line above.</p>
          )}
          <p style={{ marginBottom: 0 }}><Link to={`/offers/${b.offerId}/proofs?ref=${encodeURIComponent(b.bidRef)}`}>Prove this bid was in the book →</Link></p>
        </Card>
      ))}
    </>
  );
}
