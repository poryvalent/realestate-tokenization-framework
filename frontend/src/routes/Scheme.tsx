import { Fragment, type ReactNode } from 'react';
import { Link, useParams } from 'react-router-dom';
import { motion } from 'framer-motion';
import { ArrowLeft, ArrowRight, CalendarDays, Layers } from 'lucide-react';
import { usePoll } from '../api/usePoll';
import { pageItems } from '../api/client';
import type { Offer, Period, Scheme } from '../api/types';
import { inrShort, int, calDate } from '../lib/format';
import { Card, LoadingCard, ErrorBox, StatusPill, Empty } from '../components/ui';

const EASE: [number, number, number, number] = [0.25, 0.1, 0.25, 1];

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

export default function Scheme() {
  const { id } = useParams();
  const scheme = usePoll<Scheme>(id ? `/schemes/${id}` : null, {});
  const offers = usePoll<Offer[] | { items: Offer[] }>(id ? `/schemes/${id}/offers` : null, {});
  const periods = usePoll<Period[] | { items: Period[] }>(id ? `/schemes/${id}/periods` : null, {});
  const offerList = pageItems(offers.data);
  const periodList = pageItems(periods.data);

  if (scheme.loading) return <LoadingCard label="Loading scheme" />;
  if (scheme.error) return <ErrorBox error={scheme.error} retry={scheme.refresh} />;
  const s = scheme.data;
  if (!s) return <Empty title="Scheme not found" />;

  return (
    <>
      <motion.p
        initial={{ opacity: 0, y: 12 }}
        animate={{ opacity: 1, y: 0 }}
        transition={{ duration: 0.4, ease: EASE }}
        className="mt-6"
      >
        <Link
          to="/"
          className="inline-flex items-center gap-1.5 text-sm text-white/60 transition hover:text-white"
        >
          <ArrowLeft className="h-4 w-4" />
          Schemes
        </Link>
      </motion.p>

      <motion.div
        initial={{ opacity: 0, y: 28, filter: 'blur(8px)' }}
        animate={{ opacity: 1, y: 0, filter: 'blur(0px)' }}
        transition={{ duration: 0.65, ease: EASE }}
      >
        <p className="mt-4 text-xs font-medium uppercase tracking-[0.22em] text-brand-mist">
          Scheme · <span className="font-mono normal-case tracking-normal">{String(s.id)}</span>
        </p>
        <h1 className="text-gradient mt-2 font-sans text-4xl font-semibold leading-[1.05] tracking-tight sm:text-5xl lg:text-6xl">
          {String(s.name ?? s.id)}
        </h1>
      </motion.div>

      <Fade delay={0.1} className="mt-8">
        <Card>
          <dl className="grid gap-x-8 gap-y-4 text-sm sm:grid-cols-2">
            <div className="flex items-baseline justify-between gap-4 border-b border-white/10 pb-3">
              <dt className="text-white/55">Target corpus</dt>
              <dd className="font-mono text-white">{inrShort(s.targetCorpusPaise)}</dd>
            </div>
            <div className="flex items-baseline justify-between gap-4 border-b border-white/10 pb-3">
              <dt className="text-white/55">Units</dt>
              <dd className="font-mono text-white">{int(s.totalUnits)} total · {int(s.managerUnits)} manager</dd>
            </div>
            <div className="flex items-baseline justify-between gap-4 border-b border-white/10 pb-3">
              <dt className="text-white/55">Manager</dt>
              <dd className="text-right text-white">{String(s.investmentManager ?? '—')}</dd>
            </div>
            <div className="flex items-baseline justify-between gap-4 border-b border-white/10 pb-3">
              <dt className="text-white/55">Trustee</dt>
              <dd className="text-right text-white">{String(s.trustee ?? '—')}</dd>
            </div>
          </dl>
          {s.contracts && (
            <dl className="mt-4 space-y-2.5 border-t border-white/10 pt-4 text-sm">
              {Object.entries(s.contracts).map(([k, v]) => (
                <Fragment key={k}>
                  <div className="flex flex-col gap-1 sm:flex-row sm:items-baseline sm:justify-between sm:gap-4">
                    <dt className="font-mono text-xs text-white/55">{k}</dt>
                    <dd className="min-w-0">
                      <a
                        className="block truncate font-mono text-[13px] text-brand-mist underline-offset-4 hover:underline sm:max-w-md sm:text-right"
                        href={`https://sepolia.etherscan.io/address/${String(v).toLowerCase()}`}
                        target="_blank"
                        rel="noreferrer"
                        title={String(v)}
                      >
                        {String(v)}
                      </a>
                    </dd>
                  </div>
                </Fragment>
              ))}
            </dl>
          )}
        </Card>
      </Fade>

      <div className="mt-12">
        <Fade>
          <div className="flex items-center gap-3">
            <Layers className="h-5 w-5 text-brand-mist" aria-hidden="true" />
            <h2 className="font-sans text-2xl font-semibold tracking-tight text-white sm:text-3xl">Offers</h2>
            {offerList.length > 0 && (
              <span className="rounded-full border border-white/15 bg-white/5 px-3 py-1 text-xs text-white/60">
                {offerList.length}
              </span>
            )}
          </div>
        </Fade>
        <div className="mt-5 space-y-4">
          {offers.loading && <LoadingCard lines={2} label="Loading offers" />}
          {offers.error && <ErrorBox error={offers.error} retry={offers.refresh} />}
          {!offers.loading && !offers.error && offerList.length === 0 && (
            <Empty title="No offers" body="This scheme has no offers yet." />
          )}
          {offerList.map((o, i) => (
            <Fade key={o.id} delay={Math.min(i, 5) * 0.06}>
              <Link
                to={`/offers/${o.id}`}
                className="liquid-glass group block rounded-3xl p-6 transition-colors hover:border-white/20 sm:p-7"
              >
                <div className="flex flex-wrap items-center justify-between gap-3">
                  <h3 className="font-sans text-xl font-semibold tracking-tight text-white group-hover:underline group-hover:underline-offset-4">
                    {o.offerType ?? 'Offer'} · {inrShort(o.terms?.priceBandLowerPaise)}–{inrShort(o.terms?.priceBandUpperPaise)} per unit
                  </h3>
                  <StatusPill kind="offer" status={o.status} />
                </div>
                <p className="mt-3 flex flex-wrap items-center gap-x-2 gap-y-1 font-mono text-sm text-white/60">
                  <span>{int(o.terms?.unitsOnOffer)} units</span>
                  <span aria-hidden="true" className="text-white/25">·</span>
                  <span>{int(o.subscription?.bidCount)} bids</span>
                  {o.subscription?.oversubscriptionNumerator != null && (
                    <>
                      <span aria-hidden="true" className="text-white/25">·</span>
                      <span>
                        subscribed {o.subscription.oversubscriptionNumerator}/{o.subscription.oversubscriptionDenominator ?? '—'}
                      </span>
                    </>
                  )}
                  <span className="inline-flex items-center gap-1 font-sans text-brand-mist">
                    Details
                    <ArrowRight className="h-3.5 w-3.5 transition-transform group-hover:translate-x-0.5" />
                  </span>
                </p>
              </Link>
            </Fade>
          ))}
        </div>
      </div>

      <div className="mt-12">
        <Fade>
          <div className="flex items-center gap-3">
            <CalendarDays className="h-5 w-5 text-brand-mist" aria-hidden="true" />
            <h2 className="font-sans text-2xl font-semibold tracking-tight text-white sm:text-3xl">
              Distribution periods
            </h2>
            {periodList.length > 0 && (
              <span className="rounded-full border border-white/15 bg-white/5 px-3 py-1 text-xs text-white/60">
                {periodList.length}
              </span>
            )}
          </div>
        </Fade>
        <div className="mt-5 space-y-4">
          {periods.loading && <LoadingCard lines={2} label="Loading periods" />}
          {periods.error && <ErrorBox error={periods.error} retry={periods.refresh} />}
          {!periods.loading && !periods.error && periodList.length === 0 && (
            <Empty title="No periods" body="No distribution periods yet." />
          )}
          {periodList.map((p, i) => (
            <Fade key={p.id} delay={Math.min(i, 5) * 0.06}>
              <Link
                to={`/periods/${p.id}`}
                className="liquid-glass group flex flex-wrap items-center justify-between gap-3 rounded-3xl p-6 transition-colors hover:border-white/20"
              >
                <h3 className="font-sans text-lg font-semibold tracking-tight text-white group-hover:underline group-hover:underline-offset-4">
                  Period {String(p.id).slice(0, 8)}
                </h3>
                <p className="flex flex-wrap items-center gap-3 text-sm text-white/60">
                  <StatusPill kind="period" status={String(p.status ?? 'UNKNOWN')} />
                  <span>
                    Record date <span className="font-mono text-white/80">{calDate(p.recordDate)}</span>
                  </span>
                </p>
              </Link>
            </Fade>
          ))}
        </div>
      </div>
    </>
  );
}
