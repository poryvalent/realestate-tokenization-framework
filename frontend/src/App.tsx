import { useEffect, useState } from 'react'
import { Link, NavLink, Route, Routes, useLocation, useNavigate } from 'react-router-dom'
import { AnimatePresence, motion } from 'framer-motion'
import { Menu, X, ArrowRight } from 'lucide-react'
import { useSession } from './api/session'
import { Grain, Preloader, ScrollProgress } from './components/motion'
import Home from './routes/Home'
import Scheme from './routes/Scheme'
import Offer from './routes/Offer'
import PlaceBid from './routes/PlaceBid'
import Bids from './routes/Bids'
import Ballot from './routes/Ballot'
import Allotments from './routes/Allotments'
import ProofBid from './routes/ProofBid'
import Documents from './routes/Documents'
import Period from './routes/Period'
import Portfolio from './routes/Portfolio'
import MeView from './routes/Me'
import Login from './routes/Login'
import Admin from './routes/Admin'

function Topbar() {
  const { signedIn, mockRole, investorLabel, isOperator, isInvestor, signOut } = useSession()
  const nav = useNavigate()
  const loc = useLocation()
  const [open, setOpen] = useState(false)
  const [scrolled, setScrolled] = useState(false)

  const isHome = loc.pathname === '/'

  useEffect(() => {
    const handleScroll = () => {
      setScrolled(window.scrollY > 40)
    }
    window.addEventListener('scroll', handleScroll, { passive: true })
    handleScroll()
    return () => window.removeEventListener('scroll', handleScroll)
  }, [])

  const links = isOperator
    ? [
        { to: '/', label: 'Schemes', end: true },
        { to: '/admin', label: 'Operator Console', end: false },
      ]
    : [
        { to: '/', label: 'Schemes', end: true },
        { to: '/offers/4d7c3397-023e-4e1d-a328-a2af07fca928/documents', label: 'Due Diligence', end: false },
        { to: '/offers/4d7c3397-023e-4e1d-a328-a2af07fca928/proofs', label: 'Merkle Proofs', end: false },
        { to: '/portfolio', label: 'Portfolio', end: false },
        { to: '/bids', label: 'My bids', end: false },
      ];

  const showLogo = !isHome || scrolled;

  return (
    <>
      <header
        className={`fixed inset-x-0 top-0 z-[60] transition-all duration-300 ${
          isHome
            ? scrolled
              ? 'border-b border-white/10 bg-[#050c1f]/90 py-3 shadow-[0_10px_30px_rgba(0,0,0,0.6)] backdrop-blur-xl opacity-100 pointer-events-auto'
              : 'border-b border-transparent bg-transparent py-5 opacity-0 pointer-events-none'
            : 'border-b border-white/12 bg-[#050c1f]/95 py-3 backdrop-blur-xl'
        }`}
      >
        <div className="mx-auto flex w-full max-w-7xl items-center gap-4 px-6 sm:px-10 md:gap-8">
          <Link
            to="/"
            className={`flex items-center gap-2.5 transition-all duration-300 ${
              showLogo
                ? 'opacity-100 translate-x-0 pointer-events-auto'
                : 'opacity-0 -translate-x-4 pointer-events-none'
            }`}
          >
            <div className="flex h-8 w-8 items-center justify-center rounded-full bg-white text-[#040916] shadow-sm">
              <svg viewBox="0 0 256 256" className="h-4 w-4 fill-[#040916]" aria-hidden="true">
                <path d="M228 0C172.772 0 128 44.772 128 100L128 0L0 0L0 28C0 83.228 44.772 128 100 128L0 128L0 256L28 256C83.228 256 128 211.228 128 156L128 256L256 256L256 228C256 172.772 211.228 128 156 128L256 128L256 0Z" />
              </svg>
            </div>
            <span className="text-base font-bold tracking-tight text-white uppercase">AcreSync</span>
          </Link>
          <nav className="hidden items-center gap-2 md:flex" aria-label="Primary">
            {links.map((l) => (
              <NavLink
                key={l.to}
                to={l.to}
                end={l.end}
                className={({ isActive }) =>
                  `px-3.5 py-1.5 text-xs font-semibold tracking-wider transition-all duration-200 rounded-full ${
                    isActive
                      ? 'bg-white/10 text-white border border-white/20'
                      : 'text-slate-300 hover:text-white hover:bg-white/5'
                  }`
                }
              >
                {l.label}
              </NavLink>
            ))}
          </nav>
          <div className="ml-auto hidden items-center gap-4 md:flex">
            <span className="rounded-full border border-blue-400/30 bg-blue-500/10 px-3 py-1 text-[10px] font-mono uppercase tracking-wider text-blue-300">
              SEBI SM-REIT Rail
            </span>
            {signedIn ? (
              <>
                <span className="max-w-48 truncate font-mono text-xs text-slate-400" title="Active session">
                  {isOperator ? (
                    <span className="text-blue-300 font-bold uppercase tracking-wider">
                      [{mockRole || 'OPERATOR'}]
                    </span>
                  ) : (
                    <span className="text-emerald-400 font-medium">
                      {investorLabel || 'Investor'}
                    </span>
                  )}
                </span>
                {isInvestor && (
                  <Link to="/me" className="text-xs font-bold uppercase tracking-wider text-slate-300 hover:text-white transition-colors">
                    Profile
                  </Link>
                )}
                <button
                  type="button"
                  className="rounded-full border border-white/20 bg-white/5 px-4 py-1.5 text-xs font-semibold uppercase tracking-wider text-white transition hover:bg-white/15 cursor-pointer"
                  onClick={() => { signOut(); nav('/'); }}
                >
                  Sign out
                </button>
              </>
            ) : (
              <Link
                to="/login"
                className="button-round rounded-full bg-white px-5 py-2 text-xs font-bold uppercase tracking-wider text-[#040916] transition hover:bg-slate-200 shadow-md"
              >
                <span>Sign in</span>
                <ArrowRight className="h-3.5 w-3.5" />
              </Link>
            )}
          </div>
          <button className="ml-auto md:hidden p-1.5 text-white" onClick={() => setOpen((o) => !o)} aria-label="Menu">
            {open ? <X className="h-6 w-6 text-white" /> : <Menu className="h-6 w-6 text-white" />}
          </button>
        </div>
      </header>
      <AnimatePresence>
        {open && (
          <motion.div
            initial={{ opacity: 0 }}
            animate={{ opacity: 1 }}
            exit={{ opacity: 0 }}
            className="fixed inset-0 z-[55] bg-[#050c1f]/98 backdrop-blur-2xl md:hidden"
          >
            <div className="flex h-full flex-col items-center justify-center gap-6">
              {links.map((l, i) => (
                <motion.div
                  key={l.to}
                  initial={{ opacity: 0, y: 16 }}
                  animate={{ opacity: 1, y: 0 }}
                  transition={{ delay: 0.1 + i * 0.05, duration: 0.3 }}
                >
                  <Link to={l.to} onClick={() => setOpen(false)} className="text-2xl font-bold uppercase tracking-wider text-white">{l.label}</Link>
                </motion.div>
              ))}
              {signedIn ? (
                <button
                  className="mt-4 rounded-none border border-white/30 px-6 py-2.5 text-xs font-bold uppercase tracking-wider text-white"
                  onClick={() => { signOut(); setOpen(false); nav('/'); }}
                >
                  Sign out
                </button>
              ) : (
                <Link
                  to="/login"
                  onClick={() => setOpen(false)}
                  className="mt-4 rounded-none bg-white px-8 py-3 text-xs font-bold uppercase tracking-wider text-[#040916]"
                >
                  Sign in
                </Link>
              )}
            </div>
          </motion.div>
        )}
      </AnimatePresence>
    </>
  )
}

export default function App() {
  const loc = useLocation()
  return (
    <div className="min-h-screen bg-[#030712] text-white">
      <Preloader />
      <ScrollProgress />
      <Grain />
      {/* Ambient background - Pure Navy Blue & Crisp Grid */}
      <div className="pointer-events-none fixed inset-0" aria-hidden="true">
        <div className="hero-grid absolute inset-0" />
        <div className="absolute -top-40 left-1/2 h-[500px] w-[860px] -translate-x-1/2 bg-[#1e3a8a]/20 blur-[150px]" />
        <div className="absolute bottom-0 right-0 h-[350px] w-[450px] bg-[#1d4ed8]/15 blur-[130px]" />
      </div>
      <Topbar />
      <main className={`relative z-10 w-full ${loc.pathname === '/' ? 'pb-0 pt-0' : 'mx-auto max-w-6xl px-5 pb-24 pt-24 sm:px-8 md:pt-28'}`}>
        <AnimatePresence mode="wait">
          <motion.div
            key={loc.pathname}
            initial={{ opacity: 0, y: 12 }}
            animate={{ opacity: 1, y: 0 }}
            exit={{ opacity: 0, y: -8 }}
            transition={{ duration: 0.3, ease: [0.25, 0.1, 0.25, 1] }}
          >
            <Routes location={loc}>
              <Route path="/" element={<Home />} />
              <Route path="/schemes/:id" element={<Scheme />} />
              <Route path="/offers/:id" element={<Offer />} />
              <Route path="/offers/:id/bid" element={<PlaceBid />} />
              <Route path="/offers/:id/ballot" element={<Ballot />} />
              <Route path="/offers/:id/allotments" element={<Allotments />} />
              <Route path="/offers/:id/documents" element={<Documents />} />
              <Route path="/offers/:id/proofs" element={<ProofBid />} />
              <Route path="/periods/:id" element={<Period />} />
              <Route path="/portfolio" element={<Portfolio />} />
              <Route path="/bids" element={<Bids />} />
              <Route path="/me" element={<MeView />} />
              <Route path="/login" element={<Login />} />
              <Route path="/admin" element={<Admin />} />
              <Route path="/admin/:offerId" element={<Admin />} />
              <Route path="*" element={<div className="py-20 text-center"><h1 className="text-3xl font-bold uppercase tracking-wider">Not found</h1><p className="mt-3"><Link to="/" className="text-white underline font-semibold">Back to schemes</Link></p></div>} />
            </Routes>
          </motion.div>
        </AnimatePresence>
      </main>
      {loc.pathname !== '/' && (
        <footer className="relative z-10 border-t border-white/10 bg-[#050c1f]">
          <div className="mx-auto flex max-w-6xl flex-col gap-2 px-5 py-8 text-xs text-slate-400 sm:px-8 md:flex-row md:items-center md:gap-6">
            <span className="font-semibold text-white">AcreSync — the chain attests, never custodies.</span>
            <a href="https://sepolia.etherscan.io/address/0xa656a42974b40cf64f32e758abb0689a2a178391" target="_blank" rel="noreferrer" className="text-blue-300 hover:text-white underline">
              AcreSyncScheme Sepolia ↗
            </a>
            <span className="md:ml-auto">Real estate investments carry risk, including possible loss of capital. Institutional compliance under SEBI SM-REIT.</span>
          </div>
        </footer>
      )}
    </div>
  )
}
