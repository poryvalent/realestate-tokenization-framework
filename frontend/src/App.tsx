import { useEffect } from 'react'
import { Link, NavLink, Route, Routes, useLocation, useNavigate } from 'react-router-dom'
import { useSession } from './api/session'
import { ScrollProgress, Cursor, Grain, Preloader } from './components/motion'
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
  const { signedIn, mockRole, investorLabel, signOut } = useSession()
  const nav = useNavigate()
  return (
    <>
      <header className="topbar">
        <div className="topbar-inner">
          <Link className="brand" to="/">
            <b>AcreSync</b>
            <span>SM-REIT · attests, never custodies</span>
          </Link>
          <nav className="nav" aria-label="Primary">
            <NavLink to="/" end className={({ isActive }) => (isActive ? 'active' : '')}>Schemes</NavLink>
            <NavLink to="/portfolio" className={({ isActive }) => (isActive ? 'active' : '')}>Portfolio</NavLink>
            <NavLink to="/bids" className={({ isActive }) => (isActive ? 'active' : '')}>My bids</NavLink>
            <NavLink to="/admin" className={({ isActive }) => (isActive ? 'active' : '')}>Operators</NavLink>
          </nav>
          <div className="topbar-right">
            {signedIn ? (
              <>
                <span title="Local session">
                  {investorLabel ? `${investorLabel} · ` : ''}{mockRole || 'investor'}
                </span>
                <Link to="/me">Profile</Link>
                <button
                  type="button"
                  className="btn small"
                  onClick={() => {
                    signOut()
                    nav('/')
                  }}
                >
                  Sign out
                </button>
              </>
            ) : (
              <Link className="btn small" to="/login">Sign in</Link>
            )}
          </div>
        </div>
      </header>
    </>
  )
}

export default function App() {
  const loc = useLocation()
  useEffect(() => {
    window.scrollTo(0, 0)
  }, [loc.pathname])

  return (
    <>
      <Preloader />
      <ScrollProgress />
      <Cursor />
      <Grain />
      <Topbar />
      <main className="wrap route-enter" key={loc.pathname}>
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
          <Route
            path="*"
            element={
              <>
                <h1>Not found</h1>
                <p>
                  <Link to="/">Back to schemes</Link>
                </p>
              </>
            }
          />
        </Routes>
      </main>
      <footer className="footer">
        <div className="footer-inner">
          <span>AcreSync — the chain attests, never custodies.</span>
          <span>Contracts on Sepolia, source-verified, no proxy.</span>
          <a href="https://sepolia.etherscan.io/address/0xa656a42974b40cf64f32e758abb0689a2a178391" target="_blank" rel="noreferrer">
            AcreSyncScheme ↗
          </a>
          <span>Real estate investments carry risk, including possible loss of capital. Nothing here is investment advice.</span>
        </div>
      </footer>
    </>
  )
}
