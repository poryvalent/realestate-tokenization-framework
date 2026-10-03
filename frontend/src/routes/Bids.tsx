import { Link } from 'react-router-dom';
import { motion } from 'framer-motion';
import { ArrowUpRight } from 'lucide-react';
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
      <div className="mx-auto max-w-3xl">
        <motion.h1
          initial={{ opacity: 0, y: 24, filter: 'blur(6px)' }}
          animate={{ opacity: 1, y: 0, filter: 'blur(0px)' }}
          transition={{ duration: 0.6, ease: [0.25, 0.1, 0.25, 1] }}
          className="text-gradient text-4xl font-semibold tracking-tight sm:text-5xl"
        >
          My bids
        </motion.h1>
        <div className="mt-6">
          <Empty
            title="Not signed in"
            action={
              <Link
                to="/login"
                className="rounded-full bg-white px-5 py-2 text-sm font-semibold text-[#020319] transition hover:bg-white/85"
              >
                Sign in
              </Link>
            }
          />
        </div>
      </div>
    );
  }
  if (loading) return <LoadingCard lines={5} label="Loading bids" />;
  if (error) return <ErrorBox error={error} retry={refresh} />;

  return (
    <div className="mx-auto max-w-3xl">
      <motion.h1
        initial={{ opacity: 0, y: 24, filter: 'blur(6px)' }}
        animate={{ opacity: 1, y: 0, filter: 'blur(0px)' }}
        transition={{ duration: 0.6, ease: [0.25, 0.1, 0.25, 1] }}
        className="text-gradient text-4xl font-semibold tracking-tight sm:text-5xl"
      >
        My bids
      </motion.h1>
      <motion.p
        initial={{ opacity: 0, y: 16 }}
        animate={{ opacity: 1, y: 0 }}
        transition={{ duration: 0.5, delay: 0.12 }}
        className="mt-4 max-w-2xl text-sm leading-relaxed text-white/60"
      >
        Before the draw, the outcome is empty. After it: full, partial, lost-the-draw, or rejected — and
        payable plus refund always equals what was blocked. If this screen ever shows otherwise, that is
        our bug, not your money.
      </motion.p>

      {bids.length === 0 && (
        <div className="mt-6">
          <Empty
            title="No bids yet"
            body="Browse an open offer and place your first bid."
            action={
              <Link
                to="/"
                className="rounded-full bg-white px-5 py-2 text-sm font-semibold text-[#020319] transition hover:bg-white/85"
              >
                Browse offers
              </Link>
            }
          />
        </div>
      )}
      <div className="mt-6 space-y-5">
        {bids.map((b, i) => (
          <motion.div
            key={b.id}
            initial={{ opacity: 0, y: 20 }}
            animate={{ opacity: 1, y: 0 }}
            transition={{ duration: 0.45, delay: Math.min(i, 5) * 0.06 }}
          >
            <Card>
              <h3 className="mt-0 text-base font-semibold text-white">
                <Link to={`/offers/${b.offerId}`} className="hover:underline">
                  Offer {String(b.offerId).slice(0, 8)}
                </Link>{' '}
                <span className="font-normal text-white/50">·</span>{' '}
                <span className="tabular-nums">{int(b.unitsBid)} units @ {inr(b.pricePerUnitPaise)}</span>
              </h3>
              <p className="mt-2 flex flex-wrap gap-2">
                <StatusPill kind="bid" status={b.status} />{' '}
                {b.rejectionReason && <StatusPill kind="bid" status={b.rejectionReason} />}
              </p>
              <dl className="mt-4 grid grid-cols-[130px_1fr] gap-x-4 gap-y-2 text-sm sm:grid-cols-[160px_1fr]">
                <dt className="text-white/40">Bid reference</dt>
                <dd className="font-mono text-[13px] text-white/85">{b.bidRef}</dd>
                <dt className="text-white/40">Total blocked</dt>
                <dd className="tabular-nums text-white/90">{inr(b.totalAmountPaise)}</dd>
                <dt className="text-white/40">Funds</dt>
                <dd className="text-white/70">{b.block ? `${(b.block.status ?? '—').toLowerCase()}${b.block.failureCode ? ` (${b.block.failureCode})` : ''}` : '—'}</dd>
                {b.allotment && (
                  <>
                    <dt className="text-white/40">Outcome</dt>
                    <dd className="text-white/80">{b.allotment.outcome} — {OUTCOME_COPY[b.allotment.outcome ?? ''] ?? ''}</dd>
                    <dt className="text-white/40">Payable / refund</dt>
                    <dd className="tabular-nums text-white/90">{inr(b.allotment.amountPayablePaise)} / {inr(b.allotment.refundAmountPaise)}</dd>
                  </>
                )}
                <dt className="text-white/40">Submitted</dt>
                <dd className="text-white/70">{dateTime(b.submittedAt)}</dd>
              </dl>
              {b.status === 'REJECTED_BALLOT' && (
                <p className="mt-3 text-xs leading-relaxed text-white/50">
                  A losing bid keeps this state with its reason even after the money is released — the release shows on the funds line above.
                </p>
              )}
              <p className="mb-0 mt-4">
                <Link
                  to={`/offers/${b.offerId}/proofs?ref=${encodeURIComponent(b.bidRef)}`}
                  className="inline-flex items-center gap-1 text-sm text-brand-mist hover:underline"
                >
                  Prove this bid was in the book <ArrowUpRight className="h-3.5 w-3.5" />
                </Link>
              </p>
            </Card>
          </motion.div>
        ))}
      </div>
    </div>
  );
}
