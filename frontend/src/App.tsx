import { useState } from 'react'
import { Link, NavLink, Route, Routes, useLocation, useNavigate } from 'react-router-dom'
import { AnimatePresence, motion } from 'framer-motion'
import { Menu, X } from 'lucide-react'
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

const LINKS = [
  { to: '/', label: 'Schemes', end: true },
  { to: '/portfolio', label: 'Portfolio', end: false },
  { to: '/bids', label: 'My bids', end: false },
  { to: '/admin', label: 'Operators', end: false },
];

function Topbar() {
  const { signedIn, mockRole, investorLabel, signOut } = useSession()
  const nav = useNavigate()
  const [open, setOpen] = useState(false)
  return (
    <>
      <div className="fixed inset-x-0 top-0 z-[60] flex justify-center px-4 pt-4 md:px-6">
        <div className="glass flex w-full max-w-5xl items-center gap-4 rounded-full px-4 py-3 md:gap-8 md:px-6">
          <Link to="/" className="flex items-center gap-2.5">
            <svg viewBox="0 0 256 256" className="h-6 w-6 fill-white" aria-hidden="true">
              <path d="M228 0C172.772 0 128 44.772 128 100L128 0L0 0L0 28C0 83.228 44.772 128 100 128L0 128L0 256L28 256C83.228 256 128 211.228 128 156L128 256L256 256L256 228C256 172.772 211.228 128 156 128L256 128L256 0Z" />
            </svg>
            <span className="text-base font-medium text-white md:text-lg">AcreSync</span>
          </Link>
          <nav className="hidden items-center gap-6 md:flex" aria-label="Primary">
            {LINKS.map((l) => (
              <NavLink
                key={l.to}
                to={l.to}
                end={l.end}
                className={({ isActive }) => `text-sm transition ${isActive ? 'text-white' : 'text-white/60 hover:text-white'}`}
              >
                {l.label}
              </NavLink>
            ))}
          </nav>
          <div className="ml-auto hidden items-center gap-3 md:flex">
            {signedIn ? (
              <>
                <span className="max-w-44 truncate text-xs text-white/60" title="Local session">
                  {investorLabel ? `${investorLabel} · ` : ''}{mockRole || 'investor'}
                </span>
                <Link to="/me" className="text-sm text-white/70 hover:text-white">Profile</Link>
                <button
                  type="button"
                  className="rounded-full border border-white/25 px-4 py-1.5 text-sm text-white transition hover:bg-white/10"
                  onClick={() => { signOut(); nav('/'); }}
                >
                  Sign out
                </button>
              </>
            ) : (
              <Link to="/login" className="rounded-full bg-brand-mist px-5 py-1.5 text-sm font-medium text-brand-navy transition hover:scale-105">Sign in</Link>
            )}
          </div>
          <button className="ml-auto md:hidden" onClick={() => setOpen((o) => !o)} aria-label="Menu">
            {open ? <X className="h-5 w-5 text-white" /> : <Menu className="h-5 w-5 text-white" />}
          </button>
        </div>
      </div>
      <AnimatePresence>
        {open && (
          <motion.div
            initial={{ opacity: 0 }}
            animate={{ opacity: 1 }}
            exit={{ opacity: 0 }}
            className="fixed inset-0 z-[55] bg-brand-dark/95 backdrop-blur-xl md:hidden"
          >
            <div className="flex h-full flex-col items-center justify-center gap-6">
              {LINKS.map((l, i) => (
                <motion.div
                  key={l.to}
                  initial={{ opacity: 0, y: 20, filter: 'blur(4px)' }}
                  animate={{ opacity: 1, y: 0, filter: 'blur(0px)' }}
                  transition={{ delay: 0.1 + i * 0.06, duration: 0.35 }}
                >
                  <Link to={l.to} onClick={() => setOpen(false)} className="text-3xl font-medium text-white/90">{l.label}</Link>
                </motion.div>
              ))}
              {signedIn ? (
                <button className="mt-4 rounded-full border border-white/25 px-6 py-2 text-white" onClick={() => { signOut(); setOpen(false); nav('/'); }}>Sign out</button>
              ) : (
                <Link to="/login" onClick={() => setOpen(false)} className="mt-4 rounded-full bg-white px-8 py-3 font-medium text-black">Sign in</Link>
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
    <div className="min-h-screen bg-brand-dark text-white">
      <Preloader />
      <ScrollProgress />
      <Grain />
      {/* ambient background */}
      <div className="pointer-events-none fixed inset-0" aria-hidden="true">
        <div className="hero-grid absolute inset-0" />
        <div className="absolute -top-40 left-1/2 h-[480px] w-[820px] -translate-x-1/2 rounded-full bg-[#312d7c]/50 blur-[140px]" />
        <div className="absolute bottom-0 right-0 h-[300px] w-[420px] rounded-full bg-brand-blue/10 blur-[120px]" />
      </div>
      <Topbar />
      <main className="relative z-10 mx-auto w-full max-w-6xl px-5 pb-24 pt-28 sm:px-8 md:pt-32">
        <AnimatePresence mode="wait">
          <motion.div
            key={loc.pathname}
            initial={{ opacity: 0, y: 16 }}
            animate={{ opacity: 1, y: 0 }}
            exit={{ opacity: 0, y: -12 }}
            transition={{ duration: 0.35, ease: [0.25, 0.1, 0.25, 1] }}
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
              <Route path="*" element={<div className="py-20 text-center"><h1 className="text-3xl font-semibold">Not found</h1><p className="mt-3"><Link to="/" className="text-brand-mist underline">Back to schemes</Link></p></div>} />
            </Routes>
          </motion.div>
        </AnimatePresence>
      </main>
      <footer className="relative z-10 border-t border-white/10">
        <div className="mx-auto flex max-w-6xl flex-col gap-2 px-5 py-8 text-xs text-white/45 sm:px-8 md:flex-row md:items-center md:gap-6">
          <span className="font-medium text-white/70">AcreSync — the chain attests, never custodies.</span>
          <a href="https://sepolia.etherscan.io/address/0xa656a42974b40cf64f32e758abb0689a2a178391" target="_blank" rel="noreferrer" className="text-brand-mist hover:underline">
            AcreSyncScheme ↗
          </a>
          <span className="md:ml-auto">Real estate investments carry risk, including possible loss of capital. Nothing here is investment advice.</span>
        </div>
      </footer>
    </div>
  )
}
