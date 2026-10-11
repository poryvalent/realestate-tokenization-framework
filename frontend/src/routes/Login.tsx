import { useState, type ChangeEvent, type FormEvent } from 'react';
import { useNavigate } from 'react-router-dom';
import {
  Ripple,
  TechOrbitDisplay,
  BoxReveal,
  Input,
  Label,
  type ProofChip,
} from '@/components/ui/modern-animated-sign-in';
import { useSession, type OperatorRole } from '../api/session';
import { supabase, isSupabaseConfigured } from '../api/supabase';
import { useToast } from '../components/Toast';
import {
  Eye,
  EyeOff,
  Sparkles,
  ShieldCheck,
  Landmark,
  Lock,
  Database,
  Layers,
  KeyRound,
  User,
  Shield,
  ArrowRight,
} from 'lucide-react';

const DEMO_INVESTOR_TOKEN =
  'eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiJkZXZ8YTdlYjkxYmEtZmNlOS00YzM2LTg4NTAtNzc5OTY3OWZkYjJjIiwia25kIjoiSU5WRVNUT1IiLCJpbnYiOiJhN2ViOTFiYS1mY2U5LTRjMzYtODg1MC03Nzk5Njc5ZmRiMmMiLCJpYXQiOjE3OTE3MDU2NjYsImV4cCI6MTc5MjMxMDQ2Nn0.yAKoFtUVx6i8oJgLy8iSEkC__zFF3OVTjyh0jjFBlzE';

const DEMO_LIVE_BIDDER_TOKEN =
  'eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiJkZXZ8ODQyOGUzZWMtNjhkYy00MTNkLTg5NDQtMzBkYWVlOThjZGY4Iiwia25kIjoiSU5WRVNUT1IiLCJpbnYiOiI4NDI4ZTNlYy02OGRjLTQxM2QtODk0NC0zMGRhZWU5OGNkZjgiLCJpYXQiOjE3OTE3MTkyMzYsImV4cCI6MTgyMzI1NTIzNn0.UumMI8jlC_FMmY0iSdMqW1LDXAdHSTNsV_GDdXYsl_0';

const DEMO_MANAGER_TOKEN =
  'eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiJkZXZ8TUFOQUdFUiIsImtuZCI6Ik9QRVJBVE9SIiwicm9sIjoiTUFOQUdFUiIsImlhdCI6MTc5MTcwNTcxMSwiZXhwIjoxNzkyMzEwNTExfQ.G6yXhmI2ZJHsvi16O4Epmu4ebQ71JgRu6vAO6FEio7U';

const DEMO_TRUSTEE_TOKEN =
  'eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiJkZXZ8VFJVU1RFRSIsImtuZCI6Ik9QRVJBVE9SIiwicm9sIjoiVFJVU1RFRSIsImlhdCI6MTc5MTcwNzI3MiwiZXhwIjoxNzkyMzEyMDcyfQ.mdedXX54zVBc5LUXlW48s8h6k5LLhpyK5sOXt08i4qc';

const proofChips: ProofChip[] = [
  // Orbit 1: Inner ring (radius: 110, duration: 26s, 2 chips spaced 180° apart)
  {
    label: 'SEBI Regulated',
    sub: 'SM-REIT Framework',
    icon: ShieldCheck,
    radius: 110,
    duration: 26,
    delay: 0,
    reverse: false,
  },
  {
    label: 'ASBA Escrow',
    sub: 'Zero Custody Rails',
    icon: Landmark,
    radius: 110,
    duration: 26,
    delay: 13, // 180° phase offset
    reverse: false,
  },
  // Orbit 2: Outer ring (radius: 180, duration: 36s, 3 chips spaced 120° apart)
  {
    label: 'SHA-256 Digest',
    sub: 'Merkle Commit',
    icon: Lock,
    radius: 180,
    duration: 36,
    delay: 0,
    reverse: true,
  },
  {
    label: 'Sepolia Rollup',
    sub: 'On-Chain Ledger Anchor',
    icon: Layers,
    radius: 180,
    duration: 36,
    delay: 12, // 120° phase offset
    reverse: true,
  },
  {
    label: 'DPDP Privacy Key',
    sub: 'KMS Salted Identity',
    icon: KeyRound,
    radius: 180,
    duration: 36,
    delay: 24, // 240° phase offset
    reverse: true,
  },
];

export default function Login() {
  const { signIn } = useSession();
  const toast = useToast();
  const nav = useNavigate();

  const [authMode, setAuthMode] = useState<'investor' | 'operator'>('investor');
  const [isSignUp, setIsSignUp] = useState(false);
  const [email, setEmail] = useState('demo.investor@acresync.io');
  const [password, setPassword] = useState('password123');
  const [fullName, setFullName] = useState('Ananya Sharma');
  const [visible, setVisible] = useState(false);
  const [loading, setLoading] = useState(false);

  // Operator selection
  const [operatorRole, setOperatorRole] = useState<OperatorRole>('MANAGER');
  const [operatorToken, setOperatorToken] = useState(DEMO_MANAGER_TOKEN);

  const handleSubmit = async (e: FormEvent<HTMLFormElement>) => {
    e.preventDefault();
    setLoading(true);

    try {
      if (authMode === 'operator') {
        signIn(operatorToken.trim(), {
          persona: 'operator',
          mockRole: operatorRole,
          investorLabel: `${operatorRole} Operator`,
          authSource: 'devtoken',
        });
        toast(`Authenticated as ${operatorRole}.`);
        nav('/admin');
        return;
      }

      // Investor authentication via Supabase
      if (isSupabaseConfigured && supabase) {
        if (isSignUp) {
          const { data, error } = await supabase.auth.signUp({
            email,
            password,
            options: {
              data: { full_name: fullName, investor_class: 'RESIDENT_INDIVIDUAL' },
            },
          });
          if (error) throw error;
          if (data.session) {
            signIn(data.session.access_token, {
              persona: 'investor',
              mockRole: '',
              investorLabel: fullName || email.split('@')[0],
              authSource: 'supabase',
              user: data.user ?? undefined,
            });
            toast('Account created with Supabase. Welcome to AcreSync!');
            nav('/portfolio');
            return;
          }
          toast('Verification link sent. Check your inbox.');
        } else {
          const { data, error } = await supabase.auth.signInWithPassword({
            email,
            password,
          });
          if (error) throw error;
          if (data.session) {
            signIn(data.session.access_token, {
              persona: 'investor',
              mockRole: '',
              investorLabel: data.user.user_metadata?.full_name || email.split('@')[0],
              authSource: 'supabase',
              user: data.user,
            });
            toast('Signed in via Supabase.');
            nav('/portfolio');
            return;
          }
        }
      } else {
        // Local showcase fallback connected to local seed investor
        signIn(DEMO_INVESTOR_TOKEN, {
          persona: 'investor',
          mockRole: '',
          investorLabel: fullName || email.split('@')[0],
          authSource: 'supabase',
        });
        toast(`Signed in as ${fullName || email} (Verified Profile).`);
        nav('/portfolio');
      }
    } catch (err: unknown) {
      const msg = err instanceof Error ? err.message : 'Sign in failed';
      toast(msg);
    } finally {
      setLoading(false);
    }
  };

  const handleInstantDemo = () => {
    signIn(DEMO_INVESTOR_TOKEN, {
      persona: 'investor',
      mockRole: '',
      investorLabel: 'Ananya Sharma (Verified)',
      authSource: 'devtoken',
    });
    toast('Signed in as Showcase Investor (2 units, ₹34,200 payout).');
    nav('/portfolio');
  };

  const handleLiveBidderDemo = () => {
    signIn(DEMO_LIVE_BIDDER_TOKEN, {
      persona: 'investor',
      mockRole: '',
      investorLabel: 'Rohan Verma (Ready to Bid)',
      authSource: 'devtoken',
    });
    toast('Signed in as Rohan Verma. Ready to bid on the live offer!');
    nav('/offers/4d7c3397-023e-4e1d-a328-a2af07fca928/bid');
  };

  return (
    <div className="relative mx-auto max-w-5xl">
      {/* Ambient Top Glow - Pure Navy */}
      <div
        className="pointer-events-none absolute -top-24 left-1/2 h-96 w-96 -translate-x-1/2 bg-[#1e40af]/25 blur-[140px]"
        aria-hidden="true"
      />

      {/* Main Curved Split Chassis */}
      <div className="relative overflow-hidden rounded-3xl border border-white/15 bg-[#050c1f]/95 shadow-[0_30px_90px_rgba(0,0,0,0.8)] backdrop-blur-3xl">
        <div className="grid grid-cols-1 lg:grid-cols-2">
          {/* Left Side: Proof Radar & Attestation Orbit */}
          <div className="relative hidden lg:flex flex-col items-center justify-center border-r border-white/10 p-8 xl:p-10 bg-gradient-to-b from-[#081533] via-[#050c1f] to-[#030712]">
            <Ripple mainCircleSize={110} numCircles={5} />
            <TechOrbitDisplay chips={proofChips} />

            {/* Bottom Proof Trust Banner */}
            <div className="relative z-10 mt-3 flex items-center gap-2 rounded-full border border-emerald-400/30 bg-emerald-500/10 px-4 py-1.5 text-xs font-mono uppercase tracking-wider text-emerald-300 backdrop-blur-md">
              <span className="h-2 w-2 rounded-full bg-emerald-400 animate-pulse" />
              <span>The chain attests · It never custodies</span>
            </div>
          </div>

          {/* Right Side: Authentication Terminal */}
          <div className="flex flex-col justify-center p-7 sm:p-9 lg:p-10">
            {/* Persona Switcher Pill */}
            <div className="mb-6 flex rounded-2xl border border-white/15 bg-[#030816] p-1.5">
              <button
                type="button"
                onClick={() => setAuthMode('investor')}
                className={`flex flex-1 items-center justify-center gap-2 rounded-xl py-2.5 text-xs font-bold uppercase tracking-wider transition-all duration-200 cursor-pointer ${
                  authMode === 'investor'
                    ? 'bg-white text-[#040916] shadow-md'
                    : 'text-slate-400 hover:text-white hover:bg-white/5'
                }`}
              >
                <User className="h-3.5 w-3.5" />
                <span>Unitholder Portal</span>
              </button>
              <button
                type="button"
                onClick={() => setAuthMode('operator')}
                className={`flex flex-1 items-center justify-center gap-2 rounded-xl py-2.5 text-xs font-bold uppercase tracking-wider transition-all duration-200 cursor-pointer ${
                  authMode === 'operator'
                    ? 'bg-white text-[#040916] shadow-md'
                    : 'text-slate-400 hover:text-white hover:bg-white/5'
                }`}
              >
                <Shield className="h-3.5 w-3.5" />
                <span>Operator Console</span>
              </button>
            </div>

            {/* Header Titles */}
            <BoxReveal boxColor="#ffffff" duration={0.3}>
              <h2 className="text-gradient font-sans text-2xl sm:text-3xl font-bold uppercase tracking-tight">
                {authMode === 'investor'
                  ? isSignUp
                    ? 'Create Unitholder Profile'
                    : 'Welcome back'
                  : 'Operator Terminal'}
              </h2>
            </BoxReveal>

            <p className="mt-1.5 text-sm text-slate-300">
              {authMode === 'investor'
                ? 'Access allotted holdings, verifiable ballot roots, and bank distributions.'
                : 'Role-separated cryptographic authorization for the SEBI SM-REIT lifecycle.'}
            </p>

            {/* Supabase Status & Register Toggle */}
            {authMode === 'investor' && (
              <div className="mt-4 mb-2 flex items-center justify-between border-b border-white/10 pb-3 text-xs">
                <span className="inline-flex items-center gap-1.5 font-mono text-[11px] text-blue-300">
                  <span className="h-1.5 w-1.5 rounded-none bg-emerald-400 animate-pulse" />
                  <Database className="h-3.5 w-3.5 text-blue-300" />
                  {isSupabaseConfigured ? 'Supabase Vault Active' : 'Supabase Identity Escrow'}
                </span>
                <button
                  type="button"
                  onClick={() => setIsSignUp(!isSignUp)}
                  className="font-bold uppercase tracking-wider text-white underline underline-offset-4 hover:text-slate-300 cursor-pointer transition-colors"
                >
                  {isSignUp ? 'Existing unitholder? Sign in' : 'New investor? Register'}
                </button>
              </div>
            )}

            {/* Form Fields */}
            <form onSubmit={handleSubmit} className="mt-4 space-y-4">
              {authMode === 'investor' ? (
                <>
                  {isSignUp && (
                    <div className="flex flex-col gap-1.5">
                      <Label htmlFor="fullName">Legal Full Name (as per PAN)</Label>
                      <Input
                        id="fullName"
                        type="text"
                        placeholder="Ananya Sharma"
                        value={fullName}
                        onChange={(e: ChangeEvent<HTMLInputElement>) => setFullName(e.target.value)}
                        required
                      />
                    </div>
                  )}

                  <div className="flex flex-col gap-1.5">
                    <Label htmlFor="email">Email address</Label>
                    <Input
                      id="email"
                      type="email"
                      placeholder="investor@acresync.io"
                      value={email}
                      onChange={(e: ChangeEvent<HTMLInputElement>) => setEmail(e.target.value)}
                      required
                    />
                  </div>

                  <div className="flex flex-col gap-1.5">
                    <Label htmlFor="password">Password</Label>
                    <div className="relative">
                      <Input
                        id="password"
                        type={visible ? 'text' : 'password'}
                        placeholder="••••••••••••"
                        value={password}
                        onChange={(e: ChangeEvent<HTMLInputElement>) => setPassword(e.target.value)}
                        required
                      />
                      <button
                        type="button"
                        onClick={() => setVisible(!visible)}
                        className="absolute inset-y-0 right-0 flex items-center pr-3.5 text-slate-400 hover:text-white cursor-pointer transition-colors"
                      >
                        {visible ? <Eye className="h-4 w-4" /> : <EyeOff className="h-4 w-4" />}
                      </button>
                    </div>
                  </div>
                </>
              ) : (
                <>
                  <Label>Select Role Persona</Label>
                  <div className="grid grid-cols-3 gap-2">
                    {[
                      { role: 'MANAGER' as OperatorRole, label: 'Manager (REIT)', token: DEMO_MANAGER_TOKEN },
                      { role: 'TRUSTEE' as OperatorRole, label: 'Trustee (Axis)', token: DEMO_TRUSTEE_TOKEN },
                      { role: 'COMPLIANCE' as OperatorRole, label: 'Compliance', token: DEMO_MANAGER_TOKEN },
                    ].map((item) => (
                      <button
                        key={item.role}
                        type="button"
                        onClick={() => {
                          setOperatorRole(item.role);
                          setOperatorToken(item.token);
                        }}
                        className={`rounded-xl border p-2.5 text-center text-xs font-bold uppercase tracking-wider transition-all cursor-pointer ${
                          operatorRole === item.role
                            ? 'border-white bg-white text-[#040916] shadow-md'
                            : 'border-white/15 bg-[#030816] text-slate-300 hover:text-white hover:border-white/30'
                        }`}
                      >
                        {item.label}
                      </button>
                    ))}
                  </div>

                  <div className="flex flex-col gap-1.5 pt-2">
                    <Label htmlFor="operatorToken">Cryptographic Bearer Token</Label>
                    <Input
                      id="operatorToken"
                      type="text"
                      className="font-mono text-xs"
                      value={operatorToken}
                      onChange={(e: ChangeEvent<HTMLInputElement>) => setOperatorToken(e.target.value)}
                      required
                    />
                  </div>
                </>
              )}

              {/* Curved Primary Button */}
              <div className="pt-2">
                <button
                  type="submit"
                  disabled={loading}
                  className="rounded-xl bg-white text-[#040916] font-bold text-xs uppercase tracking-wider h-12 w-full flex items-center justify-center gap-2 hover:bg-slate-100 transition-all cursor-pointer shadow-lg hover:shadow-[0_0_25px_rgba(255,255,255,0.25)] disabled:opacity-50"
                >
                  <span>
                    {loading
                      ? 'Authenticating...'
                      : authMode === 'investor'
                      ? isSignUp
                        ? 'Create Supabase Profile'
                        : 'Sign In to Portfolio'
                      : `Access Console as ${operatorRole}`}
                  </span>
                  <ArrowRight className="h-4 w-4" />
                </button>
              </div>
            </form>

            {/* Quick Demo Shortcut - Curved Glass Cards */}
            {authMode === 'investor' && (
              <div className="mt-5 space-y-2.5 border-t border-white/10 pt-4">
                <button
                  type="button"
                  onClick={handleLiveBidderDemo}
                  className="group flex w-full items-center justify-between rounded-2xl border border-emerald-500/35 bg-[#061e24]/80 p-3.5 text-left transition-all hover:border-emerald-300 hover:bg-[#082a33] cursor-pointer shadow-sm hover:shadow-[0_8px_25px_rgba(16,185,129,0.15)]"
                >
                  <div className="flex items-center gap-3">
                    <span className="flex h-9 w-9 items-center justify-center rounded-xl bg-emerald-400 text-[#040916]">
                      <Sparkles className="h-4 w-4" />
                    </span>
                    <div>
                      <div className="flex items-center gap-2">
                        <p className="font-sans text-xs font-bold uppercase tracking-wider text-white">
                          Ready-to-Bid Investor
                        </p>
                        <span className="rounded-full bg-emerald-500/20 border border-emerald-400/40 px-2.5 py-0.5 text-[9px] font-bold font-mono text-emerald-300 uppercase">
                          Open Offer Bidder
                        </span>
                      </div>
                      <p className="font-mono text-[11px] text-slate-300">
                        Rohan Verma · Onboarded to Live Scheme · Test Placing Bids
                      </p>
                    </div>
                  </div>
                  <span className="font-sans text-xs font-bold uppercase tracking-wider text-emerald-300 group-hover:translate-x-0.5 transition-transform">
                    Bid Live &rarr;
                  </span>
                </button>

                <button
                  type="button"
                  onClick={handleInstantDemo}
                  className="group flex w-full items-center justify-between rounded-2xl border border-blue-500/30 bg-[#06122d]/80 p-3.5 text-left transition-all hover:border-white hover:bg-[#0a1e47] cursor-pointer shadow-sm hover:shadow-[0_8px_25px_rgba(59,130,246,0.15)]"
                >
                  <div className="flex items-center gap-3">
                    <span className="flex h-9 w-9 items-center justify-center rounded-xl bg-white text-[#040916]">
                      <Sparkles className="h-4 w-4" />
                    </span>
                    <div>
                      <div className="flex items-center gap-2">
                        <p className="font-sans text-xs font-bold uppercase tracking-wider text-white">
                          Showcase Portfolio Investor
                        </p>
                        <span className="rounded-full bg-blue-500/20 border border-blue-400/40 px-2.5 py-0.5 text-[9px] font-bold font-mono text-blue-300 uppercase">
                          Settled Seed
                        </span>
                      </div>
                      <p className="font-mono text-[11px] text-slate-300">
                        Ananya Sharma · 2 Allotted Units · ₹34,200 Bank Payout
                      </p>
                    </div>
                  </div>
                  <span className="font-sans text-xs font-bold uppercase tracking-wider text-white group-hover:translate-x-0.5 transition-transform">
                    Portfolio &rarr;
                  </span>
                </button>
              </div>
            )}
          </div>
        </div>
      </div>
    </div>
  );
}
