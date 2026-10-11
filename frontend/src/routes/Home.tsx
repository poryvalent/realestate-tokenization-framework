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
import { usePoll } from '../api/usePoll';
import { pageItems } from '../api/client';
import type { Scheme } from '../api/types';
import { inr } from '../lib/format';

export default function Home() {
  const navigate = useNavigate();
  const [showSplash, setShowSplash] = useState<boolean>(true);
  const [role, setRole] = useState<'INVESTOR' | 'OPERATOR'>('INVESTOR');
  const [menuOpen, setMenuOpen] = useState(false);
  const [isMenuClosing, setIsMenuClosing] = useState(false);
  const [scrollProgress, setScrollProgress] = useState(0);

  const stageRef = useRef<HTMLDivElement>(null);

  // Poll live schemes from the API backend
  const schemesReq = usePoll<Scheme[] | { items: Scheme[] }>('/schemes', {});
  const schemes = pageItems(schemesReq.data);

  // Measure scroll progress (0 to 1) over the 900vh stage
  useEffect(() => {
    const handleScroll = () => {
      if (!stageRef.current) return;
      const stage = stageRef.current;
      const totalScrollable = stage.offsetHeight - window.innerHeight;
      if (totalScrollable <= 0) return;
      const currentScroll = window.scrollY;
      const p = Math.max(0, Math.min(1, currentScroll / totalScrollable));
      setScrollProgress(p);
    };

    window.addEventListener('scroll', handleScroll, { passive: true });
    handleScroll();
    return () => window.removeEventListener('scroll', handleScroll);
  }, []);

  // Parallax calculations for the giant "ACRESYNC" watermark
  // Maps over 0.00 - 0.16
  const pWatermark = Math.min(1, scrollProgress / 0.16);
  const watermarkScale = 1 + pWatermark * 0.45;
  const watermarkTranslateY = -pWatermark * 60;
  const watermarkOpacity = 1 - pWatermark * 0.6;

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
    <div className="relative w-full bg-ink text-white selection:bg-amber-500 selection:text-ink font-geist">
      {/* ========================================================================= */}
      {/* SECTION 1: SPLASH SCREEN (plays intro video once) */}
      {/* ========================================================================= */}
      {showSplash && <SplashScreen onComplete={() => setShowSplash(false)} />}

      {/* ========================================================================= */}
      {/* SECTION 2: HERO SECTION (Fixed background, watermark, navbars) */}
      {/* ========================================================================= */}
      <section className="fixed inset-0 h-screen w-full overflow-hidden z-0 bg-ink pointer-events-auto flex flex-col justify-between">
        {/* 2A. Looping Background Video */}
        <div className="absolute inset-0 z-0">
          <video
            autoPlay
            muted
            loop
            playsInline
            className="w-full h-full object-cover"
          >
            <source src="/videos/hero.mp4" type="video/mp4" />
            <source src="https://d3u0tzju9qaucj.cloudfront.net/fe1f1a5c-425f-4f81-b26b-c5ba3efab5ec/384db88a-5918-410b-bd25-da17893a1e87.mp4" type="video/mp4" />
          </video>
          {/* Legibility Gradient */}
          <div className="absolute inset-0 bg-gradient-to-b from-ink/70 via-ink/20 to-ink/80" />
        </div>

        {/* 2B. Giant "ACRESYNC" Parallax Watermark */}
        <div className="absolute inset-0 flex items-center justify-center pointer-events-none select-none z-[5]">
          <span
            className="font-haboro uppercase text-white/[0.07] leading-none tracking-[0.05em] whitespace-nowrap text-[22vw]"
            style={{
              transform: `translateY(${watermarkTranslateY}px) scale(${watermarkScale})`,
              opacity: watermarkOpacity,
              WebkitTextStroke: '1px rgba(255,255,255,0.18)',
              willChange: 'transform, opacity',
            }}
          >
            ACRESYNC
          </span>
        </div>

        {/* 2C. Top Navbar */}
        <header className="relative z-10 flex items-center justify-between px-4 sm:px-6 md:px-12 pt-4 sm:pt-6 md:pt-8 pb-4 border-b border-white/10">
          {/* Left: Logo */}
          <Link
            to="/"
            className="font-haboro text-white text-lg sm:text-xl md:text-2xl tracking-[0.2em] uppercase hover:opacity-90 transition-opacity"
          >
            ACRESYNC
          </Link>

          {/* Center: Navigation Links */}
          <nav className="hidden md:flex items-center gap-10">
            <a
              href="#schemes"
              className="font-geist text-white/70 hover:text-white text-xs tracking-[0.2em] uppercase transition-colors duration-200"
            >
              SCHEMES
            </a>
            <Link
              to="/offers/4d7c3397-023e-4e1d-a328-a2af07fca928/proofs"
              className="font-geist text-white/70 hover:text-white text-xs tracking-[0.2em] uppercase transition-colors duration-200"
            >
              PROOFS
            </Link>
          </nav>

          {/* Right: Square Segmented Role Switcher */}
          <div className="hidden sm:flex border border-white/20 rounded-none bg-ink/60 backdrop-blur-md">
            <button
              type="button"
              onClick={() => setRole('INVESTOR')}
              className={`px-4 md:px-5 py-2 md:py-2.5 text-[11px] md:text-xs font-geist font-medium tracking-[0.18em] uppercase transition-colors duration-200 rounded-none ${
                role === 'INVESTOR'
                  ? 'bg-white text-ink'
                  : 'text-white/70 hover:text-white'
              }`}
            >
              INVESTOR
            </button>
            <button
              type="button"
              onClick={() => setRole('OPERATOR')}
              className={`px-4 md:px-5 py-2 md:py-2.5 text-[11px] md:text-xs font-geist font-medium tracking-[0.18em] uppercase transition-colors duration-200 rounded-none ${
                role === 'OPERATOR'
                  ? 'bg-white text-ink'
                  : 'text-white/70 hover:text-white'
              }`}
            >
              OPERATOR
            </button>
          </div>
        </header>

        {/* 2D. Hero Heading (centered, above the watermark) */}
        <div className="relative z-10 flex-1 flex flex-col items-center justify-center px-4 sm:px-6 text-center">
          {/* Eyebrow */}
          <div className="font-geist text-[11px] sm:text-xs tracking-[0.3em] uppercase text-white/60 mb-6 flex items-center gap-3">
            <span className="w-1.5 h-1.5 bg-amber-500 rounded-none" />
            LIVE INSTITUTIONAL RAILS
          </div>

          {/* Main Title */}
          <h1 className="font-haboro text-white text-center text-2xl sm:text-3xl md:text-5xl lg:text-6xl xl:text-7xl leading-[1.1] tracking-wide uppercase max-w-5xl">
            INSTITUTIONAL REAL ESTATE,
            <br />
            TOKENIZED UNDER SEBI SM-REIT
          </h1>

          {/* Role-Dependent Subline */}
          <p className="mt-6 font-geist font-light text-white/70 text-sm sm:text-base text-center max-w-xl">
            {role === 'INVESTOR'
              ? 'Bid on live SM-REIT offerings with verified ASBA bank liens.'
              : 'Structure SPV schemes under Axis Trustee statutory oversight.'}
          </p>

          {/* Action CTAs */}
          <div className="mt-8 flex flex-wrap items-center justify-center gap-4">
            <a
              href="#schemes"
              className="px-6 py-3 bg-white text-ink text-xs font-semibold uppercase tracking-[0.2em] rounded-none hover:bg-neutral-200 transition-colors duration-200"
            >
              EXPLORE OFFERINGS
            </a>
            <Link
              to={role === 'OPERATOR' ? '/admin' : '/login'}
              className="px-6 py-3 border border-white/25 text-white text-xs font-semibold uppercase tracking-[0.2em] rounded-none hover:border-white transition-colors duration-200"
            >
              {role === 'OPERATOR' ? 'OPERATOR CONSOLE' : 'ACCESS PORTAL'}
            </Link>
          </div>
        </div>

        {/* 2E. Bottom Morphing Navbar */}
        <div className="relative z-10 flex justify-center pb-4 sm:pb-6 md:pb-10">
          <div
            onClick={handleToggleMenu}
            className={`cursor-pointer bg-ink border border-white/15 rounded-none flex items-center justify-between transition-all duration-500 ease-[cubic-bezier(0.16,1,0.3,1)] select-none shadow-2xl ${
              menuOpen
                ? 'w-14 h-14 p-0 justify-center'
                : 'w-[280px] max-w-[85vw] h-14 px-6'
            }`}
          >
            {menuOpen ? (
              // Collapsed state: X close button
              <div className="relative w-5 h-5 flex items-center justify-center">
                <span className="absolute w-5 h-[1.5px] bg-white rotate-45" />
                <span className="absolute w-5 h-[1.5px] bg-white -rotate-45" />
              </div>
            ) : (
              // Expanded state: Building2, CONSOLE, Hamburger
              <>
                <div className="flex items-center gap-3">
                  <Building2 className="w-5 h-5 text-amber-500" />
                  <span className="font-geist text-white text-sm tracking-[0.15em] uppercase font-medium">
                    CONSOLE
                  </span>
                </div>
                {/* Two horizontal white lines */}
                <div className="relative w-6 h-6">
                  <span className="absolute top-[8px] left-0 w-6 h-[1.5px] bg-white" />
                  <span className="absolute top-[15px] left-0 w-6 h-[1.5px] bg-white" />
                </div>
              </>
            )}
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
            className={`fixed z-30 left-1/2 bottom-[80px] sm:bottom-[100px] md:bottom-[120px] w-[92vw] max-w-[480px] bg-ink border border-white/15 rounded-none p-6 sm:p-8 md:p-10 shadow-2xl ${
              isMenuClosing ? 'menu-close' : 'menu-open'
            }`}
          >
            {/* Header */}
            <div className="font-geist text-neutral-400 text-xs tracking-[0.2em] uppercase mb-6 border-b border-white/10 pb-3 flex justify-between items-center">
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
                      className="font-haboro text-white text-2xl md:text-3xl lg:text-4xl hover:text-neutral-300 transition-colors duration-200 block py-1 uppercase"
                    >
                      {item.label}
                    </Link>
                  ) : (
                    <a
                      href={item.href}
                      onClick={handleCloseMenu}
                      className="font-haboro text-white text-2xl md:text-3xl lg:text-4xl hover:text-neutral-300 transition-colors duration-200 block py-1 uppercase"
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
                <span className="font-geist text-neutral-400 text-sm">Trustee</span>
                <span className="font-geist text-white text-sm font-medium">
                  Axis Trustee
                </span>
              </div>
              <div className="flex justify-between items-center">
                <span className="font-geist text-neutral-400 text-sm">Escrow</span>
                <span className="font-geist text-white text-sm font-medium">
                  100% ASBA bank lien
                </span>
              </div>
              <div className="flex justify-between items-center">
                <span className="font-geist text-neutral-400 text-sm">Network</span>
                <span className="font-geist text-white text-sm font-medium">
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
              className="w-full mt-6 bg-white text-ink py-3 rounded-none font-geist text-sm font-medium tracking-[0.15em] uppercase hover:bg-neutral-100 transition-colors duration-200"
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
        <ScrollGallery
          progress={scrollProgress}
          visible={scrollProgress <= 1.05}
        />
      </div>

      {/* ========================================================================= */}
      {/* SECTION 4: CONTENT BLOCK (Metrics, Expandable Cards, Schemes, Footer) */}
      {/* ========================================================================= */}
      <div className="relative z-20 w-full bg-ink text-white border-t border-white/10">
        {/* 4A. Institutional Metrics Bar */}
        <section className="w-full border-b border-white/10 bg-[#050c1f]">
          <div className="max-w-7xl mx-auto px-6 py-16 grid grid-cols-1 sm:grid-cols-2 lg:grid-cols-4 gap-8">
            <div className="border border-white/10 p-8 rounded-none bg-ink/40">
              <div className="font-mono text-xs uppercase tracking-[0.25em] text-neutral-400 mb-3 flex items-center gap-2">
                <Landmark className="w-4 h-4 text-amber-500" />
                SEBI TICKET SIZE
              </div>
              <div className="font-haboro text-3xl sm:text-4xl text-white font-bold tracking-tight">
                ₹10,00,000
              </div>
              <div className="mt-2 text-xs text-neutral-400 font-geist">
                Statutory unitholder threshold under SM-REIT guidelines.
              </div>
            </div>

            <div className="border border-white/10 p-8 rounded-none bg-ink/40">
              <div className="font-mono text-xs uppercase tracking-[0.25em] text-neutral-400 mb-3 flex items-center gap-2">
                <ShieldCheck className="w-4 h-4 text-emerald-400" />
                BANK LIEN ESCROW
              </div>
              <div className="font-haboro text-3xl sm:text-4xl text-white font-bold tracking-tight">
                100% ASBA
              </div>
              <div className="mt-2 text-xs text-neutral-400 font-geist">
                Funds blocked in investor bank account until statutory allotment.
              </div>
            </div>

            <div className="border border-white/10 p-8 rounded-none bg-ink/40">
              <div className="font-mono text-xs uppercase tracking-[0.25em] text-neutral-400 mb-3 flex items-center gap-2">
                <Lock className="w-4 h-4 text-blue-400" />
                CUSTODY RISK
              </div>
              <div className="font-haboro text-3xl sm:text-4xl text-white font-bold tracking-tight">
                0.00
              </div>
              <div className="mt-2 text-xs text-neutral-400 font-geist">
                Pure non-custodial attestation. Zero platform fund holding.
              </div>
            </div>

            <div className="border border-white/10 p-8 rounded-none bg-ink/40">
              <div className="font-mono text-xs uppercase tracking-[0.25em] text-neutral-400 mb-3 flex items-center gap-2">
                <Coins className="w-4 h-4 text-purple-400" />
                AUDITABILITY
              </div>
              <div className="font-haboro text-3xl sm:text-4xl text-white font-bold tracking-tight">
                SHA-256
              </div>
              <div className="mt-2 text-xs text-neutral-400 font-geist">
                Cryptographic Merkle tree anchored on Ethereum Sepolia.
              </div>
            </div>
          </div>
        </section>

        {/* 4B. 3 Expandable Service Banner Cards (findrealestate style) */}
        <section className="w-full py-24 sm:py-32 border-b border-white/10">
          <div className="max-w-7xl mx-auto px-6 sm:px-10">
            <div className="grid grid-cols-1 lg:grid-cols-12 gap-8 items-end mb-16">
              <div className="lg:col-span-4 font-mono text-xs uppercase tracking-[0.25em] text-neutral-400">
                ECOSYSTEM RAILS
              </div>
              <div className="lg:col-span-8">
                <h2 className="font-haboro text-3xl sm:text-5xl font-bold tracking-tight text-white leading-[1.1] uppercase">
                  Institutional Architecture
                </h2>
              </div>
            </div>

            <div className="space-y-4">
              {[
                {
                  num: '01',
                  title: 'Invest & Bid Smarter',
                  desc: 'Bid in live SEBI SM-REIT offerings with verified ASBA bank lien. One bid per investor, mathematical lottery ballot, and automated excess fund release.',
                  badge: 'ASBA Escrow Rail',
                  link: '/offers/4d7c3397-023e-4e1d-a328-a2af07fca928/bid',
                  image:
                    'https://images.unsplash.com/photo-1486406146926-c627a92ad1ab?auto=format&fit=crop&w=2400&q=85',
                },
                {
                  num: '02',
                  title: 'Structure & Sponsor Schemes',
                  desc: 'Institutional REIT managers structure SPV schemes, publish certified valuer reports to IPFS, and execute compliant offerings with Axis Trustee statutory oversight.',
                  badge: 'Operator Console',
                  link: '/admin',
                  image:
                    'https://images.unsplash.com/photo-1541888946425-d0fbb1861593?auto=format&fit=crop&w=2000&q=80',
                },
                {
                  num: '03',
                  title: 'Verify & Audit Cryptographic Proofs',
                  desc: 'Open cryptographic proofs for any unitholder, regulator, or auditor. Recompute SHA-256 Merkle branches directly against Ethereum Sepolia on-chain shadow ledger contracts.',
                  badge: 'Merkle Shadow Ledger',
                  link: '/offers/4d7c3397-023e-4e1d-a328-a2af07fca928/proofs',
                  image:
                    'https://images.unsplash.com/photo-1507679799987-c73779587ccf?auto=format&fit=crop&w=1800&q=80',
                },
              ].map((card) => (
                <Link
                  key={card.num}
                  to={card.link}
                  className="group relative block w-full overflow-hidden border border-white/12 bg-[#050c1f] p-8 sm:p-12 transition-all duration-500 hover:border-white/40 rounded-none"
                >
                  <img
                    src={card.image}
                    alt={card.title}
                    className="absolute inset-0 h-full w-full object-cover opacity-15 grayscale transition-all duration-700 group-hover:scale-105 group-hover:opacity-30 group-hover:grayscale-0 rounded-none pointer-events-none"
                  />
                  <div className="absolute inset-0 bg-gradient-to-r from-[#030712] via-[#030712]/90 to-transparent pointer-events-none" />

                  <div className="relative z-10 flex flex-col justify-between min-h-[180px]">
                    <div className="flex items-center justify-between">
                      <div className="flex items-center gap-4">
                        <span className="font-haboro text-3xl sm:text-4xl text-amber-500 font-bold">
                          {card.num}
                        </span>
                        <span className="font-mono text-xs uppercase tracking-[0.2em] text-neutral-400 border border-white/10 px-3 py-1 rounded-none bg-black/40">
                          {card.badge}
                        </span>
                      </div>
                      <div className="flex h-10 w-10 items-center justify-center border border-white/20 bg-white/5 transition-colors group-hover:bg-white group-hover:text-ink rounded-none">
                        <ArrowRight className="h-5 w-5" />
                      </div>
                    </div>

                    <div className="mt-8 max-w-2xl">
                      <h3 className="font-haboro text-2xl sm:text-3xl font-bold tracking-tight text-white group-hover:text-amber-400 transition-colors uppercase">
                        {card.title}
                      </h3>
                      <p className="mt-3 font-geist text-sm sm:text-base text-neutral-300 leading-relaxed">
                        {card.desc}
                      </p>
                    </div>
                  </div>
                </Link>
              ))}
            </div>
          </div>
        </section>

        {/* 4C. Live Scheme Offerings Grid */}
        <section id="schemes" className="w-full py-24 sm:py-32 border-b border-white/10">
          <div className="max-w-7xl mx-auto px-6 sm:px-10">
            <div className="flex flex-col md:flex-row md:items-end justify-between mb-16 gap-6">
              <div>
                <div className="font-mono text-xs uppercase tracking-[0.25em] text-neutral-400 mb-3">
                  SEBI APPROVED PIPELINE
                </div>
                <h2 className="font-haboro text-3xl sm:text-5xl font-bold uppercase tracking-tight text-white">
                  Live Scheme Offerings
                </h2>
              </div>
              <div className="font-geist text-sm text-neutral-400 max-w-md">
                Institutional Grade-A real estate assets structured into SEBI SM-REIT schemes with statutory valuer disclosures.
              </div>
            </div>

            <div className="grid grid-cols-1 md:grid-cols-2 lg:grid-cols-3 gap-8">
              {schemes.length > 0 ? (
                schemes.map((s) => (
                  <div
                    key={s.id}
                    className="border border-white/15 bg-[#050c1f] p-8 flex flex-col justify-between rounded-none hover:border-white/40 transition-colors"
                  >
                    <div>
                      <div className="flex justify-between items-start mb-6">
                        <span className="font-mono text-[10px] tracking-widest text-amber-500 uppercase border border-amber-500/30 px-2 py-0.5 rounded-none">
                          SM-REIT SCHEME
                        </span>
                        <span className="font-mono text-[11px] text-neutral-400">
                          {typeof s.symbol === 'string' ? s.symbol : 'REIT-01'}
                        </span>
                      </div>
                      <h3 className="font-haboro text-2xl font-bold text-white uppercase mb-2">
                        {s.name}
                      </h3>
                      <p className="font-geist text-xs text-neutral-400 leading-relaxed mb-6">
                        {typeof s.description === 'string'
                          ? s.description
                          : 'Premium commercial asset held under registered Special Purpose Vehicle (SPV) with Axis Trustee statutory oversight.'}
                      </p>
                      <div className="space-y-2 py-4 border-y border-white/10 text-xs">
                        <div className="flex justify-between">
                          <span className="text-neutral-400">Target Corpus</span>
                          <span className="font-mono font-medium text-white">
                            {inr(s.targetCorpusPaise || 4800000000)}
                          </span>
                        </div>
                        <div className="flex justify-between">
                          <span className="text-neutral-400">Trustee</span>
                          <span className="text-white">Axis Trustee Services</span>
                        </div>
                        <div className="flex justify-between">
                          <span className="text-neutral-400">Escrow Mechanism</span>
                          <span className="text-emerald-400 font-mono">
                            ASBA Direct Lien
                          </span>
                        </div>
                      </div>
                    </div>

                    <div className="mt-8 flex gap-3">
                      <Link
                        to={`/offers/4d7c3397-023e-4e1d-a328-a2af07fca928/bid`}
                        className="flex-1 py-3 bg-white text-ink text-center text-xs font-semibold uppercase tracking-[0.15em] rounded-none hover:bg-neutral-200 transition-colors"
                      >
                        PLACE ASBA BID
                      </Link>
                      <Link
                        to={`/schemes/${s.id}`}
                        className="p-3 border border-white/20 text-white flex items-center justify-center rounded-none hover:border-white transition-colors"
                        title="View Scheme Details"
                      >
                        <ExternalLink className="w-4 h-4" />
                      </Link>
                    </div>
                  </div>
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
                  <div
                    key={mock.title}
                    className="border border-white/15 bg-[#050c1f] p-8 flex flex-col justify-between rounded-none hover:border-white/40 transition-colors"
                  >
                    <div>
                      <div className="flex justify-between items-start mb-6">
                        <span className="font-mono text-[10px] tracking-widest text-amber-500 uppercase border border-amber-500/30 px-2 py-0.5 rounded-none">
                          SM-REIT SPV 0{i + 1}
                        </span>
                        <span className="font-mono text-[11px] text-neutral-400">
                          {mock.irr} Projected IRR
                        </span>
                      </div>
                      <h3 className="font-haboro text-2xl font-bold text-white uppercase mb-2">
                        {mock.title}
                      </h3>
                      <p className="font-geist text-xs text-neutral-400 leading-relaxed mb-6">
                        {mock.location} — Registered SPV with Axis Trustee statutory oversight, licensed valuation reports, and zero custody rails.
                      </p>
                      <div className="space-y-2 py-4 border-y border-white/10 text-xs">
                        <div className="flex justify-between">
                          <span className="text-neutral-400">Target Corpus</span>
                          <span className="font-mono font-medium text-white">
                            {inr(mock.corpus)}
                          </span>
                        </div>
                        <div className="flex justify-between">
                          <span className="text-neutral-400">Escrow Mechanism</span>
                          <span className="text-emerald-400 font-mono">
                            ASBA Direct Lien
                          </span>
                        </div>
                        <div className="flex justify-between">
                          <span className="text-neutral-400">Audit Status</span>
                          <span className="text-white font-mono">
                            Ethereum Sepolia
                          </span>
                        </div>
                      </div>
                    </div>

                    <div className="mt-8 flex gap-3">
                      <Link
                        to="/offers/4d7c3397-023e-4e1d-a328-a2af07fca928/bid"
                        className="flex-1 py-3 bg-white text-ink text-center text-xs font-semibold uppercase tracking-[0.15em] rounded-none hover:bg-neutral-200 transition-colors"
                      >
                        PLACE ASBA BID
                      </Link>
                      <Link
                        to="/offers/4d7c3397-023e-4e1d-a328-a2af07fca928/proofs"
                        className="p-3 border border-white/20 text-white flex items-center justify-center rounded-none hover:border-white transition-colors"
                        title="View Merkle Proofs"
                      >
                        <ExternalLink className="w-4 h-4" />
                      </Link>
                    </div>
                  </div>
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
                <div className="font-haboro text-2xl font-bold uppercase tracking-[0.2em] text-white mb-4">
                  ACRESYNC
                </div>
                <p className="font-geist text-xs text-neutral-400 max-w-md leading-relaxed">
                  AcreSync operates as an institutional technological framework for SEBI SM-REIT tokenization. All bids and financial settlements execute strictly through authorized ASBA bank lien mandates with zero platform custody.
                </p>
                <div className="mt-6 flex items-center gap-4 text-xs font-mono text-neutral-400">
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
                <div className="font-mono text-xs uppercase tracking-[0.2em] text-neutral-400 mb-4">
                  PLATFORM RAILS
                </div>
                <ul className="space-y-2.5 text-xs font-geist text-neutral-300">
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
                <div className="font-mono text-xs uppercase tracking-[0.2em] text-neutral-400 mb-4">
                  LEGAL & GOVERNANCE
                </div>
                <ul className="space-y-2.5 text-xs font-geist text-neutral-300">
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

            <div className="pt-8 flex flex-col sm:flex-row items-center justify-between gap-4 font-mono text-[11px] text-neutral-500">
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
