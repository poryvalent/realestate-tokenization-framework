import { useEffect, useRef, useState, type ReactNode } from 'react';
import { Link } from 'react-router-dom';
import { animate, motion, useInView } from 'framer-motion';
import {
  ArrowRight,
  ArrowUpRight,
  BadgeCheck,
  Eye,
  FileCheck2,
  Landmark,
  Lock,
  Scale,
  ShieldCheck,
} from 'lucide-react';
import { usePoll } from '../api/usePoll';
import { pageItems } from '../api/client';
import type { Offer, Scheme } from '../api/types';
import { inr, inrShort, int, demandRatio } from '../lib/format';
import { Card, LoadingCard, ErrorBox, StatusPill } from '../components/ui';

const EASE: [number, number, number, number] = [0.25, 0.1, 0.25, 1];

const TICKER = [
  'Fractional ownership',
  'On-chain evidence',
  'Blocked in your account, never collected',
  'Commit–reveal ballot',
  'Every outcome published',
  '95% of NDCF distributed',
  'Verify, don’t trust',
];

const STEPS = [
  {
    n: '01',
    title: 'Browse the offer',
    body: 'Terms, price band and live subscription — units on offer, demand ratio, funds blocked. Know exactly what you are buying into before a single rupee moves.',
  },
  {
    n: '02',
    title: 'Block funds, place a bid',
    body: 'One bid per investor per offer. Money is blocked in your own bank account under ASBA — never collected, never held by the platform. It is debited only for the units you are allotted; the rest is released.',
  },
  {
    n: '03',
    title: 'Watch the ballot',
    body: 'The bid-book root is anchored before a seed exists, and the seed mixes a future block hash nobody can predict. After the draw, every bid gets a published outcome — winners and losers alike.',
  },
  {
    n: '04',
    title: 'Hold, earn, verify',
    body: 'Holdings, entitlements and NDCF payouts accrue to your account — and anyone holding a reference can recompute the proof against the chain with a single hash function.',
  },
];

const WHY = [
  {
    icon: ShieldCheck,
    title: 'The chain attests, never custodies',
    body: 'Rupees move through regulated bank rails. There is deliberately no house balance anywhere in the system — modelling one would model a custody arrangement the platform does not hold.',
  },
  {
    icon: Lock,
    title: 'Your money stays in your account',
    body: 'Blocking is ASBA-style: funds are reserved in the investor’s own bank account, debited only to the extent units are allotted. A block is all or nothing — never a partial reservation.',
  },
  {
    icon: Eye,
    title: 'Losers are published too',
    body: 'A book of 490 bids for 475 units produces 490 published allocations. Publishing only winners would make the draw unfalsifiable for exactly the people with the strongest reason to check it.',
  },
  {
    icon: FileCheck2,
    title: 'One hash everywhere',
    body: 'SHA-256 across Merkle trees, document digests and seed commitments — so a third party reimplementing verification needs one primitive from their standard library.',
  },
  {
    icon: BadgeCheck,
    title: 'The digest is the commitment',
    body: 'A content locator can stop resolving; a digest cannot. Verification is fetch the bytes, hash them, compare — the anchored value commits to the document itself.',
  },
  {
    icon: Scale,
    title: 'No personal data on-chain',
    body: 'Investors appear only as HMAC anchors. Public endpoints carry leaf indices and anchors — never identities — so diligence never leaks privacy.',
  },
];

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

function CountUp({ end, prefix = '', suffix = '' }: { end: number; prefix?: string; suffix?: string }) {
  const ref = useRef<HTMLSpanElement>(null);
  const inView = useInView(ref, { once: true, margin: '-40px' });
  const [val, setVal] = useState(0);
  useEffect(() => {
    if (!inView) return;
    const controls = animate(0, end, {
      duration: 1.6,
      ease: EASE,
      onUpdate: (v) => setVal(Math.round(v)),
    });
    return () => controls.stop();
  }, [inView, end]);
  return (
    <span ref={ref}>
      {prefix}
      {val.toLocaleString('en-IN')}
      {suffix}
    </span>
  );
}

/* Live market panel: the current offer's real state, at hero scale. */
function LiveOfferPanel({ scheme, loadingSchemes }: { scheme: Scheme | null; loadingSchemes: boolean }) {
  const offers = usePoll<Offer[] | { items: Offer[] }>(
    scheme ? `/schemes/${scheme.id}/offers` : null, {},
  );
  const first = pageItems(offers.data)[0] ?? null;
  const detail = usePoll<Offer>(first ? `/offers/${first.id}` : null, {});
  const offer = detail.data ?? first;

  if (loadingSchemes || offers.loading || detail.loading) {
    return <LoadingCard lines={5} label="Loading live offer" />;
  }
  if (!scheme || !offer) {
    return (
      <div className="liquid-glass rounded-3xl p-7 sm:p-8" role="status">
        <span className="inline-flex items-center gap-2 rounded-full border border-white/15 bg-white/5 px-3 py-1 text-xs font-medium uppercase tracking-[0.18em] text-white/60">
          Market desk
        </span>
        <p className="text-gradient mt-5 font-sans text-4xl font-semibold leading-[1.05] tracking-tight sm:text-5xl">
          Next offer
          <br />
          opens soon.
        </p>
        <p className="mt-4 text-sm leading-relaxed text-white/60">
          New schemes appear here as they open — ask the team at the showcase desk for the timetable.
        </p>
      </div>
    );
  }
  if (offers.error && !offer) {
    return (
      <div className="liquid-glass rounded-3xl p-7 sm:p-8" role="status">
        <span className="inline-flex items-center gap-2 rounded-full border border-white/15 bg-white/5 px-3 py-1 text-xs font-medium uppercase tracking-[0.18em] text-white/60">
          Market desk
        </span>
        <p className="text-gradient mt-5 font-sans text-4xl font-semibold leading-[1.05] tracking-tight sm:text-5xl">
          Live market
          <br />
          unreachable.
        </p>
        <p className="mt-4 text-sm text-white/60">
          <Link to={`/schemes/${scheme.id}`} className="text-brand-mist underline-offset-4 hover:underline">
            Open the scheme →
          </Link>
        </p>
      </div>
    );
  }

  const sub = offer.subscription;
  const ratio =
    sub?.oversubscriptionNumerator != null && sub?.oversubscriptionDenominator
      ? sub.oversubscriptionNumerator / sub.oversubscriptionDenominator
      : 0;
  const bar = Math.max(0, Math.min(1, ratio));

  return (
    <div className="liquid-glass relative overflow-hidden rounded-3xl p-7 sm:p-8">
      <div
        className="pointer-events-none absolute -top-24 right-0 h-56 w-56 rounded-full bg-brand-blue/25 blur-[90px]"
        aria-hidden="true"
      />
      <div className="relative flex items-center justify-between gap-3">
        <span className="inline-flex items-center gap-2 rounded-full border border-brand-mist/30 bg-brand-mist/10 px-3 py-1 text-xs font-medium uppercase tracking-[0.18em] text-brand-mist">
          <span className="relative flex h-1.5 w-1.5" aria-hidden="true">
            <span className="absolute inline-flex h-full w-full animate-ping rounded-full bg-brand-mist opacity-75" />
            <span className="relative inline-flex h-1.5 w-1.5 rounded-full bg-brand-mist" />
          </span>
          Live offer
        </span>
        <StatusPill kind="offer" status={offer.status} />
      </div>
      <p className="relative mt-5 font-sans text-4xl font-semibold tracking-tight text-white sm:text-5xl">
        {inr(offer.terms?.priceBandLowerPaise)}–{inr(offer.terms?.priceBandUpperPaise)}
        <span className="mt-1 block text-sm font-normal tracking-normal text-white/55">per unit</span>
      </p>
      <dl className="relative mt-6 space-y-3 text-sm">
        <div className="flex items-baseline justify-between gap-4 border-b border-white/10 pb-3">
          <dt className="text-white/55">Demand</dt>
          <dd className="font-mono text-white">{demandRatio(sub?.oversubscriptionNumerator, sub?.oversubscriptionDenominator)}</dd>
        </div>
        <div className="flex items-baseline justify-between gap-4 border-b border-white/10 pb-3">
          <dt className="text-white/55">Bids</dt>
          <dd className="font-mono text-white">{int(sub?.bidCount)} from {int(sub?.distinctBidders)} investors</dd>
        </div>
        <div className="flex items-baseline justify-between gap-4">
          <dt className="text-white/55">Units bid</dt>
          <dd className="font-mono text-white">{int(sub?.unitsBid)} of {int(offer.terms?.unitsOnOffer)}</dd>
        </div>
      </dl>
      <div
        className="relative mt-5 h-1.5 overflow-hidden rounded-full bg-white/10"
        role="progressbar"
        aria-valuenow={Math.round(bar * 100)}
        aria-valuemin={0}
        aria-valuemax={100}
        aria-label="Subscription demand"
      >
        <motion.i
          className="block h-full w-full origin-left rounded-full bg-gradient-to-r from-brand-blue to-brand-mist"
          initial={{ scaleX: 0 }}
          whileInView={{ scaleX: bar }}
          viewport={{ once: true }}
          transition={{ duration: 1, ease: EASE }}
        />
      </div>
      <p className="relative mt-6 flex flex-wrap gap-3">
        <Link
          to={`/offers/${offer.id}`}
          className="group inline-flex items-center gap-2 rounded-full bg-white px-5 py-2.5 text-sm font-semibold text-black transition hover:scale-[1.03]"
        >
          Open the offer
          <ArrowRight className="h-4 w-4 transition-transform group-hover:translate-x-0.5" />
        </Link>
        <Link
          to={`/offers/${offer.id}/ballot`}
          className="inline-flex items-center gap-2 rounded-full border border-white/20 px-5 py-2.5 text-sm text-white transition hover:bg-white/10"
        >
          Verify the ballot
        </Link>
      </p>
    </div>
  );
}

export default function Home() {
  const { data, loading, error, refresh } = usePoll<Scheme[] | { items: Scheme[] }>('/schemes', {});
  const schemes = pageItems(data);

  return (
    <>
      {/* ── Hero ─────────────────────────────────────────── */}
      <section className="relative" aria-label="Introduction">
        <div className="grid items-center gap-10 lg:grid-cols-[1.08fr_0.92fr]">
          <div>
            <motion.div
              initial={{ opacity: 0, y: 16, filter: 'blur(4px)' }}
              animate={{ opacity: 1, y: 0, filter: 'blur(0px)' }}
              transition={{ duration: 0.55, ease: EASE }}
            >
              <span className="inline-flex items-center gap-2 rounded-full border border-white/15 bg-white/5 px-3.5 py-1.5 text-xs text-white/70">
                <Landmark className="h-3.5 w-3.5 text-brand-mist" />
                Fractional real estate · on-chain evidence
              </span>
            </motion.div>
            <motion.h1
              initial={{ opacity: 0, y: 32, filter: 'blur(8px)' }}
              animate={{ opacity: 1, y: 0, filter: 'blur(0px)' }}
              transition={{ duration: 0.7, delay: 0.08, ease: EASE }}
              className="mt-6 font-sans text-5xl font-semibold leading-[1.02] tracking-tight sm:text-6xl lg:text-7xl"
            >
              <span className="text-gradient block">The chain attests.</span>
              <motion.em
                className="block text-white"
                initial={{ opacity: 0, y: 24, filter: 'blur(8px)' }}
                animate={{ opacity: 1, y: 0, filter: 'blur(0px)' }}
                transition={{ duration: 0.7, delay: 0.5, ease: EASE }}
              >
                It never custodies.
              </motion.em>
            </motion.h1>
            <motion.p
              initial={{ opacity: 0, y: 20 }}
              animate={{ opacity: 1, y: 0 }}
              transition={{ duration: 0.6, delay: 0.28, ease: EASE }}
              className="mt-6 max-w-xl text-base leading-relaxed text-white/65 sm:text-lg"
            >
              AcreSync brings high-value real estate within reach through fractional ownership —
              500 units at ₹10 lakh each instead of one ₹50 crore cheque. Rupees move through
              regulated bank rails; what goes on-chain is a commitment to what happened, so you can
              verify your own allotment and entitlement without being given access to anything internal.
            </motion.p>
            <motion.div
              initial={{ opacity: 0, y: 16 }}
              animate={{ opacity: 1, y: 0 }}
              transition={{ duration: 0.55, delay: 0.4, ease: EASE }}
              className="mt-6 flex flex-wrap gap-2.5"
            >
              <a
                className="inline-flex items-center gap-2 rounded-full border border-white/12 bg-white/[0.04] px-3.5 py-1.5 text-xs text-white/65 transition hover:bg-white/[0.08]"
                href="https://sepolia.etherscan.io/address/0xa656a42974b40cf64f32e758abb0689a2a178391"
                target="_blank"
                rel="noreferrer"
              >
                <span className="h-1.5 w-1.5 rounded-full bg-brand-mist" aria-hidden="true" />
                Live on Sepolia · source-verified
                <ArrowUpRight className="h-3.5 w-3.5" />
              </a>
              <span className="inline-flex items-center rounded-full border border-white/12 bg-white/[0.04] px-3.5 py-1.5 text-xs text-white/65">
                No proxy · no upgrade keys
              </span>
              <span className="inline-flex items-center rounded-full border border-white/12 bg-white/[0.04] px-3.5 py-1.5 text-xs text-white/65">
                One hash · SHA-256 everywhere
              </span>
            </motion.div>
            <motion.p
              initial={{ opacity: 0, y: 16 }}
              animate={{ opacity: 1, y: 0 }}
              transition={{ duration: 0.55, delay: 0.5, ease: EASE }}
              className="mt-8 flex flex-wrap gap-3"
            >
              <Link
                to="/login"
                className="group inline-flex items-center gap-2 rounded-full bg-white px-6 py-3 text-sm font-semibold text-black transition hover:scale-[1.03]"
              >
                Start investing
                <ArrowRight className="h-4 w-4 transition-transform group-hover:translate-x-0.5" />
              </Link>
              <Link
                to="/admin"
                className="inline-flex items-center gap-2 rounded-full border border-white/20 px-6 py-3 text-sm text-white transition hover:bg-white/10"
              >
                See the operator console
              </Link>
            </motion.p>
          </div>
          <motion.div
            initial={{ opacity: 0, y: 32, filter: 'blur(6px)' }}
            animate={{ opacity: 1, y: 0, filter: 'blur(0px)' }}
            transition={{ duration: 0.7, delay: 0.42, ease: EASE }}
          >
            <LiveOfferPanel scheme={schemes[0] ?? null} loadingSchemes={loading} />
          </motion.div>
        </div>
      </section>

      {/* ── Figures ──────────────────────────────────────── */}
      <Fade className="mt-16 sm:mt-20" aria-label="Reference scheme at a glance">
        <dl className="liquid-glass grid grid-cols-2 gap-px overflow-hidden rounded-3xl lg:grid-cols-4">
          {[
            { el: <CountUp end={50} prefix="₹" suffix=" Cr" />, label: 'Target corpus' },
            { el: <CountUp end={500} />, label: 'Units · ₹10L each' },
            { el: <CountUp end={200} suffix="+" />, label: 'Holders, on-chain floor' },
            { el: <CountUp end={95} suffix="%" />, label: 'Of NDCF distributed' },
          ].map((f) => (
            <div key={f.label} className="bg-white/[0.015] px-6 py-7 text-center">
              <dd className="font-sans text-3xl font-semibold tracking-tight text-white sm:text-4xl">{f.el}</dd>
              <dt className="mt-2 text-xs uppercase tracking-[0.16em] text-white/50">{f.label}</dt>
            </div>
          ))}
        </dl>
      </Fade>

      {/* ── Ticker ───────────────────────────────────────── */}
      <Fade className="mt-12" delay={0.05}>
        <div className="relative overflow-hidden border-y border-white/10 py-4" aria-hidden="true">
          <div className="pointer-events-none absolute inset-y-0 left-0 z-10 w-24 bg-gradient-to-r from-brand-dark to-transparent" />
          <div className="pointer-events-none absolute inset-y-0 right-0 z-10 w-24 bg-gradient-to-l from-brand-dark to-transparent" />
          <motion.div
            className="flex w-max items-center gap-10"
            animate={{ x: ['0%', '-50%'] }}
            transition={{ duration: 42, repeat: Infinity, ease: 'linear' }}
          >
            {[...TICKER, ...TICKER].map((t, i) => (
              <span key={i} className="flex items-center gap-10 whitespace-nowrap text-sm uppercase tracking-[0.2em] text-white/40">
                {t}
                <span className="h-1 w-1 rounded-full bg-brand-blue/60" />
              </span>
            ))}
          </motion.div>
        </div>
      </Fade>

      {/* ── How it works ─────────────────────────────────── */}
      <div className="mt-20 grid gap-10 sm:mt-24 lg:grid-cols-[0.9fr_1.1fr]">
        <div className="lg:sticky lg:top-28 lg:self-start">
          <Fade>
            <p className="text-xs font-medium uppercase tracking-[0.22em] text-brand-mist">How it works</p>
            <h2 className="text-gradient mt-3 font-sans text-4xl font-semibold leading-[1.05] tracking-tight sm:text-5xl">
              From first bid
              <br />
              to verified payout,
              <br />
              in four moves.
            </h2>
            <p className="mt-4 max-w-md text-sm leading-relaxed text-white/60 sm:text-base">
              Scroll — each step is a state transition on the offer, enforced by the contracts, not by paperwork.
            </p>
            <p className="mt-6">
              <Link
                to="/login"
                className="group inline-flex items-center gap-2 rounded-full bg-white px-6 py-3 text-sm font-semibold text-black transition hover:scale-[1.03]"
              >
                Try it live
                <ArrowRight className="h-4 w-4 transition-transform group-hover:translate-x-0.5" />
              </Link>
            </p>
          </Fade>
        </div>
        <ol className="space-y-4">
          {STEPS.map((s, i) => (
            <Fade key={s.n} delay={(i % 4) * 0.07}>
              <li className="liquid-glass group rounded-3xl p-6 transition-colors hover:border-white/20 sm:p-7">
                <span className="font-mono text-sm text-brand-mist" aria-hidden="true">
                  {s.n}
                </span>
                <h3 className="mt-2 font-sans text-xl font-semibold tracking-tight text-white">{s.title}</h3>
                <p className="mt-2 text-sm leading-relaxed text-white/60">{s.body}</p>
              </li>
            </Fade>
          ))}
        </ol>
      </div>

      {/* ── Why ──────────────────────────────────────────── */}
      <div className="mt-20 sm:mt-24">
        <Fade>
          <p className="text-xs font-medium uppercase tracking-[0.22em] text-brand-mist">Why AcreSync</p>
          <h2 className="text-gradient mt-3 font-sans text-4xl font-semibold tracking-tight sm:text-5xl">
            Why the evidence is the product
          </h2>
          <p className="mt-4 max-w-2xl text-sm leading-relaxed text-white/60 sm:text-base">
            Neighbouring fractional platforms custody funds and publish only winners. AcreSync is built
            the other way round: the ledger entry you can check is the point, and everything else —
            money flow, identity, custody — stays exactly where regulation puts it.
          </p>
        </Fade>
        <div className="mt-8 grid gap-4 sm:grid-cols-2 lg:grid-cols-3">
          {WHY.map((w, i) => {
            const Icon = w.icon;
            return (
              <Fade key={w.title} delay={(i % 3) * 0.08}>
                <Card flat>
                  <Icon className="h-5 w-5 text-brand-mist" aria-hidden="true" />
                  <h3 className="mt-4 font-sans text-lg font-semibold tracking-tight text-white">{w.title}</h3>
                  <p className="mt-2 text-sm leading-relaxed text-white/60">{w.body}</p>
                </Card>
              </Fade>
            );
          })}
        </div>
      </div>

      {/* ── Verify band ──────────────────────────────────── */}
      <Fade className="mt-20 sm:mt-24">
        <section
          className="liquid-glass relative overflow-hidden rounded-3xl p-8 sm:p-12"
          aria-label="How distributions work"
        >
          <div
            className="pointer-events-none absolute -left-24 top-0 h-64 w-64 rounded-full bg-brand-blue/20 blur-[100px]"
            aria-hidden="true"
          />
          <div className="relative">
            <p className="text-xs font-medium uppercase tracking-[0.22em] text-brand-mist">Distributions</p>
            <h2 className="text-gradient mt-3 font-sans text-3xl font-semibold tracking-tight sm:text-4xl">
              Don’t take our word for it
            </h2>
            <p className="mt-3 max-w-2xl text-sm leading-relaxed text-white/60 sm:text-base">
              Every claim on this platform terminates at evidence you can check yourself — one hash function, standard library only.
            </p>
            <ol className="mt-6 space-y-3 text-sm leading-relaxed text-white/75">
              <li className="flex gap-3">
                <span className="font-mono text-brand-mist">01</span>
                Fetch the bytes, hash them with SHA-256, compare against the anchored digest.
              </li>
              <li className="flex gap-3">
                <span className="font-mono text-brand-mist">02</span>
                Rebuild the Merkle root from your bid’s proof path; check the root against the anchor transaction on Sepolia.
              </li>
              <li className="flex gap-3">
                <span className="font-mono text-brand-mist">03</span>
                Losers are published too — a draw you can’t falsify isn’t a draw.
              </li>
            </ol>
            <div className="mt-8 flex flex-col items-stretch gap-3 text-center sm:flex-row sm:items-center">
              <div className="flex-1 rounded-2xl border border-white/10 bg-white/[0.03] px-4 py-4">
                <p className="font-semibold text-white">Gross rent</p>
                <p className="mt-1 text-xs text-white/55">collected on the asset</p>
              </div>
              <div className="font-mono text-brand-mist" aria-hidden="true">
                →
              </div>
              <div className="flex-1 rounded-2xl border border-white/10 bg-white/[0.03] px-4 py-4">
                <p className="font-semibold text-white">NDCF</p>
                <p className="mt-1 text-xs text-white/55">after costs, fees &amp; tax</p>
              </div>
              <div className="font-mono text-brand-mist" aria-hidden="true">
                →
              </div>
              <div className="flex-1 rounded-2xl border border-white/10 bg-white/[0.03] px-4 py-4">
                <p className="font-semibold text-white">≥ 95% paid out</p>
                <p className="mt-1 text-xs text-white/55">enforced in Solidity, every period</p>
              </div>
            </div>
            <p className="mt-6 text-sm leading-relaxed text-white/65">
              <StatusPill kind="outbox" status="ANCHORED" label="Live on Sepolia" />{' '}
              <span className="ml-1">
                Contracts are source-verified and immutable — no proxy, no upgrade keys. Anchored commitments are checkable by anyone, on-chain.
              </span>
            </p>
          </div>
        </section>
      </Fade>

      {/* ── Schemes ──────────────────────────────────────── */}
      <div className="mt-20 sm:mt-24">
        <Fade>
          <div className="flex items-baseline gap-3">
            <h2 className="text-gradient font-sans text-4xl font-semibold tracking-tight sm:text-5xl">Schemes</h2>
            {schemes.length > 0 && (
              <span className="rounded-full border border-white/15 bg-white/5 px-3 py-1 text-xs text-white/60">
                {schemes.length} live
              </span>
            )}
          </div>
        </Fade>
        <div className="mt-6 space-y-4">
          {loading && <LoadingCard lines={3} label="Loading schemes" />}
          {error && <ErrorBox error={error} retry={refresh} />}
          {!loading && !error && schemes.length === 0 && (
            <Fade>
              <div className="rounded-3xl border border-dashed border-white/20 bg-white/[0.02] p-10 text-center">
                <h3 className="font-sans text-xl font-semibold text-white">No schemes live right now</h3>
                <p className="mx-auto mt-2 max-w-md text-sm text-white/60">
                  New schemes appear here as they open. Check back soon — or ask the team at the showcase desk.
                </p>
              </div>
            </Fade>
          )}
          {schemes.map((s, i) => (
            <Fade key={s.id} delay={Math.min(i, 5) * 0.06}>
              <Link
                to={`/schemes/${s.id}`}
                className="liquid-glass group flex items-center gap-5 rounded-3xl p-6 transition-colors hover:border-white/20 sm:px-7"
              >
                <div className="min-w-0 grow">
                  <h3 className="truncate font-sans text-xl font-semibold tracking-tight text-white group-hover:underline group-hover:underline-offset-4">
                    {String(s.name ?? s.id)}
                  </h3>
                  <p className="mt-1.5 text-sm text-white/60">
                    Target {inrShort(s.targetCorpusPaise)} · {s.totalUnits ?? 500} units
                    {s.environment && (
                      <>
                        {' '}· <span className="font-mono text-xs text-white/55">{String(s.environment)}</span>
                      </>
                    )}
                  </p>
                </div>
                <span className="hidden shrink-0 items-center gap-1.5 rounded-full border border-white/15 px-4 py-2 text-sm text-white/75 transition group-hover:border-white/30 group-hover:text-white sm:inline-flex">
                  Open scheme
                  <ArrowRight className="h-4 w-4 transition-transform group-hover:translate-x-0.5" />
                </span>
              </Link>
            </Fade>
          ))}
        </div>
      </div>
    </>
  );
}
