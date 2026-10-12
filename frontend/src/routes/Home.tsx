import { useState, useRef, useEffect } from 'react';
import { Link, useNavigate } from 'react-router-dom';
import {
  Building2,
  ArrowRight,
  ShieldCheck,
  Landmark,
  Coins,
  ExternalLink,
  Lock,
} from 'lucide-react';
import SplashScreen from '../components/SplashScreen';
import ScrollGallery from '../components/ScrollGallery';
import { Io } from '../components/motion';
import Lenis from 'lenis';
import { usePoll } from '../api/usePoll';
import { pageItems } from '../api/client';
import type { Scheme } from '../api/types';
import { inr } from '../lib/format';

const METRICS = [
  {
    icon: Landmark,
    tone: 'text-amber-500',
    label: 'SEBI TICKET SIZE',
    value: '₹10,00,000',
    note: 'Statutory unitholder threshold under SM-REIT guidelines.',
  },
  {
    icon: ShieldCheck,
    tone: 'text-emerald-400',
    label: 'BANK LIEN ESCROW',
    value: '100% ASBA',
    note: 'Funds blocked in investor bank account until statutory allotment.',
  },
  {
    icon: Lock,
    tone: 'text-blue-400',
    label: 'CUSTODY RISK',
    value: '0.00',
    note: 'Pure non-custodial attestation. Zero platform fund holding.',
  },
  {
    icon: Coins,
    tone: 'text-purple-400',
    label: 'AUDITABILITY',
    value: 'SHA-256',
    note: 'Cryptographic Merkle tree anchored on Ethereum Sepolia.',
  },
] as const;

export default function Home() {
  const navigate = useNavigate();
  const [showSplash, setShowSplash] = useState<boolean>(true);
  const [role, setRole] = useState<'INVESTOR' | 'OPERATOR'>('INVESTOR');
  const [menuOpen, setMenuOpen] = useState(false);
  const [isMenuClosing, setIsMenuClosing] = useState(false);
  const [scrollProgress, setScrollProgress] = useState(0);
  const [galleryVisible, setGalleryVisible] = useState(true);

  const stageRef = useRef<HTMLDivElement>(null);

  // Poll live schemes from the API backend
  const schemesReq = usePoll<Scheme[] | { items: Scheme[] }>('/schemes', {});
  const schemes = pageItems(schemesReq.data);

  // Measure scroll progress (0 to 1) over the 900vh stage, at most once per animation frame.
  // The gallery overlay is hidden once the content block has fully covered the viewport,
  // i.e. once the stage's bottom edge has scrolled above the top of the screen.
  useEffect(() => {
    let frame = 0;

    const measure = () => {
      frame = 0;
      const stage = stageRef.current;
      if (!stage) return;
      const totalScrollable = stage.offsetHeight - window.innerHeight;
      if (totalScrollable <= 0) return;
      const rect = stage.getBoundingClientRect();
      setScrollProgress(Math.max(0, Math.min(1, -rect.top / totalScrollable)));
      setGalleryVisible(rect.bottom > 0);
    };

    const schedule = () => {
      if (frame === 0) frame = window.requestAnimationFrame(measure);
    };

    window.addEventListener('scroll', schedule, { passive: true });
    window.addEventListener('resize', schedule);
    measure();
    return () => {
      window.removeEventListener('scroll', schedule);
      window.removeEventListener('resize', schedule);
      if (frame !== 0) window.cancelAnimationFrame(frame);
    };
  }, []);

  // Inertial smooth scrolling for the scroll-driven story. Lenis drives the native scroll
  // position, so the progress listener above keeps working; it also respects reduced motion.
  useEffect(() => {
    if (showSplash) return;
    const lenis = new Lenis({ lerp: 0.08, smoothWheel: true, anchors: true, autoRaf: true });
    return () => lenis.destroy();
  }, [showSplash]);

  // Menu open / close logic
  const handleToggleMenu = () => {
    if (menuOpen) {
      setIsMenuClosing(true);
      setTimeout(() => {
        setMenuOpen(false);
        setIsMenuClosing(false);
      }, 300);
    } else {
      setMenuOpen(true);
    }
  };

  const handleCloseMenu = () => {
    if (!menuOpen) return;
    setIsMenuClosing(true);
    setTimeout(() => {
      setMenuOpen(false);
      setIsMenuClosing(false);
    }, 300);
  };

  return (
    <div className="relative w-full bg-ink text-white selection:bg-amber-500 selection:text-ink font-ui">
      {/* ========================================================================= */}
      {/* SECTION 1: SPLASH SCREEN (plays intro video once) */}
      {/* ========================================================================= */}
      {showSplash && <SplashScreen onComplete={() => setShowSplash(false)} />}

      {/* ========================================================================= */}
      {/* SECTION 2: HERO SECTION (Fixed background, navbars) */}
      {/* ========================================================================= */}
      <section
        className={`fixed inset-0 h-screen w-full overflow-hidden z-0 bg-ink pointer-events-auto flex flex-col justify-between ${
          showSplash ? 'hero-pending' : 'hero-ready'
        }`}
      >
        {/* 2A. Background clip: aerial glass towers at dusk, looping. Bundled locally under the
            Mixkit Stock Video Free License (mixkit.co/free-stock-video/landscape-from-an-aerial-view-of-a-big-city-49837/).
            The original hero.mp4 stays in public/videos but is
            not used: it dissolves into a generated website mock-up with garbled text after ~0.4s. */}
        <div className="absolute inset-0 z-0">
          <video
            autoPlay
            muted
            loop
            playsInline
            preload="auto"
            poster="/images/hero-poster.jpg"
            className="w-full h-full object-cover"
          >
            <source src="/videos/hero-skyline.mp4" type="video/mp4" />
          </video>
          {/* Legibility gradient plus a soft vignette for depth */}
          <div className="absolute inset-0 bg-gradient-to-b from-ink/70 via-ink/20 to-ink/85" />
          <div className="absolute inset-0 bg-[radial-gradient(ellipse_at_center,transparent_38%,rgba(3,7,18,0.6)_100%)]" />
        </div>

        {/* 2C. Top navbar */}
        <header
          data-enter="drop"
          className="relative z-10 flex items-center justify-between px-4 sm:px-6 md:px-12 pt-4 sm:pt-6 md:pt-8 pb-4 border-b border-white/10"
        >
          <Link
            to="/"
            className="font-display font-semibold text-white text-xl sm:text-2xl md:text-3xl tracking-[0.2em] uppercase hover:opacity-90 transition-opacity"
          >
            ACRESYNC
          </Link>

          <nav className="hidden md:flex items-center gap-10">
            <a
              href="#schemes"
              className="nav-link font-ui text-white/70 hover:text-white text-xs tracking-[0.2em] uppercase transition-colors duration-200"
            >
              SCHEMES
            </a>
            <Link
              to="/offers/4d7c3397-023e-4e1d-a328-a2af07fca928/proofs"
              className="nav-link font-ui text-white/70 hover:text-white text-xs tracking-[0.2em] uppercase transition-colors duration-200"
            >
              PROOFS
            </Link>
          </nav>

          {/* Segmented role switcher */}
          <div className="hidden sm:flex border border-white/20 rounded-sm overflow-hidden bg-ink/60 backdrop-blur-md">
            {(['INVESTOR', 'OPERATOR'] as const).map((r) => (
              <button
                key={r}
                type="button"
                onClick={() => setRole(r)}
                className={`px-4 md:px-5 py-2 md:py-2.5 text-[11px] md:text-xs font-ui font-medium tracking-[0.18em] uppercase transition-colors duration-200 ${
                  role === r ? 'bg-white text-ink' : 'text-white/70 hover:text-white'
                }`}
              >
                {r}
              </button>
            ))}
          </div>
        </header>

        {/* 2D. Hero heading */}
        <div className="relative z-10 flex-1 flex flex-col items-center justify-center px-4 sm:px-6 text-center">
          <div
            data-enter="rise"
            style={{ animationDelay: '0.25s' }}
            className="font-ui text-[11px] sm:text-xs tracking-[0.3em] uppercase text-white/60 mb-6 flex items-center gap-3"
          >
            <span className="w-1.5 h-1.5 bg-amber-500" />
            LIVE INSTITUTIONAL RAILS
          </div>

          {/* Fluid size keeps the two spec'd lines on two lines from tablet up. */}
          <h1
            data-enter="rise"
            className="font-display font-medium text-white text-center leading-[1.08] tracking-wide uppercase max-w-[92vw]"
            style={{ fontSize: 'clamp(1.9rem, 4.6vw, 5rem)', animationDelay: '0.4s' }}
          >
            INSTITUTIONAL REAL ESTATE,{' '}
            <br className="hidden md:inline" />
            TOKENIZED UNDER SEBI SM-REIT
          </h1>

          <p
            data-enter="rise"
            style={{ animationDelay: '0.7s' }}
            className="mt-6 font-ui font-light text-white/70 text-sm sm:text-base text-center max-w-xl"
          >
            {role === 'INVESTOR'
              ? 'Bid on live SM-REIT offerings with verified ASBA bank liens.'
              : 'Structure SPV schemes under Axis Trustee statutory oversight.'}
          </p>
        </div>

        {/* 2E. Bottom bar: scroll cue, morphing console, trust row */}
        <div className="relative z-10 grid grid-cols-[1fr_auto_1fr] items-center px-4 sm:px-6 md:px-12 pb-4 sm:pb-6 md:pb-10">
          <div
            data-enter="fade"
            style={{ animationDelay: '1.3s' }}
            className="hidden lg:flex items-center gap-4 font-ui text-[11px] tracking-[0.25em] uppercase text-white/50"
          >
            <span className="scroll-cue" aria-hidden="true" />
            Scroll to explore
          </div>

          <div data-enter="rise" style={{ animationDelay: '0.95s' }}>
            <div
              onClick={handleToggleMenu}
              className={`cursor-pointer bg-ink border border-white/15 rounded-sm flex items-center justify-between transition-all duration-500 ease-[cubic-bezier(0.16,1,0.3,1)] select-none shadow-2xl hover:border-white/40 ${
                menuOpen ? 'w-14 h-14 p-0 justify-center' : 'w-[280px] max-w-[85vw] h-14 px-6'
              }`}
            >
              {menuOpen ? (
                // Collapsed state: X close button
                <div className="relative w-5 h-5 flex items-center justify-center">
                  <span className="absolute w-5 h-[1.5px] bg-white rotate-45" />
                  <span className="absolute w-5 h-[1.5px] bg-white -rotate-45" />
                </div>
              ) : (
                // Expanded state: Building2, CONSOLE, hamburger
                <>
                  <div className="flex items-center gap-3">
                    <Building2 className="w-5 h-5 text-amber-500" />
                    <span className="font-ui text-white text-sm tracking-[0.15em] uppercase font-medium">
                      CONSOLE
                    </span>
                  </div>
                  <div className="relative w-6 h-6">
                    <span className="absolute top-[8px] left-0 w-6 h-[1.5px] bg-white" />
                    <span className="absolute top-[15px] left-0 w-6 h-[1.5px] bg-white" />
                  </div>
                </>
              )}
            </div>
          </div>

          <div
            data-enter="fade"
            style={{ animationDelay: '1.3s' }}
            className="hidden lg:flex items-center justify-end gap-3 font-ui text-[11px] tracking-[0.25em] uppercase text-white/50"
          >
            <span>Axis Trustee</span>
            <span className="text-white/25">·</span>
            <span>100% ASBA lien</span>
            <span className="text-white/25">·</span>
            <span>Ethereum Sepolia</span>
          </div>
        </div>
      </section>

      {/* ========================================================================= */}
      {/* 2F. MENU POPUP (Triggered by Hamburger click) */}
      {/* ========================================================================= */}
      {menuOpen && (
        <>
          {/* Overlay */}
          <div
            onClick={handleCloseMenu}
            className={`fixed inset-0 z-20 bg-ink/40 ${
              isMenuClosing ? 'menu-overlay-close' : 'menu-overlay-open'
            }`}
          />

          {/* Menu Panel */}
          <div
            className={`fixed z-30 left-1/2 bottom-[80px] sm:bottom-[100px] md:bottom-[120px] w-[92vw] max-w-[480px] bg-ink border border-white/15 rounded-md p-6 sm:p-8 md:p-10 shadow-2xl ${
              isMenuClosing ? 'menu-close' : 'menu-open'
            }`}
          >
            {/* Header */}
            <div className="font-ui text-neutral-400 text-xs tracking-[0.2em] uppercase mb-6 border-b border-white/10 pb-3 flex justify-between items-center">
              <span>MENU</span>
              <span className="text-[10px] text-amber-500">SEBI SM-REIT</span>
            </div>

            {/* Menu Items List */}
            <ul className="space-y-1">
              {[
                { label: 'Schemes', href: '#schemes' },
                { label: 'Offerings', href: '#schemes' },
                {
                  label: 'Proofs',
                  href: '/offers/4d7c3397-023e-4e1d-a328-a2af07fca928/proofs',
                },
                { label: 'Sponsors', href: '/admin' },
                { label: 'Contact', href: '#footer' },
              ].map((item, i) => (
                <li
                  key={item.label}
                  className="menu-item-enter"
                  style={{
                    animationDelay: `${i * 60 + 100}ms`,
                    opacity: 0,
                  }}
                >
                  {item.href.startsWith('/') ? (
                    <Link
                      to={item.href}
                      onClick={handleCloseMenu}
                      className="font-display font-medium text-white text-3xl md:text-4xl lg:text-5xl hover:text-neutral-300 transition-colors duration-200 block py-1 tracking-tight"
                    >
                      {item.label}
                    </Link>
                  ) : (
                    <a
                      href={item.href}
                      onClick={handleCloseMenu}
                      className="font-display font-medium text-white text-3xl md:text-4xl lg:text-5xl hover:text-neutral-300 transition-colors duration-200 block py-1 tracking-tight"
                    >
                      {item.label}
                    </a>
                  )}
                </li>
              ))}
            </ul>

            {/* Footer Section */}
            <div className="mt-8 pt-6 border-t border-white/10 space-y-3">
              <div className="flex justify-between items-center">
                <span className="font-ui text-neutral-400 text-sm">Trustee</span>
                <span className="font-ui text-white text-sm font-medium">
                  Axis Trustee
                </span>
              </div>
              <div className="flex justify-between items-center">
                <span className="font-ui text-neutral-400 text-sm">Escrow</span>
                <span className="font-ui text-white text-sm font-medium">
                  100% ASBA bank lien
                </span>
              </div>
              <div className="flex justify-between items-center">
                <span className="font-ui text-neutral-400 text-sm">Network</span>
                <span className="font-ui text-white text-sm font-medium">
                  Ethereum Sepolia
                </span>
              </div>
            </div>

            {/* CTA Button */}
            <button
              type="button"
              onClick={() => {
                handleCloseMenu();
                navigate('/login');
              }}
              className="w-full mt-6 bg-white text-ink py-3 rounded-sm font-ui text-sm font-medium tracking-[0.15em] uppercase hover:bg-neutral-100 transition-colors duration-200"
            >
              OPEN CONSOLE
            </button>
          </div>
        </>
      )}

      {/* ========================================================================= */}
      {/* SECTION 3: SCROLL-DRIVEN GALLERY OVERLAY (900vh stage) */}
      {/* ========================================================================= */}
      <div ref={stageRef} className="relative h-[900vh] w-full pointer-events-none">
        {/* Render fixed gallery while within scroll stage */}
        <ScrollGallery progress={scrollProgress} visible={galleryVisible} />
      </div>

      {/* ========================================================================= */}
      {/* SECTION 4: CONTENT BLOCK (Metrics, Expandable Cards, Schemes, Footer) */}
      {/* ========================================================================= */}
      <div className="relative z-20 w-full bg-ink text-white border-t border-white/10 shadow-[0_-24px_48px_rgba(3,7,18,0.35)]">
        {/* 4A. Institutional Metrics Bar */}
        <section className="w-full border-b border-white/10 bg-[#050c1f]">
          <div className="max-w-7xl mx-auto px-6 py-16 grid grid-cols-1 sm:grid-cols-2 lg:grid-cols-4 gap-8">
            {METRICS.map((m, i) => {
              const Icon = m.icon;
              return (
                <Io key={m.label} delay={i * 90} className="h-full">
                  <div className="h-full border border-white/10 p-8 rounded-md bg-ink/40">
                    <div className="font-ui font-medium text-[11px] uppercase tracking-[0.3em] text-neutral-400 mb-3 flex items-center gap-2">
                      <Icon className={`w-4 h-4 ${m.tone}`} />
                      {m.label}
                    </div>
                    <div className="font-display text-4xl sm:text-5xl text-white font-semibold tracking-tight lining-nums">{m.value}</div>
                    <div className="mt-2 text-xs text-neutral-400 font-ui">{m.note}</div>
                  </div>
                </Io>
              );
            })}
          </div>
        </section>

        {/* 4B. 3 Expandable Service Banner Cards (findrealestate style) */}
        <section className="w-full py-24 sm:py-32 border-b border-white/10">
          <div className="max-w-7xl mx-auto px-6 sm:px-10">
            <Io className="grid grid-cols-1 lg:grid-cols-12 gap-8 items-end mb-16">
              <div className="lg:col-span-4 font-ui font-medium text-[11px] uppercase tracking-[0.3em] text-neutral-400">
                ECOSYSTEM RAILS
              </div>
              <div className="lg:col-span-8">
                <h2 className="font-display text-5xl sm:text-6xl font-medium tracking-tight text-white leading-[1.05]">
                  Institutional <em className="italic font-normal text-amber-500">architecture</em>
                </h2>
              </div>
            </Io>

            <div className="space-y-4">
              {[
                {
                  num: '01',
                  title: 'Invest & Bid Smarter',
                  desc: 'Bid in live SEBI SM-REIT offerings with verified ASBA bank lien. One bid per investor, mathematical lottery ballot, and automated excess fund release.',
                  badge: 'ASBA Escrow Rail',
                  link: '/offers/4d7c3397-023e-4e1d-a328-a2af07fca928/bid',
                  image:
                    '/images/gallery-01.jpg',
                },
                {
                  num: '02',
                  title: 'Structure & Sponsor Schemes',
                  desc: 'Institutional REIT managers structure SPV schemes, publish certified valuer reports to IPFS, and execute compliant offerings with Axis Trustee statutory oversight.',
                  badge: 'Operator Console',
                  link: '/admin',
                  image:
                    '/images/gallery-02.webp',
                },
                {
                  num: '03',
                  title: 'Verify & Audit Cryptographic Proofs',
                  desc: 'Open cryptographic proofs for any unitholder, regulator, or auditor. Recompute SHA-256 Merkle branches directly against Ethereum Sepolia on-chain shadow ledger contracts.',
                  badge: 'Merkle Shadow Ledger',
                  link: '/offers/4d7c3397-023e-4e1d-a328-a2af07fca928/proofs',
                  image:
                    '/images/gallery-03.jpg',
                },
              ].map((card, i) => (
                <Io key={card.num} delay={i * 100}>
                  <Link
                    to={card.link}
                    className="group relative block w-full overflow-hidden border border-white/12 bg-[#050c1f] p-8 sm:p-12 transition-all duration-500 hover:border-white/40 rounded-md"
                  >
                    <img
                      src={card.image}
                      alt={card.title}
                      className="absolute inset-0 h-full w-full object-cover opacity-15 grayscale transition-all duration-700 group-hover:scale-105 group-hover:opacity-30 group-hover:grayscale-0 pointer-events-none"
                    />
                    <div className="absolute inset-0 bg-gradient-to-r from-[#030712] via-[#030712]/90 to-transparent pointer-events-none" />

                    <div className="relative z-10 flex flex-col justify-between min-h-[180px]">
                      <div className="flex items-center justify-between">
                        <div className="flex items-center gap-4">
                          <span className="font-display text-4xl sm:text-5xl text-amber-500 font-semibold lining-nums">
                            {card.num}
                          </span>
                          <span className="font-ui font-medium text-[11px] uppercase tracking-[0.25em] text-neutral-300 border border-white/10 px-3 py-1 rounded-xs bg-black/40">
                            {card.badge}
                          </span>
                        </div>
                        <div className="flex h-10 w-10 items-center justify-center border border-white/20 bg-white/5 transition-colors group-hover:bg-white group-hover:text-ink rounded-sm">
                          <ArrowRight className="h-5 w-5" />
                        </div>
                      </div>

                      <div className="mt-8 max-w-2xl">
                        <h3 className="font-display text-3xl sm:text-4xl font-medium tracking-tight text-white group-hover:text-amber-400 transition-colors">
                          {card.title}
                        </h3>
                        <p className="mt-3 font-ui text-sm sm:text-base text-neutral-300 leading-relaxed">
                          {card.desc}
                        </p>
                      </div>
                    </div>
                  </Link>
                </Io>
              ))}
            </div>
          </div>
        </section>

        {/* 4C. Live Scheme Offerings Grid */}
        <section id="schemes" className="w-full py-24 sm:py-32 border-b border-white/10">
          <div className="max-w-7xl mx-auto px-6 sm:px-10">
            <Io className="flex flex-col md:flex-row md:items-end justify-between mb-16 gap-6">
              <div>
                <div className="font-ui font-medium text-[11px] uppercase tracking-[0.3em] text-neutral-400 mb-3">
                  SEBI APPROVED PIPELINE
                </div>
                <h2 className="font-display text-5xl sm:text-6xl font-medium tracking-tight text-white leading-[1.05]">
                  Live scheme <em className="italic font-normal text-amber-500">offerings</em>
                </h2>
              </div>
              <div className="font-ui text-sm text-neutral-400 max-w-md">
                Institutional Grade-A real estate assets structured into SEBI SM-REIT schemes with statutory valuer disclosures.
              </div>
            </Io>

            <div className="grid grid-cols-1 md:grid-cols-2 lg:grid-cols-3 gap-8">
              {schemes.length > 0 ? (
                schemes.map((s, i) => (
                  <Io key={s.id} delay={i * 80} className="h-full">
                    <div
                      className="h-full border border-white/15 bg-[#050c1f] p-8 flex flex-col justify-between rounded-md hover:border-white/40 transition-colors"
                    >
                      <div>
                        <div className="flex justify-between items-start mb-6">
                          <span className="font-ui font-medium text-[10px] tracking-[0.25em] text-amber-500 uppercase border border-amber-500/30 px-2 py-0.5 rounded-xs">
                            SM-REIT SCHEME
                          </span>
                          <span className="font-ui text-[11px] text-neutral-400">
                            {typeof s.symbol === 'string' ? s.symbol : 'REIT-01'}
                          </span>
                        </div>
                        <h3 className="font-display text-3xl font-medium text-white tracking-tight mb-2">
                          {s.name}
                        </h3>
                        <p className="font-ui text-xs text-neutral-400 leading-relaxed mb-6">
                          {typeof s.description === 'string'
                            ? s.description
                            : 'Premium commercial asset held under registered Special Purpose Vehicle (SPV) with Axis Trustee statutory oversight.'}
                        </p>
                        <div className="space-y-2 py-4 border-y border-white/10 text-xs">
                          <div className="flex justify-between">
                            <span className="text-neutral-400">Target Corpus</span>
                            <span className="font-mono font-medium text-white tabular-nums">
                              {inr(s.targetCorpusPaise || 4800000000)}
                            </span>
                          </div>
                          <div className="flex justify-between">
                            <span className="text-neutral-400">Trustee</span>
                            <span className="text-white">Axis Trustee Services</span>
                          </div>
                          <div className="flex justify-between">
                            <span className="text-neutral-400">Escrow Mechanism</span>
                            <span className="text-emerald-400 font-ui">
                              ASBA Direct Lien
                            </span>
                          </div>
                        </div>
                      </div>

                      <div className="mt-8 flex gap-3">
                        <Link
                          to={`/offers/4d7c3397-023e-4e1d-a328-a2af07fca928/bid`}
                          className="flex-1 py-3 bg-white text-ink text-center text-xs font-semibold uppercase tracking-[0.15em] rounded-sm hover:bg-neutral-200 transition-colors"
                        >
                          PLACE ASBA BID
                        </Link>
                        <Link
                          to={`/schemes/${s.id}`}
                          className="p-3 border border-white/20 text-white flex items-center justify-center rounded-sm hover:border-white transition-colors"
                          title="View Scheme Details"
                        >
                          <ExternalLink className="w-4 h-4" />
                        </Link>
                      </div>
                    </div>
                  </Io>
                ))
              ) : (
                // Fallback static cards when schemes are loading
                [
                  {
                    title: 'Cyber Gateway Tech Park',
                    location: 'Whitefield, Bengaluru',
                    corpus: 4800000000,
                    units: '48,000 Units',
                    irr: '14.8%',
                  },
                  {
                    title: 'Horizon Logistics Hub',
                    location: 'JNPT Port Corridor, Navi Mumbai',
                    corpus: 6200000000,
                    units: '62,000 Units',
                    irr: '15.4%',
                  },
                  {
                    title: 'Financial District Atrium',
                    location: 'Gachibowli, Hyderabad',
                    corpus: 8500000000,
                    units: '85,000 Units',
                    irr: '14.2%',
                  },
                ].map((mock, i) => (
                  <Io key={mock.title} delay={i * 80} className="h-full">
                    <div
                      className="h-full border border-white/15 bg-[#050c1f] p-8 flex flex-col justify-between rounded-md hover:border-white/40 transition-colors"
                    >
                      <div>
                        <div className="flex justify-between items-start mb-6">
                          <span className="font-ui font-medium text-[10px] tracking-[0.25em] text-amber-500 uppercase border border-amber-500/30 px-2 py-0.5 rounded-xs">
                            SM-REIT SPV 0{i + 1}
                          </span>
                          <span className="font-ui text-[11px] text-neutral-400">
                            {mock.irr} Projected IRR
                          </span>
                        </div>
                        <h3 className="font-display text-3xl font-medium text-white tracking-tight mb-2">
                          {mock.title}
                        </h3>
                        <p className="font-ui text-xs text-neutral-400 leading-relaxed mb-6">
                          {mock.location} — Registered SPV with Axis Trustee statutory oversight, licensed valuation reports, and zero custody rails.
                        </p>
                        <div className="space-y-2 py-4 border-y border-white/10 text-xs">
                          <div className="flex justify-between">
                            <span className="text-neutral-400">Target Corpus</span>
                            <span className="font-mono font-medium text-white tabular-nums">
                              {inr(mock.corpus)}
                            </span>
                          </div>
                          <div className="flex justify-between">
                            <span className="text-neutral-400">Escrow Mechanism</span>
                            <span className="text-emerald-400 font-ui">
                              ASBA Direct Lien
                            </span>
                          </div>
                          <div className="flex justify-between">
                            <span className="text-neutral-400">Audit Status</span>
                            <span className="text-white font-ui">
                              Ethereum Sepolia
                            </span>
                          </div>
                        </div>
                      </div>

                      <div className="mt-8 flex gap-3">
                        <Link
                          to="/offers/4d7c3397-023e-4e1d-a328-a2af07fca928/bid"
                          className="flex-1 py-3 bg-white text-ink text-center text-xs font-semibold uppercase tracking-[0.15em] rounded-sm hover:bg-neutral-200 transition-colors"
                        >
                          PLACE ASBA BID
                        </Link>
                        <Link
                          to="/offers/4d7c3397-023e-4e1d-a328-a2af07fca928/proofs"
                          className="p-3 border border-white/20 text-white flex items-center justify-center rounded-sm hover:border-white transition-colors"
                          title="View Merkle Proofs"
                        >
                          <ExternalLink className="w-4 h-4" />
                        </Link>
                      </div>
                    </div>
                  </Io>
                ))
              )}
            </div>
          </div>
        </section>

        {/* 4D. Institutional Statutory Footer */}
        <footer id="footer" className="w-full bg-ink border-t border-white/10 py-16 sm:py-24">
          <div className="max-w-7xl mx-auto px-6 sm:px-10">
            <div className="grid grid-cols-1 md:grid-cols-4 gap-12 pb-16 border-b border-white/10">
              <div className="md:col-span-2">
                <div className="font-display text-3xl font-semibold uppercase tracking-[0.2em] text-white mb-4">
                  ACRESYNC
                </div>
                <p className="font-ui text-xs text-neutral-400 max-w-md leading-relaxed">
                  AcreSync operates as an institutional technological framework for SEBI SM-REIT tokenization. All bids and financial settlements execute strictly through authorized ASBA bank lien mandates with zero platform custody.
                </p>
                <div className="mt-6 flex items-center gap-4 text-xs font-ui text-neutral-400">
                  <span className="flex items-center gap-2">
                    <span className="w-1.5 h-1.5 bg-emerald-400 rounded-none" />
                    SEBI SM-REIT Compliant
                  </span>
                  <span className="flex items-center gap-2">
                    <span className="w-1.5 h-1.5 bg-blue-400 rounded-none" />
                    Axis Trustee Oversight
                  </span>
                </div>
              </div>

              <div>
                <div className="font-ui font-medium text-[11px] uppercase tracking-[0.3em] text-neutral-400 mb-4">
                  PLATFORM RAILS
                </div>
                <ul className="space-y-2.5 text-xs font-ui text-neutral-300">
                  <li>
                    <a href="#schemes" className="hover:text-white transition-colors">
                      Live Offerings
                    </a>
                  </li>
                  <li>
                    <Link
                      to="/offers/4d7c3397-023e-4e1d-a328-a2af07fca928/proofs"
                      className="hover:text-white transition-colors"
                    >
                      Merkle Tree Proofs
                    </Link>
                  </li>
                  <li>
                    <Link to="/portfolio" className="hover:text-white transition-colors">
                      Investor Portfolio
                    </Link>
                  </li>
                  <li>
                    <Link to="/admin" className="hover:text-white transition-colors">
                      Operator Console
                    </Link>
                  </li>
                </ul>
              </div>

              <div>
                <div className="font-ui font-medium text-[11px] uppercase tracking-[0.3em] text-neutral-400 mb-4">
                  LEGAL & GOVERNANCE
                </div>
                <ul className="space-y-2.5 text-xs font-ui text-neutral-300">
                  <li>
                    <Link
                      to="/offers/4d7c3397-023e-4e1d-a328-a2af07fca928/documents"
                      className="hover:text-white transition-colors"
                    >
                      Due Diligence Room
                    </Link>
                  </li>
                  <li>
                    <span className="text-neutral-500">SEBI Regulated SM-REIT</span>
                  </li>
                  <li>
                    <span className="text-neutral-500">RBI ASBA Circular Compliance</span>
                  </li>
                  <li>
                    <span className="text-neutral-500">Axis Trustee Framework</span>
                  </li>
                </ul>
              </div>
            </div>

            <div className="pt-8 flex flex-col sm:flex-row items-center justify-between gap-4 font-ui text-[11px] text-neutral-500">
              <div>
                © {new Date().getFullYear()} AcreSync Technologies. All rights reserved.
              </div>
              <div className="tracking-widest uppercase">
                Zero Custody · On-Chain Attestation · Sepolia
              </div>
            </div>
          </div>
        </footer>
      </div>
    </div>
  );
}
