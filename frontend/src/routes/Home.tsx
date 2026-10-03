import { Link } from 'react-router-dom';
import { usePoll } from '../api/usePoll';
import { pageItems } from '../api/client';
import type { Offer, Scheme } from '../api/types';
import { inr, inrShort, int, demandRatio } from '../lib/format';
import { Card, LoadingCard, ErrorBox, StatusPill } from '../components/ui';
import { ParallaxScene } from '../components/Parallax';
import { Io, CountUp, Marquee, Magnetic, SpotField, Kinetic } from '../components/motion';

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
    title: 'The chain attests, never custodies',
    body: 'Rupees move through regulated bank rails. There is deliberately no house balance anywhere in the system — modelling one would model a custody arrangement the platform does not hold.',
  },
  {
    title: 'Your money stays in your account',
    body: 'Blocking is ASBA-style: funds are reserved in the investor’s own bank account, debited only to the extent units are allotted. A block is all or nothing — never a partial reservation.',
  },
  {
    title: 'Losers are published too',
    body: 'A book of 490 bids for 475 units produces 490 published allocations. Publishing only winners would make the draw unfalsifiable for exactly the people with the strongest reason to check it.',
  },
  {
    title: 'One hash everywhere',
    body: 'SHA-256 across Merkle trees, document digests and seed commitments — so a third party reimplementing verification needs one primitive from their standard library.',
  },
  {
    title: 'The digest is the commitment',
    body: 'A content locator can stop resolving; a digest cannot. Verification is fetch the bytes, hash them, compare — the anchored value commits to the document itself.',
  },
  {
    title: 'No personal data on-chain',
    body: 'Investors appear only as HMAC anchors. Public endpoints carry leaf indices and anchors — never identities — so diligence never leaks privacy.',
  },
];

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
      <div className="live-card">
        <span className="live-tag">Market desk</span>
        <p className="live-price">Next offer<br />opens soon.</p>
        <p>New schemes appear here as they open — ask the team at the showcase desk for the timetable.</p>
      </div>
    );
  }
  if (offers.error && !offer) {
    return (
      <div className="live-card">
        <span className="live-tag">Market desk</span>
        <p className="live-price">Live market<br />unreachable.</p>
        <p><Link to={`/schemes/${scheme.id}`}>Open the scheme →</Link></p>
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
    <div className="live-card">
      <div className="live-top">
        <span className="live-tag"><span className="live-dot" aria-hidden="true" /> Live offer</span>
        <StatusPill kind="offer" status={offer.status} />
      </div>
      <p className="live-price">
        {inr(offer.terms?.priceBandLowerPaise)}–{inr(offer.terms?.priceBandUpperPaise)}
        <span> per unit</span>
      </p>
      <dl className="kv live-kv">
        <dt>Demand</dt>
        <dd className="num">{demandRatio(sub?.oversubscriptionNumerator, sub?.oversubscriptionDenominator)}</dd>
        <dt>Bids</dt>
        <dd className="num">{int(sub?.bidCount)} from {int(sub?.distinctBidders)} investors</dd>
        <dt>Units bid</dt>
        <dd className="num">{int(sub?.unitsBid)} of {int(offer.terms?.unitsOnOffer)}</dd>
      </dl>
      <div className="progress live-bar" role="progressbar" aria-valuenow={Math.round(bar * 100)} aria-valuemin={0} aria-valuemax={100} aria-label="Subscription demand">
        <i style={{ transform: `scaleX(${bar})` }} />
      </div>
      <p className="live-cta">
        <Link className="btn primary" to={`/offers/${offer.id}`}>Open the offer →</Link>{' '}
        <Link className="btn" to={`/offers/${offer.id}/ballot`}>Verify the ballot</Link>
      </p>
    </div>
  );
}

export default function Home() {
  const { data, loading, error, refresh } = usePoll<Scheme[] | { items: Scheme[] }>('/schemes', {});
  const schemes = pageItems(data);

  return (
    <>
      <ParallaxScene>
        <div className="px-layer px-grid" aria-hidden="true" data-speed="0.05" />
        <div className="px-content">
          <div className="hero hero-grid">
            <div>
              <h1>
                <Kinetic text="The chain attests." />{' '}
                <em>
                  <Kinetic text="It never custodies." delay={420} />
                </em>
              </h1>
              <Io delay={200}>
                <p className="lede">
                  AcreSync brings high-value real estate within reach through fractional ownership —
                  500 units at ₹10 lakh each instead of one ₹50 crore cheque. Rupees move through
                  regulated bank rails; what goes on-chain is a commitment to what happened, so you can
                  verify your own allotment and entitlement without being given access to anything internal.
                </p>
              </Io>
              <Io delay={320}>
                <div className="proof-row">
                  <a className="proof-chip" href="https://sepolia.etherscan.io/address/0xa656a42974b40cf64f32e758abb0689a2a178391" target="_blank" rel="noreferrer">
                    <span className="dot" aria-hidden="true" /> Live on Sepolia · source-verified
                  </a>
                  <span className="proof-chip">No proxy · no upgrade keys</span>
                  <span className="proof-chip">One hash · SHA-256 everywhere</span>
                </div>
              </Io>
              <Io delay={420}>
                <p style={{ marginTop: 22, marginBottom: 0 }}>
                  <Magnetic>
                    <Link className="btn primary" to="/login">Start investing →</Link>
                  </Magnetic>{' '}
                  <Magnetic>
                    <Link className="btn" to="/admin">See the operator console</Link>
                  </Magnetic>
                </p>
              </Io>
            </div>
            <Io delay={350} className="hero-panel">
              <LiveOfferPanel scheme={schemes[0] ?? null} loadingSchemes={loading} />
            </Io>
          </div>
        </div>
      </ParallaxScene>

      <div className="figures" aria-label="Reference scheme at a glance">
        <div className="figure"><b><CountUp end={50} prefix="₹" suffix=" Cr" /></b><span>Target corpus</span></div>
        <div className="figure"><b><CountUp end={500} /></b><span>Units · ₹10L each</span></div>
        <div className="figure"><b><CountUp end={200} suffix="+" /></b><span>Holders, on-chain floor</span></div>
        <div className="figure"><b><CountUp end={95} suffix="%" /></b><span>Of NDCF distributed</span></div>
      </div>

      <Marquee items={TICKER} />

      <div className="how">
        <div className="how-sticky">
          <Io>
            <h2>From first bid<br />to verified payout,<br />in four moves.</h2>
            <p>Scroll — each step is a state transition on the offer, enforced by the contracts, not by paperwork.</p>
            <p style={{ marginBottom: 0 }}>
              <Magnetic>
                <Link className="btn primary" to="/login">Try it live →</Link>
              </Magnetic>
            </p>
          </Io>
        </div>
        <ol className="how-steps">
          {STEPS.map((s, i) => (
            <Io as="li" key={s.n} className="how-step" delay={(i % 4) * 70}>
              <span className="how-n num" aria-hidden="true">{s.n}</span>
              <h3>{s.title}</h3>
              <p>{s.body}</p>
            </Io>
          ))}
        </ol>
      </div>

      <div className="sec-head">
        <h2>Why the evidence is the product</h2>
      </div>
      <Io>
        <p style={{ marginTop: 12 }}>
          Neighbouring fractional platforms custody funds and publish only winners. AcreSync is built
          the other way round: the ledger Entry you can check is the point, and everything else —
          money flow, identity, custody — stays exactly where regulation puts it.
        </p>
      </Io>
      <SpotField className="why-grid">
        {WHY.map((w, i) => (
          <Io key={w.title} delay={(i % 3) * 80}>
            <Card flat>
              <div className="spot-card why-card">
                <h3 style={{ marginTop: 0 }}>{w.title}</h3>
                <p style={{ marginBottom: 0 }}>{w.body}</p>
              </div>
            </Card>
          </Io>
        ))}
      </SpotField>

      <section className="band" aria-label="How distributions work">
        <Io>
          <h2>Don’t take our word for it</h2>
          <p>Every claim on this platform terminates at evidence you can check yourself — one hash function, standard library only.</p>
        </Io>
        <Io delay={100}>
          <ol>
            <li>Fetch the bytes, hash them with SHA-256, compare against the anchored digest.</li>
            <li>Rebuild the Merkle root from your bid’s proof path; check the root against the anchor transaction on Sepolia.</li>
            <li>Losers are published too — a draw you can’t falsify isn’t a draw.</li>
          </ol>
        </Io>
        <Io delay={160}>
          <div className="ndcf-line">
            <div><b>Gross rent</b><span>collected on the asset</span></div>
            <div className="ndcf-arrow" aria-hidden="true">→</div>
            <div><b>NDCF</b><span>after costs, fees &amp; tax</span></div>
            <div className="ndcf-arrow" aria-hidden="true">→</div>
            <div><b>≥ 95% paid out</b><span>enforced in Solidity, every period</span></div>
          </div>
        </Io>
        <Io delay={200}>
          <p style={{ marginBottom: 0 }}>
            <StatusPill kind="outbox" status="ANCHORED" label="Live on Sepolia" />{' '}
            Contracts are source-verified and immutable — no proxy, no upgrade keys. Anchored commitments are checkable by anyone, on-chain.
          </p>
        </Io>
      </section>

      <div className="sec-head">
        <h2>Schemes</h2>
        {schemes.length > 0 && <span className="sub">{schemes.length} live</span>}
      </div>
      {loading && <LoadingCard lines={3} label="Loading schemes" />}
      {error && <ErrorBox error={error} retry={refresh} />}
      {!loading && !error && schemes.length === 0 && (
        <div className="placeholder" style={{ marginTop: 16 }}>
          <h3>No schemes live right now</h3>
          <p>New schemes appear here as they open. Check back soon — or ask the team at the showcase desk.</p>
        </div>
      )}
      {schemes.length > 0 && (
        <div className="rows" style={{ marginTop: 4 }}>
          {schemes.map((s, i) => (
            <Io key={s.id} delay={Math.min(i, 5) * 60}>
              <div className="row">
                <div className="grow">
                  <h3><Link to={`/schemes/${s.id}`}>{String(s.name ?? s.id)}</Link></h3>
                  <p>
                    Target {inrShort(s.targetCorpusPaise)} · {s.totalUnits ?? 500} units
                    {s.environment && <> · <span className="mono">{String(s.environment)}</span></>}
                  </p>
                </div>
                <span className="meta"><Link to={`/schemes/${s.id}`}>Open scheme →</Link></span>
              </div>
            </Io>
          ))}
        </div>
      )}
    </>
  );
}
