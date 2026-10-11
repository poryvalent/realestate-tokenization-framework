import { useRef, useState, type ReactNode } from 'react';
import { Link } from 'react-router-dom';
import { motion, useScroll, useTransform } from 'framer-motion';
import {
  ArrowRight,
  Coins,
  Landmark,
  Lock,
  ShieldCheck,
  ExternalLink,
} from 'lucide-react';
import { usePoll } from '../api/usePoll';
import { pageItems } from '../api/client';
import type { Scheme } from '../api/types';
import { inr } from '../lib/format';
import { LoadingCard } from '../components/ui';

const EASE: [number, number, number, number] = [0.25, 0.1, 0.25, 1];

// High-resolution architectural photography assets
const ASSETS = {
  heroSkyline: 'https://images.unsplash.com/photo-1486406146926-c627a92ad1ab?auto=format&fit=crop&w=2400&q=85',
  glassFacade: 'https://images.unsplash.com/photo-1541888946425-d0fbb1861593?auto=format&fit=crop&w=2000&q=80',
  executiveAtrium: 'https://images.unsplash.com/photo-1497366216548-37526070297c?auto=format&fit=crop&w=1800&q=80',
  campusPark: 'https://images.unsplash.com/photo-1497215728101-856f4ea42174?auto=format&fit=crop&w=1800&q=80',
  nightTower: 'https://images.unsplash.com/photo-1507679799987-c73779587ccf?auto=format&fit=crop&w=1800&q=80',
  urbanLobby: 'https://images.unsplash.com/photo-1497366754035-f200968a6e72?auto=format&fit=crop&w=1800&q=80',
  modernTerrace: 'https://images.unsplash.com/photo-1577495508048-b635879837f1?auto=format&fit=crop&w=1800&q=80',
  outroCity: 'https://images.unsplash.com/photo-1444723121867-7a241cacace9?auto=format&fit=crop&w=2400&q=85',
};

function FadeIn({
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
      initial={{ opacity: 0, y: 32 }}
      whileInView={{ opacity: 1, y: 0 }}
      viewport={{ once: true, margin: '-50px' }}
      transition={{ duration: 0.7, delay, ease: EASE }}
    >
      {children}
    </motion.div>
  );
}

export default function Home() {
  const schemesReq = usePoll<Scheme[] | { items: Scheme[] }>('/schemes', {});
  const schemes = pageItems(schemesReq.data);

  const [newsletterEmail, setNewsletterEmail] = useState('');
  const [subscribed, setSubscribed] = useState(false);

  // Parallax scroll controls for Hero
  const heroRef = useRef<HTMLDivElement>(null);
  const { scrollYProgress } = useScroll({
    target: heroRef,
    offset: ['start start', 'end start'],
  });

  const heroLogoY = useTransform(scrollYProgress, [0, 1], ['0%', '45%']);
  const heroLogoOpacity = useTransform(scrollYProgress, [0, 0.65], [1, 0]);
  const heroScale = useTransform(scrollYProgress, [0, 1], [1, 1.08]);

  const handleNewsletterSubmit = (e: React.FormEvent) => {
    e.preventDefault();
    if (newsletterEmail.trim()) {
      setSubscribed(true);
      setNewsletterEmail('');
    }
  };

  return (
    <div className="w-full bg-[#030712] text-white overflow-hidden">
      {/* ========================================================================= */}
      {/* SECTION 1: HERO (findrealestate.com architecture with giant ACRESYNC mark) */}
      {/* ========================================================================= */}
      <section ref={heroRef} className="relative min-h-[92vh] lg:min-h-screen w-full flex flex-col justify-between overflow-hidden pt-20">
        {/* Layer 1: Photographic Skyline Backdrop */}
        <motion.div
          style={{ scale: heroScale }}
          className="pointer-events-none absolute inset-0 z-0 bg-cover bg-center"
        >
          <img
            src={ASSETS.heroSkyline}
            alt="Institutional Grade Real Estate"
            className="h-full w-full object-cover object-center filter brightness-[0.45] contrast-[1.12]"
          />
          <div className="absolute inset-0 bg-gradient-to-b from-[#030712]/70 via-[#050c1f]/50 to-[#030712]" />
        </motion.div>

        {/* Layer 2: Giant Architectural Brand Typography (ACRESYNC) */}
        <motion.div
          style={{ y: heroLogoY, opacity: heroLogoOpacity }}
          className="pointer-events-none absolute inset-0 z-10 flex items-center justify-center select-none"
        >
          <div className="w-full px-4 text-center">
            <span className="block font-sans text-[16vw] font-black uppercase tracking-[-0.04em] text-white/[0.07] leading-none">
              ACRESYNC
            </span>
          </div>
        </motion.div>

        {/* Layer 3: Atmospheric Mist & Light Overlay */}
        <div className="pointer-events-none absolute inset-0 z-20 bg-[radial-gradient(ellipse_at_center,transparent_20%,#030712_90%)]" />

        {/* Layer 4: Hero Content Container */}
        <div className="relative z-30 mx-auto w-full max-w-7xl px-6 pt-16 sm:px-10 lg:pt-28">
          <div className="max-w-3xl">
            {/* Regulatory Badge */}
            <motion.div
              initial={{ opacity: 0, y: 20 }}
              animate={{ opacity: 1, y: 0 }}
              transition={{ duration: 0.6, ease: EASE }}
              className="inline-flex items-center gap-2 rounded-full border border-blue-400/30 bg-[#07173b]/80 px-4 py-1.5 text-xs font-mono uppercase tracking-[0.2em] text-blue-200 backdrop-blur-md"
            >
              <span className="h-2 w-2 rounded-full bg-emerald-400 animate-pulse" />
              <span>SEBI SM-REIT Framework · Zero Custody ASBA</span>
            </motion.div>

            {/* Giant Title */}
            <motion.h1
              initial={{ opacity: 0, y: 28 }}
              animate={{ opacity: 1, y: 0 }}
              transition={{ duration: 0.8, delay: 0.1, ease: EASE }}
              className="mt-6 font-sans text-4xl sm:text-6xl md:text-7xl font-bold tracking-tight text-white leading-[1.04]"
            >
              Invest In What <br />
              <span className="em">Moves You.</span>
            </motion.h1>

            {/* Subheading */}
            <motion.p
              initial={{ opacity: 0, y: 24 }}
              animate={{ opacity: 1, y: 0 }}
              transition={{ duration: 0.8, delay: 0.2, ease: EASE }}
              className="mt-6 max-w-2xl text-base sm:text-lg md:text-xl text-slate-300 font-normal leading-relaxed"
            >
              Institutional Grade-A Real Estate. Bank-Blocked ASBA Escrow.{' '}
              <span className="em">A clear, sovereign path to fractional unitholding.</span>
            </motion.p>

            {/* Action Buttons */}
            <motion.div
              initial={{ opacity: 0, y: 20 }}
              animate={{ opacity: 1, y: 0 }}
              transition={{ duration: 0.8, delay: 0.3, ease: EASE }}
              className="mt-8 flex flex-wrap items-center gap-4"
            >
              <a
                href="#live-schemes"
                className="button-round bg-white text-[#040916] px-7 py-4 text-xs font-bold uppercase tracking-wider shadow-[0_10px_30px_rgba(255,255,255,0.2)] hover:bg-slate-100 transition-all cursor-pointer"
              >
                <span>Find Live Schemes</span>
                <span className="flex h-6 w-6 items-center justify-center rounded-full bg-[#040916] text-white">
                  <ArrowRight className="h-3.5 w-3.5" />
                </span>
              </a>

              <Link
                to="/offers/4d7c3397-023e-4e1d-a328-a2af07fca928/documents"
                className="button-round border border-white/20 bg-white/5 px-6 py-4 text-xs font-bold uppercase tracking-wider text-white backdrop-blur-md hover:bg-white/10 hover:border-white transition-all cursor-pointer"
              >
                <span>Due Diligence Docs</span>
                <ExternalLink className="h-3.5 w-3.5 text-blue-300" />
              </Link>
            </motion.div>
          </div>
        </div>

        {/* Hero Bottom Telemetry Bar */}
        <div className="relative z-30 w-full border-t border-white/10 bg-[#050c1f]/85 backdrop-blur-2xl py-6 mt-16">
          <div className="mx-auto w-full max-w-7xl px-6 sm:px-10">
            <div className="grid grid-cols-2 md:grid-cols-4 gap-6">
              <div>
                <p className="font-mono text-[10px] uppercase tracking-[0.2em] text-slate-400">Live Active Asset</p>
                <p className="mt-1 font-sans text-sm sm:text-base font-bold text-white truncate">Kalyani TechPark</p>
                <p className="text-[11px] text-blue-300 font-mono">Whitefield, Bengaluru</p>
              </div>
              <div>
                <p className="font-mono text-[10px] uppercase tracking-[0.2em] text-slate-400">Scheme Valuation</p>
                <p className="mt-1 font-sans text-sm sm:text-base font-bold text-white">₹520,000,000</p>
                <p className="text-[11px] text-emerald-400 font-mono">SEBI Regulated SPV</p>
              </div>
              <div>
                <p className="font-mono text-[10px] uppercase tracking-[0.2em] text-slate-400">Target Net Yield</p>
                <p className="mt-1 font-sans text-sm sm:text-base font-bold text-white">7.80% IRR</p>
                <p className="text-[11px] text-slate-300 font-mono">Quarterly NDCF Distributions</p>
              </div>
              <div>
                <p className="font-mono text-[10px] uppercase tracking-[0.2em] text-slate-400">Statutory Rail</p>
                <p className="mt-1 font-sans text-sm sm:text-base font-bold text-white">Zero Custody ASBA</p>
                <p className="text-[11px] text-blue-300 font-mono">Axis Trustee Monitored</p>
              </div>
            </div>
          </div>
        </div>
      </section>

      {/* ========================================================================= */}
      {/* SECTION 2: WHY ACRESYNC (Asymmetric 2-column layout + Pipeline Simulation) */}
      {/* ========================================================================= */}
      <section className="relative w-full py-24 sm:py-32 border-t border-white/10 bg-gradient-to-b from-[#030712] via-[#050c1f] to-[#030712]">
        <div className="mx-auto w-full max-w-7xl px-6 sm:px-10">
          <div className="grid grid-cols-1 lg:grid-cols-12 gap-10 lg:gap-16 items-start">
            <FadeIn className="lg:col-span-5">
              <span className="font-mono text-xs uppercase tracking-[0.25em] text-blue-400">Institutional Philosophy</span>
              <h2 className="mt-3 font-sans text-4xl sm:text-5xl font-bold tracking-tight text-white leading-[1.08]">
                Why <span className="em">AcreSync</span>
              </h2>
            </FadeIn>

            <FadeIn delay={0.15} className="lg:col-span-7">
              <p className="text-xl sm:text-2xl text-slate-200 font-light leading-relaxed">
                Commercial real estate is changing. Don’t just watch prime institutional assets from the sidelines — own verified fractional units.{' '}
                <span className="em">
                  We help unitholders invest with sovereign confidence, regulated statutory compliance, and zero platform custody.
                </span>
              </p>
            </FadeIn>
          </div>

          {/* Interactive Flow Architecture Card */}
          <FadeIn delay={0.25} className="mt-16 overflow-hidden rounded-3xl border border-white/12 bg-[#050c1f]/90 p-8 sm:p-12 shadow-2xl backdrop-blur-xl">
            <div className="grid grid-cols-1 md:grid-cols-4 gap-6 relative">
              <div className="rounded-2xl border border-white/10 bg-[#07132e]/90 p-6 flex flex-col justify-between">
                <div>
                  <div className="flex h-10 w-10 items-center justify-center rounded-xl bg-blue-500/20 text-blue-300 border border-blue-400/30">
                    <Landmark className="h-5 w-5" />
                  </div>
                  <h4 className="mt-4 font-sans text-sm font-bold uppercase tracking-wider text-white">01. ASBA Block</h4>
                  <p className="mt-2 text-xs text-slate-300 leading-relaxed">
                    Rupees stay in your own bank account. A lien is marked by the bank under RBI regulations. Never deposited to the platform.
                  </p>
                </div>
                <div className="mt-4 font-mono text-[10px] text-emerald-400 uppercase">RBI Regulated</div>
              </div>

              <div className="rounded-2xl border border-white/10 bg-[#07132e]/90 p-6 flex flex-col justify-between">
                <div>
                  <div className="flex h-10 w-10 items-center justify-center rounded-xl bg-emerald-500/20 text-emerald-300 border border-emerald-400/30">
                    <ShieldCheck className="h-5 w-5" />
                  </div>
                  <h4 className="mt-4 font-sans text-sm font-bold uppercase tracking-wider text-white">02. Trustee Audit</h4>
                  <p className="mt-2 text-xs text-slate-300 leading-relaxed">
                    Axis Trustee Services validates every prospectus document, valuer certificate, and allotment schedule before issuance.
                  </p>
                </div>
                <div className="mt-4 font-mono text-[10px] text-blue-300 uppercase">SEBI Independent</div>
              </div>

              <div className="rounded-2xl border border-white/10 bg-[#07132e]/90 p-6 flex flex-col justify-between">
                <div>
                  <div className="flex h-10 w-10 items-center justify-center rounded-xl bg-purple-500/20 text-purple-300 border border-purple-400/30">
                    <Lock className="h-5 w-5" />
                  </div>
                  <h4 className="mt-4 font-sans text-sm font-bold uppercase tracking-wider text-white">03. Merkle Anchor</h4>
                  <p className="mt-2 text-xs text-slate-300 leading-relaxed">
                    Bid books and ballot leaves are hashed via SHA-256 and anchored to Ethereum Sepolia. Winners and losers both published.
                  </p>
                </div>
                <div className="mt-4 font-mono text-[10px] text-purple-300 uppercase">Cryptographic Proof</div>
              </div>

              <div className="rounded-2xl border border-white/10 bg-[#07132e]/90 p-6 flex flex-col justify-between">
                <div>
                  <div className="flex h-10 w-10 items-center justify-center rounded-xl bg-amber-500/20 text-amber-300 border border-amber-400/30">
                    <Coins className="h-5 w-5" />
                  </div>
                  <h4 className="mt-4 font-sans text-sm font-bold uppercase tracking-wider text-white">04. Direct NDCF</h4>
                  <p className="mt-2 text-xs text-slate-300 leading-relaxed">
                    Rental revenues from MNC tenants are distributed directly into unitholders' verified bank accounts on a quarterly schedule.
                  </p>
                </div>
                <div className="mt-4 font-mono text-[10px] text-amber-300 uppercase">95% Cashflow Payout</div>
              </div>
            </div>
          </FadeIn>
        </div>
      </section>

      {/* ========================================================================= */}
      {/* SECTION 3: ARROWS SECTION ("This isn’t just about real estate.")           */}
      {/* ========================================================================= */}
      <section className="relative w-full py-24 sm:py-32 border-t border-white/10 bg-[#030712]">
        <div className="mx-auto w-full max-w-7xl px-6 sm:px-10">
          <FadeIn>
            <h2 className="font-sans text-4xl sm:text-5xl md:text-6xl font-bold tracking-tight text-white leading-tight">
              This isn’t just <span className="em">about real estate.</span>
            </h2>
          </FadeIn>

          {/* 4 Tall Vertical Photo Cards with Hover Expansion */}
          <div className="mt-12 grid grid-cols-1 sm:grid-cols-2 lg:grid-cols-4 gap-6">
            {[
              {
                num: '01',
                title: 'Institutional Grade',
                desc: 'Premium Grade-A Tech Parks in top economic corridors with long-term lease covenants.',
                image: ASSETS.glassFacade,
              },
              {
                num: '02',
                title: 'Zero Custody Rails',
                desc: 'Your capital stays safely in your scheduled commercial bank account until allotment.',
                image: ASSETS.executiveAtrium,
              },
              {
                num: '03',
                title: 'Cryptographic Ballot',
                desc: 'Transparent lottery draws mixing future block hashes. Mathematical certainty over human trust.',
                image: ASSETS.nightTower,
              },
              {
                num: '04',
                title: 'Quarterly Cashflow',
                desc: '95% of Net Distributable Cash Flow wired straight to investor bank accounts.',
                image: ASSETS.modernTerrace,
              },
            ].map((card, i) => (
              <FadeIn key={card.num} delay={i * 0.1}>
                <div className="group relative h-[440px] sm:h-[480px] w-full overflow-hidden rounded-3xl border border-white/15 bg-[#050c1f] p-7 flex flex-col justify-between transition-all duration-500 hover:border-white/40 shadow-xl">
                  {/* Background Photo */}
                  <img
                    src={card.image}
                    alt={card.title}
                    className="absolute inset-0 h-full w-full object-cover filter brightness-[0.35] contrast-[1.1] transition-transform duration-700 group-hover:scale-105"
                  />
                  <div className="absolute inset-0 bg-gradient-to-t from-[#030712] via-[#050c1f]/40 to-transparent" />

                  {/* Card Header */}
                  <div className="relative z-10 flex items-center justify-between">
                    <span className="font-mono text-xs font-bold text-blue-300">{card.num}</span>
                    <span className="h-2 w-2 rounded-full bg-white/40 group-hover:bg-blue-400 transition-colors" />
                  </div>

                  {/* Card Content */}
                  <div className="relative z-10">
                    <h3 className="font-sans text-xl font-bold text-white uppercase tracking-tight">
                      {card.title}
                    </h3>
                    <p className="mt-2 text-xs text-slate-300 leading-relaxed font-light">
                      {card.desc}
                    </p>
                  </div>
                </div>
              </FadeIn>
            ))}
          </div>

          <FadeIn delay={0.25} className="mt-12 max-w-3xl">
            <p className="text-lg sm:text-xl text-slate-300 font-light leading-relaxed">
              It’s about sovereign ownership. Progress. Removing intermediaries.{' '}
              <span className="em">
                You’re not just looking for an investment — you’re looking for mathematical alignment and unshakeable asset backing. That’s what we build.
              </span>
            </p>
          </FadeIn>
        </div>
      </section>

      {/* ========================================================================= */}
      {/* SECTION 4: REWIRED WRAPPER ("Real Estate, Rewired." with Steps 01, 02, 03) */}
      {/* ========================================================================= */}
      <section className="relative w-full py-24 sm:py-32 border-t border-white/10 bg-gradient-to-b from-[#030712] via-[#050c1f] to-[#030712]">
        <div className="mx-auto w-full max-w-7xl px-6 sm:px-10">
          <div className="grid grid-cols-1 lg:grid-cols-12 gap-12 lg:gap-16 items-start">
            {/* Left Column: Heading & Action */}
            <FadeIn className="lg:col-span-5">
              <h2 className="font-sans text-4xl sm:text-5xl md:text-6xl font-bold tracking-tight text-white leading-[1.06]">
                Real Estate, <br />
                <span className="em">Rewired.</span>
              </h2>
              <p className="mt-6 text-sm text-slate-300 leading-relaxed max-w-md">
                We dismantled the black box of private real estate syndicates and rebuilt it on regulated bank escrow and public cryptographic rollups.
              </p>
              <div className="mt-8">
                <a
                  href="#live-schemes"
                  className="button-round bg-white text-[#040916] px-7 py-3.5 text-xs font-bold uppercase tracking-wider shadow-lg hover:bg-slate-100 transition-all cursor-pointer"
                >
                  <span>Start Your Search</span>
                  <span className="flex h-5 w-5 items-center justify-center rounded-full bg-[#040916] text-white">
                    <ArrowRight className="h-3 w-3" />
                  </span>
                </a>
              </div>
            </FadeIn>

            {/* Right Column: Steps 01, 02, 03 */}
            <FadeIn delay={0.15} className="lg:col-span-7 space-y-8">
              <div className="font-mono text-xs uppercase tracking-[0.25em] text-slate-400">
                Steps to Sovereign Unitholding:
              </div>

              {[
                {
                  step: '01',
                  lead: 'Direct ASBA Bank Lien.',
                  detail: 'Your capital never touches the platform or operator. Funds remain blocked in your own scheduled bank until official allotment.',
                },
                {
                  step: '02',
                  lead: 'Cryptographic Ballot & Commit.',
                  detail: 'Every single bid is hashed into an immutable Merkle leaf on Sepolia. The ballot mixes future block entropy nobody can game.',
                },
                {
                  step: '03',
                  lead: 'Quarterly Cashflow Yields.',
                  detail: 'Receive direct NDCF dividend payouts into your bank account, verified by independent trustee audits and on-chain proofs.',
                },
              ].map((item) => (
                <div
                  key={item.step}
                  className="group flex gap-6 sm:gap-8 rounded-2xl border border-white/10 bg-[#07132e]/60 p-6 sm:p-8 transition-all hover:border-white/30 hover:bg-[#07132e]"
                >
                  <span className="font-mono text-2xl font-bold text-blue-300 group-hover:text-white transition-colors">
                    {item.step}
                  </span>
                  <div>
                    <h3 className="font-sans text-base sm:text-lg font-bold text-white">
                      {item.lead}{' '}
                      <span className="em font-normal text-blue-200">{item.detail}</span>
                    </h3>
                  </div>
                </div>
              ))}
            </FadeIn>
          </div>
        </div>
      </section>

      {/* ========================================================================= */}
      {/* SECTION 5: FOR UNITHOLDERS & SPONSORS (Asymmetric Image Split)            */}
      {/* ========================================================================= */}
      <section className="relative w-full py-24 sm:py-32 border-t border-white/10 bg-[#030712]">
        <div className="mx-auto w-full max-w-7xl px-6 sm:px-10">
          <div className="grid grid-cols-1 lg:grid-cols-12 gap-12 lg:gap-16 items-center">
            {/* Left: Dual Staggered Images */}
            <FadeIn className="lg:col-span-6 relative">
              <div className="relative h-[380px] sm:h-[460px] w-full overflow-hidden rounded-3xl border border-white/15 shadow-2xl">
                <img
                  src={ASSETS.campusPark}
                  alt="Kalyani TechPark Whitefield"
                  className="h-full w-full object-cover filter brightness-[0.7] contrast-[1.08]"
                />
              </div>
              <div className="absolute -bottom-6 -right-4 hidden sm:block h-52 w-64 overflow-hidden rounded-2xl border border-white/20 shadow-[0_20px_50px_rgba(0,0,0,0.8)] backdrop-blur-md">
                <img
                  src={ASSETS.urbanLobby}
                  alt="Executive Reception"
                  className="h-full w-full object-cover filter brightness-[0.8]"
                />
              </div>
            </FadeIn>

            {/* Right: Narrative & Call to Action */}
            <FadeIn delay={0.2} className="lg:col-span-6">
              <span className="font-mono text-xs uppercase tracking-[0.25em] text-blue-400">
                For Investors & REIT Sponsors
              </span>
              <h2 className="mt-3 font-sans text-3xl sm:text-5xl font-bold tracking-tight text-white leading-[1.08]">
                Don’t Just Speculate. <br />
                <span className="em">Own Yield.</span>
              </h2>

              <p className="mt-6 text-sm sm:text-base text-slate-300 leading-relaxed font-light">
                At AcreSync, unitholders don’t rely on corporate promises — they verify with math.{' '}
                <span className="em">
                  SEBI SM-REIT guidelines enforce independent trusteeship, certified valuer reports, and zero custody rails. You’re building a generational portfolio with prime Grade-A commercial tech parks backed by multinational blue-chip leases.
                </span>
              </p>

              <div className="mt-8 flex items-center gap-4">
                <Link
                  to="/login"
                  className="button-round bg-white text-[#040916] px-7 py-3.5 text-xs font-bold uppercase tracking-wider shadow-md hover:bg-slate-100 transition-all cursor-pointer"
                >
                  <span>Join as Unitholder</span>
                  <span className="flex h-5 w-5 items-center justify-center rounded-full bg-[#040916] text-white">
                    <ArrowRight className="h-3 w-3" />
                  </span>
                </Link>
              </div>
            </FadeIn>
          </div>
        </div>
      </section>

      {/* ========================================================================= */}
      {/* SECTION 6: SERVICES / ECOSYSTEM (Full-Width Interactive Expandable Items) */}
      {/* ========================================================================= */}
      <section className="relative w-full py-24 sm:py-32 border-t border-white/10 bg-gradient-to-b from-[#030712] via-[#050c1f] to-[#030712]">
        <div className="mx-auto w-full max-w-7xl px-6 sm:px-10">
          <FadeIn className="grid grid-cols-1 lg:grid-cols-12 gap-8 items-end">
            <div className="lg:col-span-4 font-mono text-xs uppercase tracking-[0.25em] text-slate-400">
              Ecosystem Rails
            </div>
            <div className="lg:col-span-8">
              <h2 className="font-sans text-3xl sm:text-5xl font-bold tracking-tight text-white leading-[1.1]">
                How AcreSync <br />
                <span className="em">Can Help You</span>
              </h2>
            </div>
          </FadeIn>

          {/* 3 Full-Width Expandable Banner Cards (findrealestate style) */}
          <div className="mt-14 space-y-4">
            {[
              {
                num: '01',
                title: 'Invest & Bid Smarter',
                desc: 'Bid in live SEBI SM-REIT offerings with verified ASBA bank lien. One bid per investor, mathematical lottery ballot, and automated excess fund release.',
                badge: 'Bid & Invest',
                link: '/offers/4d7c3397-023e-4e1d-a328-a2af07fca928/bid',
                bg: ASSETS.glassFacade,
              },
              {
                num: '02',
                title: 'Structure & Sponsor Schemes',
                desc: 'Institutional REIT managers structure SPV schemes, publish certified valuer reports, and execute compliant offerings with Axis Trustee statutory oversight.',
                badge: 'Operator Console',
                link: '/admin',
                bg: ASSETS.executiveAtrium,
              },
              {
                num: '03',
                title: 'Verify & Audit Cryptographic Proofs',
                desc: 'Open cryptographic proofs for any unitholder, regulator, or auditor. Recompute SHA-256 Merkle branches directly against Ethereum Sepolia on-chain contracts.',
                badge: 'Merkle Proofs',
                link: '/offers/4d7c3397-023e-4e1d-a328-a2af07fca928/proofs',
                bg: ASSETS.nightTower,
              },
            ].map((srv) => (
              <FadeIn key={srv.num}>
                <Link
                  to={srv.link}
                  className="group relative block w-full overflow-hidden rounded-3xl border border-white/12 bg-[#050c1f] p-8 sm:p-12 transition-all duration-500 hover:border-white/40 shadow-xl"
                >
                  {/* Background Photo */}
                  <img
                    src={srv.bg}
                    alt={srv.title}
                    className="absolute inset-0 h-full w-full object-cover filter brightness-[0.25] contrast-[1.1] transition-transform duration-700 group-hover:scale-105"
                  />
                  <div className="absolute inset-0 bg-gradient-to-r from-[#030712] via-[#050c1f]/80 to-transparent" />

                  <div className="relative z-10 flex flex-col md:flex-row md:items-center justify-between gap-6">
                    <div className="max-w-2xl">
                      <span className="font-mono text-xs font-bold text-blue-300">{srv.num}</span>
                      <h3 className="mt-2 font-sans text-xl sm:text-2xl font-bold text-white uppercase tracking-tight">
                        {srv.title}
                      </h3>
                      <p className="mt-3 text-xs sm:text-sm text-slate-300 leading-relaxed font-light">
                        {srv.desc}
                      </p>
                    </div>

                    <div className="button-round rounded-full border border-white/20 bg-white/10 px-6 py-3 text-xs font-bold uppercase tracking-wider text-white backdrop-blur-md group-hover:bg-white group-hover:text-[#040916] transition-all">
                      <span>{srv.badge}</span>
                      <ArrowRight className="h-3.5 w-3.5" />
                    </div>
                  </div>
                </Link>
              </FadeIn>
            ))}
          </div>
        </div>
      </section>

      {/* ========================================================================= */}
      {/* SECTION 7: SUPPORT BEYOND BIDDING (3 Features Cards)                      */}
      {/* ========================================================================= */}
      <section className="relative w-full py-24 sm:py-32 border-t border-white/10 bg-[#030712]">
        <div className="mx-auto w-full max-w-7xl px-6 sm:px-10">
          <FadeIn className="grid grid-cols-1 lg:grid-cols-12 gap-8 items-end">
            <div className="lg:col-span-6">
              <h2 className="font-sans text-3xl sm:text-5xl font-bold tracking-tight text-white leading-[1.08]">
                Support Beyond <br />
                <span className="em">Bidding and Allotment.</span>
              </h2>
            </div>
            <div className="lg:col-span-6">
              <p className="text-sm sm:text-base text-slate-300 font-light leading-relaxed">
                The institutional commercial market never stands still — and neither do we.{' '}
                <span className="em">
                  Our infrastructure provides continuous statutory transparency and automated dividend rails throughout the entire holding cycle.
                </span>
              </p>
            </div>
          </FadeIn>

          <div className="mt-14 grid grid-cols-1 md:grid-cols-3 gap-6">
            {[
              {
                title: 'Banking Rails & ASBA',
                desc: 'Direct integration with ICICI, HDFC, and Axis bank escrow for instant zero-custody lien blocking.',
                img: ASSETS.executiveAtrium,
              },
              {
                title: 'Independent Trustee Oversight',
                desc: 'Statutory verification by Axis Trustee Services across all SPV accounts, valuation reports, and filings.',
                img: ASSETS.campusPark,
              },
              {
                title: 'On-Chain Rollup Attestation',
                desc: 'Public smart contracts on Ethereum Sepolia committing ballot roots and unitholder allocations.',
                img: ASSETS.modernTerrace,
              },
            ].map((feat, i) => (
              <FadeIn key={feat.title} delay={i * 0.1}>
                <div className="group relative h-[380px] w-full overflow-hidden rounded-3xl border border-white/15 bg-[#050c1f] p-7 flex flex-col justify-end transition-all hover:border-white/40 shadow-xl">
                  <img
                    src={feat.img}
                    alt={feat.title}
                    className="absolute inset-0 h-full w-full object-cover filter brightness-[0.35] transition-transform duration-700 group-hover:scale-105"
                  />
                  <div className="absolute inset-0 bg-gradient-to-t from-[#030712] via-[#050c1f]/60 to-transparent" />

                  <div className="relative z-10">
                    <h3 className="font-sans text-lg font-bold text-white uppercase tracking-tight">
                      {feat.title}
                    </h3>
                    <p className="mt-2 text-xs text-slate-300 leading-relaxed font-light">
                      {feat.desc}
                    </p>
                  </div>
                </div>
              </FadeIn>
            ))}
          </div>
        </div>
      </section>

      {/* ========================================================================= */}
      {/* SECTION 8: LIVE SCHEMES DESK (Our Live Core Operational Data)             */}
      {/* ========================================================================= */}
      <section id="live-schemes" className="relative w-full py-24 sm:py-32 border-t border-white/10 bg-gradient-to-b from-[#030712] via-[#050c1f] to-[#030712]">
        <div className="mx-auto w-full max-w-7xl px-6 sm:px-10">
          <FadeIn className="flex flex-col sm:flex-row sm:items-end justify-between gap-6">
            <div>
              <span className="font-mono text-xs uppercase tracking-[0.25em] text-blue-400">Live Asset Desk</span>
              <h2 className="mt-2 font-sans text-3xl sm:text-5xl font-bold tracking-tight text-white">
                Active SEBI <span className="em">SM-REIT Offerings</span>
              </h2>
            </div>
            <p className="text-xs font-mono uppercase tracking-wider text-slate-400">
              Verified by Axis Trustee Services Ltd
            </p>
          </FadeIn>

          <div className="mt-12">
            {schemesReq.loading ? (
              <LoadingCard lines={4} label="Loading active schemes from Go API..." />
            ) : schemes.length === 0 ? (
              <div className="rounded-3xl border border-white/15 bg-[#07132e]/80 p-10 text-center">
                <p className="text-slate-300">No active schemes currently listed.</p>
              </div>
            ) : (
              <div className="grid grid-cols-1 gap-8">
                {schemes.map((s) => (
                  <FadeIn key={s.id}>
                    <div className="overflow-hidden rounded-3xl border border-white/15 bg-[#050c1f]/95 shadow-2xl backdrop-blur-2xl">
                      <div className="grid grid-cols-1 lg:grid-cols-12">
                        {/* Scheme Image Preview */}
                        <div className="lg:col-span-5 relative min-h-[300px] overflow-hidden">
                          <img
                            src={ASSETS.campusPark}
                            alt={s.name}
                            className="h-full w-full object-cover filter brightness-[0.75] contrast-[1.05]"
                          />
                          <div className="absolute inset-0 bg-gradient-to-t from-[#050c1f] via-transparent to-transparent" />
                          <div className="absolute top-5 left-5">
                            <span className="rounded-full border border-emerald-400/30 bg-[#061e24]/90 px-3.5 py-1 text-xs font-mono uppercase tracking-wider text-emerald-300 backdrop-blur-md">
                              Live Bidding Active
                            </span>
                          </div>
                        </div>

                        {/* Scheme Info & Live Metrics */}
                        <div className="lg:col-span-7 p-7 sm:p-10 flex flex-col justify-between">
                          <div>
                            <div className="flex flex-wrap items-center justify-between gap-3">
                              <span className="font-mono text-xs text-blue-300 uppercase tracking-widest">
                                Scheme Code: {typeof s.code === 'string' ? s.code : s.id.slice(0, 8)}
                              </span>
                              <span className="font-mono text-xs text-slate-400">
                                CIN: U70109KA2024PTC184201
                              </span>
                            </div>

                            <h3 className="mt-3 font-sans text-2xl sm:text-3xl font-bold uppercase tracking-tight text-white">
                              {s.name}
                            </h3>
                            <p className="mt-2 text-xs sm:text-sm text-slate-300 leading-relaxed font-light">
                              {typeof s.description === 'string' ? s.description : 'Grade-A commercial IT Tech Park located in Whitefield, Bengaluru with 100% MNC tenant occupancy and long-term lease escalations.'}
                            </p>

                            {/* Key Asset Parameters */}
                            <div className="mt-6 grid grid-cols-2 sm:grid-cols-4 gap-4 border-y border-white/10 py-5">
                              <div>
                                <span className="font-mono text-[10px] uppercase text-slate-400">Total Units</span>
                                <p className="mt-1 font-sans text-base font-bold text-white">{(s.totalUnits ?? 500000).toLocaleString('en-IN')}</p>
                              </div>
                              <div>
                                <span className="font-mono text-[10px] uppercase text-slate-400">Face Value</span>
                                <p className="mt-1 font-sans text-base font-bold text-white">{inr(s.unitFaceValuePaise ?? 100000)}</p>
                              </div>
                              <div>
                                <span className="font-mono text-[10px] uppercase text-slate-400">Occupancy</span>
                                <p className="mt-1 font-sans text-base font-bold text-emerald-400">100%</p>
                              </div>
                              <div>
                                <span className="font-mono text-[10px] uppercase text-slate-400">Tenant Anchor</span>
                                <p className="mt-1 font-sans text-base font-bold text-blue-300">Amazon / Cisco</p>
                              </div>
                            </div>
                          </div>

                          {/* Action Footnotes & Direct Navigation */}
                          <div className="mt-8 flex flex-wrap items-center justify-between gap-4">
                            <div className="flex items-center gap-3">
                              <Link
                                to={`/offers/4d7c3397-023e-4e1d-a328-a2af07fca928/bid`}
                                className="button-round bg-white text-[#040916] px-6 py-3 text-xs font-bold uppercase tracking-wider hover:bg-slate-100 transition-all shadow-md cursor-pointer"
                              >
                                <span>Place ASBA Bid</span>
                                <ArrowRight className="h-3.5 w-3.5" />
                              </Link>

                              <Link
                                to={`/schemes/${s.id}`}
                                className="button-round border border-white/20 bg-white/5 px-5 py-3 text-xs font-bold uppercase tracking-wider text-white hover:bg-white/10 transition-all cursor-pointer"
                              >
                                View Scheme
                              </Link>
                            </div>

                            <Link
                              to="/offers/4d7c3397-023e-4e1d-a328-a2af07fca928/proofs"
                              className="font-mono text-xs text-blue-300 underline underline-offset-4 hover:text-white transition-colors"
                            >
                              Verify Merkle Root &rarr;
                            </Link>
                          </div>
                        </div>
                      </div>
                    </div>
                  </FadeIn>
                ))}
              </div>
            )}
          </div>
        </div>
      </section>

      {/* ========================================================================= */}
      {/* SECTION 9: RESEARCH & DILIGENCE POSTS (findrealestate latest-posts style) */}
      {/* ========================================================================= */}
      <section className="relative w-full py-24 sm:py-32 border-t border-white/10 bg-[#030712]">
        <div className="mx-auto w-full max-w-7xl px-6 sm:px-10">
          <FadeIn className="grid grid-cols-1 lg:grid-cols-12 gap-8 items-end">
            <div className="lg:col-span-6 font-mono text-xs uppercase tracking-[0.25em] text-slate-400">
              Research & Diligence Reports
            </div>
            <div className="lg:col-span-6">
              <h2 className="font-sans text-3xl sm:text-5xl font-bold tracking-tight text-white leading-[1.08]">
                Institutional <br />
                <span className="em">Market Intelligence</span>
              </h2>
            </div>
          </FadeIn>

          <div className="mt-14 grid grid-cols-1 md:grid-cols-3 gap-6">
            {[
              {
                date: '2026-09-15',
                title: 'SEBI SM-REIT Regulations: How Fractional Ownership Gained Statutory Armor',
                summary: 'An executive breakdown of the regulatory guidelines governing micro-REIT SPVs, certified valuers, and independent trustees.',
                img: ASSETS.executiveAtrium,
              },
              {
                date: '2026-08-20',
                title: 'Bangalore Commercial Tech Parks: Analyzing Grade-A Rental Cashflows',
                summary: 'A deep dive into lease escalations, weighted average lease expiry (WALE), and yield metrics across Whitefield campuses.',
                img: ASSETS.campusPark,
              },
              {
                date: '2026-07-10',
                title: 'The Demise of Platform Custody: Why Bank-Blocked ASBA Rails Win',
                summary: 'Why retail and institutional unitholders prefer zero-custody bank lien mechanisms over proprietary platform wallet balances.',
                img: ASSETS.nightTower,
              },
            ].map((post, i) => (
              <FadeIn key={post.title} delay={i * 0.1}>
                <div className="group rounded-3xl border border-white/12 bg-[#050c1f] p-6 transition-all hover:border-white/30 flex flex-col justify-between h-full">
                  <div>
                    <div className="h-48 w-full overflow-hidden rounded-2xl border border-white/10 relative">
                      <img
                        src={post.img}
                        alt={post.title}
                        className="h-full w-full object-cover filter brightness-[0.7] transition-transform duration-500 group-hover:scale-105"
                      />
                    </div>
                    <p className="mt-5 font-mono text-[11px] text-blue-300">{post.date}</p>
                    <h3 className="mt-2 font-sans text-base font-bold text-white group-hover:text-blue-200 transition-colors leading-snug">
                      {post.title}
                    </h3>
                    <p className="mt-2 text-xs text-slate-300 leading-relaxed font-light">
                      {post.summary}
                    </p>
                  </div>

                  <div className="mt-6 pt-4 border-t border-white/10">
                    <span className="inline-flex items-center gap-1.5 font-mono text-xs font-semibold uppercase tracking-wider text-white group-hover:text-blue-300 transition-colors">
                      Read Analysis &rarr;
                    </span>
                  </div>
                </div>
              </FadeIn>
            ))}
          </div>
        </div>
      </section>

      {/* ========================================================================= */}
      {/* SECTION 10: CINEMATIC OUTRO BANNER (findrealestate outro style)           */}
      {/* ========================================================================= */}
      <section className="relative w-full py-28 sm:py-36 border-t border-white/10 overflow-hidden bg-[#030712]">
        <img
          src={ASSETS.outroCity}
          alt="Night Skyline"
          className="pointer-events-none absolute inset-0 h-full w-full object-cover filter brightness-[0.22] contrast-[1.15]"
        />
        <div className="pointer-events-none absolute inset-0 bg-gradient-to-t from-[#030712] via-[#050c1f]/70 to-[#030712]" />

        <div className="relative z-10 mx-auto w-full max-w-7xl px-6 sm:px-10 text-center">
          <FadeIn>
            <h2 className="font-sans text-4xl sm:text-6xl font-bold tracking-tight text-white leading-tight">
              Invest with Sovereignty. <br />
              <span className="em">We’ll Guide Your Journey.</span>
            </h2>
            <p className="mt-6 max-w-xl mx-auto text-sm sm:text-base text-slate-300 font-light">
              Experience the certainty of SEBI SM-REIT fractional unitholding backed by RBI-regulated bank rails and public cryptographic proofs.
            </p>
            <div className="mt-8 flex justify-center">
              <Link
                to="/login"
                className="button-round bg-white text-[#040916] px-8 py-4 text-xs font-bold uppercase tracking-wider shadow-2xl hover:bg-slate-100 transition-all cursor-pointer"
              >
                <span>Access Unitholder Portal</span>
                <ArrowRight className="h-4 w-4" />
              </Link>
            </div>
          </FadeIn>
        </div>
      </section>

      {/* ========================================================================= */}
      {/* SECTION 11: FOOTER (Complete findrealestate.com footer structure)          */}
      {/* ========================================================================= */}
      <footer className="relative w-full border-t border-white/10 bg-[#02050e] pt-20 pb-12">
        <div className="mx-auto w-full max-w-7xl px-6 sm:px-10">
          {/* Top Newsletter & Offices Row */}
          <div className="grid grid-cols-1 lg:grid-cols-12 gap-12 pb-16 border-b border-white/10">
            {/* Newsletter Column */}
            <div className="lg:col-span-5">
              <h4 className="font-sans text-lg font-bold text-white uppercase tracking-tight">
                Subscribe to Institutional Disclosures
              </h4>
              <p className="mt-2 text-xs text-slate-400 font-light max-w-sm leading-relaxed">
                Receive quarterly Net Distributable Cash Flow reports, scheme valuation filings, and statutory disclosures.
              </p>

              <form onSubmit={handleNewsletterSubmit} className="mt-6 flex max-w-md items-center">
                <input
                  type="email"
                  value={newsletterEmail}
                  onChange={(e) => setNewsletterEmail(e.target.value)}
                  placeholder="Enter institutional email"
                  required
                  className="w-full rounded-l-full border border-white/15 bg-white/5 px-5 py-3 text-xs text-white placeholder:text-slate-500 focus:border-white focus:outline-none"
                />
                <button
                  type="submit"
                  className="button-round rounded-r-full bg-white px-5 py-3 text-[#040916] hover:bg-slate-200 transition-colors cursor-pointer"
                >
                  <ArrowRight className="h-4 w-4" />
                </button>
              </form>
              {subscribed && (
                <p className="mt-2 font-mono text-[11px] text-emerald-400">
                  Subscribed to AcreSync regulatory alerts.
                </p>
              )}
            </div>

            {/* Offices Row */}
            <div className="lg:col-span-7 grid grid-cols-1 sm:grid-cols-3 gap-6">
              <div>
                <p className="font-mono text-[10px] uppercase tracking-[0.2em] text-slate-400">Head Office</p>
                <p className="mt-2 text-xs font-semibold text-white">Bandra-Kurla Complex (BKC)</p>
                <p className="mt-1 text-[11px] text-slate-400 leading-relaxed font-light">
                  5th Floor, Maker Maxity, BKC, Mumbai, Maharashtra 400051
                </p>
              </div>

              <div>
                <p className="font-mono text-[10px] uppercase tracking-[0.2em] text-slate-400">Technology Hub</p>
                <p className="mt-2 text-xs font-semibold text-white">Bengaluru Tech Park</p>
                <p className="mt-1 text-[11px] text-slate-400 leading-relaxed font-light">
                  Outer Ring Road, Kadubeesanahalli, Bengaluru, Karnataka 560103
                </p>
              </div>

              <div>
                <p className="font-mono text-[10px] uppercase tracking-[0.2em] text-slate-400">Diligence & Trustee</p>
                <p className="mt-2 text-xs font-semibold text-white">Axis Trustee Services</p>
                <p className="mt-1 text-[11px] text-slate-400 leading-relaxed font-light">
                  Wadia International Centre, P.B. Marg, Worli, Mumbai 400025
                </p>
              </div>
            </div>
          </div>

          {/* Middle Nav Links */}
          <div className="py-10 flex flex-wrap items-center justify-between gap-6 border-b border-white/10">
            <div className="flex flex-wrap items-center gap-6 text-xs font-bold uppercase tracking-wider text-slate-300">
              <a href="#live-schemes" className="hover:text-white transition-colors">Schemes</a>
              <Link to="/portfolio" className="hover:text-white transition-colors">Portfolio</Link>
              <Link to="/offers/4d7c3397-023e-4e1d-a328-a2af07fca928/documents" className="hover:text-white transition-colors">Due Diligence</Link>
              <Link to="/offers/4d7c3397-023e-4e1d-a328-a2af07fca928/proofs" className="hover:text-white transition-colors">Merkle Proofs</Link>
              <Link to="/admin" className="hover:text-white transition-colors">Operator Console</Link>
            </div>

            <div className="flex items-center gap-6 font-mono text-xs text-slate-400">
              <a
                href="https://sepolia.etherscan.io/address/0xa656a42974b40cf64f32e758abb0689a2a178391"
                target="_blank"
                rel="noreferrer"
                className="hover:text-white transition-colors flex items-center gap-1"
              >
                <span>Sepolia Contract</span>
                <ExternalLink className="h-3 w-3" />
              </a>
              <a href="https://github.com/poryvalent/realestate-tokenization-framework" target="_blank" rel="noreferrer" className="hover:text-white transition-colors">
                GitHub ↗
              </a>
            </div>
          </div>

          {/* Bottom Massive Typography Brand Logo (findrealestate footer style) */}
          <div className="py-12 select-none overflow-hidden text-center">
            <span className="block font-sans text-[15vw] font-black uppercase tracking-[-0.05em] text-white/[0.08] leading-none">
              ACRESYNC
            </span>
          </div>

          {/* Legal Compliance Notices */}
          <div className="flex flex-col sm:flex-row items-center justify-between gap-4 text-[11px] text-slate-500 font-mono">
            <div>
              AcreSync Technologies India Ltd · SEBI SM-REIT Framework Reg No: IN/REIT/24-25/0012
            </div>
            <div>
              The chain attests · It never custodies · © {new Date().getFullYear()} AcreSync
            </div>
          </div>
        </div>
      </footer>
    </div>
  );
}
