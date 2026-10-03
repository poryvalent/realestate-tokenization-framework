import type { ReactNode } from 'react';
import { Link, useParams } from 'react-router-dom';
import { motion } from 'framer-motion';
import {
  ArrowLeft,
  ArrowRight,
  FileText,
  Fingerprint,
  Gavel,
  ScrollText,
  ShieldCheck,
  Wallet,
} from 'lucide-react';
import { usePoll } from '../api/usePoll';
import { useSession } from '../api/session';
import type { Offer } from '../api/types';
import { inr, int, demandRatio, dateTime, offerStatusLabel } from '../lib/format';
import { Card, LoadingCard, ErrorBox, StatusPill, Empty } from '../components/ui';

const EASE: [number, number, number, number] = [0.25, 0.1, 0.25, 1];

const LATE_STATES = new Set(['CLOSED', 'BOOK_FROZEN', 'SEED_COMMITTED', 'SEED_REVEALED', 'BALLOT_DRAWN', 'ALLOTMENT_FINALISED', 'SETTLEMENT_STARTED', 'SETTLED', 'PAYOUTS_DUE', 'DISTRIBUTED', 'ABORTED']);

function Fade({
  children,
  delay = 0,
  className,
}: {
  children: ReactNode;
  delay?: number;
  className?: string;
}) {
  return (
    <motion.div
      className={className}
      initial={{ opacity: 0, y: 28, filter: 'blur(6px)' }}
      whileInView={{ opacity: 1, y: 0, filter: 'blur(0px)' }}
      viewport={{ once: true, margin: '-60px' }}
      transition={{ duration: 0.6, delay, ease: EASE }}
    >
      {children}
    </motion.div>
  );
}

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
      <motion.p
        initial={{ opacity: 0, y: 12 }}
        animate={{ opacity: 1, y: 0 }}
        transition={{ duration: 0.4, ease: EASE }}
        className="mt-6"
      >
        <Link
          to={o.schemeId ? `/schemes/${o.schemeId}` : '/'}
          className="inline-flex items-center gap-1.5 text-sm text-white/60 transition hover:text-white"
        >
          <ArrowLeft className="h-4 w-4" />
          Back
        </Link>
      </motion.p>

      <motion.div
        initial={{ opacity: 0, y: 28, filter: 'blur(8px)' }}
        animate={{ opacity: 1, y: 0, filter: 'blur(0px)' }}
        transition={{ duration: 0.65, ease: EASE }}
      >
        <p className="mt-4 text-xs font-medium uppercase tracking-[0.22em] text-brand-mist">
          {o.offerType === 'FOLLOW_ON' ? 'Follow-on offer' : 'Initial offer'} ·{' '}
          <span className="font-mono normal-case tracking-normal">{String(o.id)}</span>
        </p>
        <h1 className="text-gradient mt-2 font-sans text-4xl font-semibold leading-[1.05] tracking-tight sm:text-5xl lg:text-6xl">
          {inr(o.terms?.priceBandLowerPaise)}–{inr(o.terms?.priceBandUpperPaise)}{' '}
          <span className="text-2xl font-normal text-white/60 sm:text-3xl">per unit</span>
        </h1>
        <p className="mt-4">
          <StatusPill kind="offer" status={o.status} />
        </p>
      </motion.div>

      {!acceptsBids && LATE_STATES.has(o.status) && o.status !== 'OPEN' && (
        <Fade delay={0.05} className="mt-6">
          <div
            className="rounded-2xl border border-sky-300/25 bg-sky-300/[0.08] p-5 text-sm leading-relaxed text-sky-100/90"
            role="note"
          >
            This offer is {offerStatusLabel(o.status).toLowerCase()} and no longer accepts bids. The ballot and allotment below show exactly what happened.
          </div>
        </Fade>
      )}

      <div className="mt-8 grid gap-4 lg:grid-cols-2">
        <Fade delay={0.08}>
          <Card>
            <div className="flex items-center gap-2.5">
              <ScrollText className="h-5 w-5 text-brand-mist" aria-hidden="true" />
              <h3 className="font-sans text-lg font-semibold tracking-tight text-white">Terms</h3>
            </div>
            <dl className="mt-5 space-y-3 text-sm">
              <div className="flex items-baseline justify-between gap-4 border-b border-white/10 pb-3">
                <dt className="text-white/55">Units on offer</dt>
                <dd className="font-mono text-white">{int(o.terms?.unitsOnOffer)}</dd>
              </div>
              <div className="flex items-baseline justify-between gap-4 border-b border-white/10 pb-3">
                <dt className="text-white/55">Bid size</dt>
                <dd className="font-mono text-white">{int(o.terms?.minBidUnits)}–{int(o.terms?.maxBidUnits)} units</dd>
              </div>
              <div className="flex items-baseline justify-between gap-4 border-b border-white/10 pb-3">
                <dt className="text-white/55">Price band</dt>
                <dd className="font-mono text-white">{inr(o.terms?.priceBandLowerPaise)} – {inr(o.terms?.priceBandUpperPaise)}</dd>
              </div>
              <div className="flex items-baseline justify-between gap-4 border-b border-white/10 pb-3">
                <dt className="text-white/55">Opens</dt>
                <dd className="text-right text-white/85">{dateTime(o.terms?.opensAt)}</dd>
              </div>
              <div className="flex items-baseline justify-between gap-4 border-b border-white/10 pb-3">
                <dt className="text-white/55">Closes</dt>
                <dd className="text-right text-white/85">{dateTime(o.terms?.closesAt)}</dd>
              </div>
              <div className="flex items-baseline justify-between gap-4 border-b border-white/10 pb-3">
                <dt className="text-white/55">Allotment due</dt>
                <dd className="text-right text-white/85">{dateTime(o.terms?.allotmentDueAt)}</dd>
              </div>
              <div className="flex items-baseline justify-between gap-4">
                <dt className="text-white/55">Min subscription</dt>
                <dd className="font-mono text-white">{int(o.terms?.minSubscriptionUnits)} units · {int(o.terms?.minDistinctHolders)} holders</dd>
              </div>
            </dl>
          </Card>
        </Fade>
        <Fade delay={0.14}>
          <Card>
            <div className="flex items-center gap-2.5">
              <Wallet className="h-5 w-5 text-brand-mist" aria-hidden="true" />
              <h3 className="font-sans text-lg font-semibold tracking-tight text-white">Subscription</h3>
            </div>
            {sub ? (
              <dl className="mt-5 space-y-3 text-sm">
                <div className="flex items-baseline justify-between gap-4 border-b border-white/10 pb-3">
                  <dt className="text-white/55">Bids</dt>
                  <dd className="font-mono text-white">{int(sub.bidCount)} from {int(sub.distinctBidders)} investors</dd>
                </div>
                <div className="flex items-baseline justify-between gap-4 border-b border-white/10 pb-3">
                  <dt className="text-white/55">Units bid</dt>
                  <dd className="font-mono text-white">{int(sub.unitsBid)}</dd>
                </div>
                <div className="flex items-baseline justify-between gap-4 border-b border-white/10 pb-3">
                  <dt className="text-white/55">Demand</dt>
                  <dd className="font-mono text-white">{demandRatio(sub.oversubscriptionNumerator, sub.oversubscriptionDenominator)}</dd>
                </div>
                <div className="flex items-baseline justify-between gap-4">
                  <dt className="text-white/55">Funds blocked</dt>
                  <dd className="font-mono text-white">{int(sub.fundsBlockedCount)}</dd>
                </div>
              </dl>
            ) : (
              <p className="mt-5 text-sm text-white/60">Bidding has not begun.</p>
            )}
            <hr className="my-5 border-white/10" />
            {acceptsBids ? (
              signedIn ? (
                <Link
                  to={`/offers/${o.id}/bid`}
                  className="group inline-flex items-center gap-2 rounded-full bg-white px-6 py-2.5 text-sm font-semibold text-black transition hover:scale-[1.03]"
                >
                  Place a bid
                  <ArrowRight className="h-4 w-4 transition-transform group-hover:translate-x-0.5" />
                </Link>
              ) : (
                <p className="text-sm text-white/65">
                  To bid you need an investor session.{' '}
                  <Link to="/login" className="text-brand-mist underline-offset-4 hover:underline">
                    Sign in →
                  </Link>
                </p>
              )
            ) : (
              <p className="text-sm text-white/65">
                Outcome:{' '}
                <Link
                  to={`/offers/${o.id}/allotments`}
                  className="text-brand-mist underline-offset-4 hover:underline"
                >
                  every bid's outcome, winners and losers →
                </Link>
              </p>
            )}
          </Card>
        </Fade>
      </div>

      <Fade className="mt-10">
        <h2 className="flex items-center gap-2.5 font-sans text-2xl font-semibold tracking-tight text-white sm:text-3xl">
          <ShieldCheck className="h-6 w-6 text-brand-mist" aria-hidden="true" />
          Verify
        </h2>
      </Fade>
      <Fade delay={0.05} className="mt-4">
        <Card flat>
          <div className="grid gap-3 sm:grid-cols-2">
            {[
              { to: `/offers/${o.id}/ballot`, icon: Gavel, title: 'Ballot ceremony', body: 'Root, seed, draw — each step on-chain.' },
              { to: `/offers/${o.id}/allotments`, icon: ScrollText, title: 'Allotments', body: 'Every bid’s outcome, winners and losers.' },
              { to: `/offers/${o.id}/documents`, icon: FileText, title: 'Offer documents', body: 'Digests anchored, bytes fetchable.' },
              { to: `/offers/${o.id}/proofs`, icon: Fingerprint, title: 'Bid proof checker', body: 'Check your bid against the root.' },
            ].map((v) => {
              const Icon = v.icon;
              return (
                <Link
                  key={v.to}
                  to={v.to}
                  className="group rounded-2xl border border-white/10 bg-white/[0.02] p-5 transition hover:border-white/25 hover:bg-white/[0.05]"
                >
                  <Icon className="h-5 w-5 text-brand-mist" aria-hidden="true" />
                  <p className="mt-3 font-semibold text-white">
                    {v.title}{' '}
                    <ArrowRight className="inline h-4 w-4 transition-transform group-hover:translate-x-0.5" />
                  </p>
                  <p className="mt-1 text-sm text-white/55">{v.body}</p>
                </Link>
              );
            })}
          </div>
          <p className="mt-5 text-sm text-white/60">
            Public. No sign-in. Anyone holding a bid reference can check it against the root on-chain.
          </p>
        </Card>
      </Fade>

      <Fade className="mt-10">
        <h2 className="font-sans text-2xl font-semibold tracking-tight text-white sm:text-3xl">Operators</h2>
      </Fade>
      <Fade delay={0.05} className="mt-4">
        <Card flat>
          <p className="text-sm text-white/65">
            <Link to={`/admin/${o.id}`} className="text-brand-mist underline-offset-4 hover:underline">
              Open in operator console →
            </Link>{' '}
            (MANAGER / TRUSTEE / COMPLIANCE persona)
          </p>
        </Card>
      </Fade>
    </>
  );
}
