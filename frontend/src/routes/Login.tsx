import { useState } from 'react';
import { Link, useNavigate } from 'react-router-dom';
import { motion } from 'framer-motion';
import { ArrowRight, KeyRound, ScanEye } from 'lucide-react';
import { useSession, type OperatorRole } from '../api/session';
import { useToast } from '../components/Toast';
import { Card, Accordion, Reveal } from '../components/ui';
import { Io } from '../components/motion';

const inputCls =
  'w-full rounded-xl border border-white/15 bg-white/5 px-4 py-2.5 text-sm text-white placeholder:text-white/30 outline-none transition focus:border-brand-blue/60 focus:bg-white/[0.07]';

export default function Login() {
  const { signIn, signedIn } = useSession();
  const toast = useToast();
  const nav = useNavigate();
  const [token, setToken] = useState('acresync-event-access');
  const [role, setRole] = useState<OperatorRole>('');
  const [label, setLabel] = useState('');

  const submit = (e: React.FormEvent) => {
    e.preventDefault();
    if (!token.trim()) return;
    signIn(token.trim(), { mockRole: role, investorLabel: label.trim() });
    toast('Signed in. Welcome to AcreSync.');
    nav('/portfolio');
  };

  return (
    <>
      <Reveal
        title={
          <>
            Sign in<span className="text-brand-blue">.</span>
          </>
        }
        lede={
          <>
            Two identities stay separate on purpose: the login says <em className="text-white">who is calling</em>,
            the investor record says <em className="text-white">what they hold</em>. Use the event access token from
            your welcome pack — it opens a funded showcase investor profile.
          </>
        }
      />
      <div className="mt-8 grid gap-4 lg:grid-cols-[1.1fr_0.9fr]">
        <Io>
          <Card>
            <p className="mb-5 flex items-center gap-2 text-xs font-medium uppercase tracking-[0.2em] text-white/40">
              <KeyRound className="h-4 w-4 text-brand-blue" /> Mock session
            </p>
            <form onSubmit={submit}>
              <div className="mb-4">
                <label htmlFor="token" className="mb-1.5 block text-sm font-semibold text-white">
                  Access token
                </label>
                <input
                  id="token"
                  className={`${inputCls} font-mono`}
                  value={token}
                  onChange={(e) => setToken(e.target.value)}
                  autoComplete="off"
                />
                <p className="mt-1.5 text-xs text-white/40">Pre-filled for this event — just press sign in.</p>
              </div>
              <div className="mb-4">
                <label htmlFor="role" className="mb-1.5 block text-sm font-semibold text-white">
                  View as
                </label>
                <select
                  id="role"
                  className={`${inputCls} appearance-none`}
                  value={role}
                  onChange={(e) => setRole(e.target.value as OperatorRole)}
                >
                  <option value="">Investor</option>
                  <option value="MANAGER">Manager — runs the lifecycle</option>
                  <option value="TRUSTEE">Trustee — second-party sign-off</option>
                  <option value="COMPLIANCE">Compliance — full read access</option>
                </select>
                <p className="mt-1.5 text-xs text-white/40">
                  Switch perspectives live: place a bid as an investor, then run the ballot as the manager.
                </p>
              </div>
              <div className="mb-5">
                <label htmlFor="label" className="mb-1.5 block text-sm font-semibold text-white">
                  Display name (shown in the top bar)
                </label>
                <input
                  id="label"
                  className={inputCls}
                  value={label}
                  onChange={(e) => setLabel(e.target.value)}
                  placeholder="e.g. Guest investor"
                  autoComplete="off"
                />
              </div>
              <motion.button
                whileTap={{ scale: 0.97 }}
                type="submit"
                className="inline-flex items-center gap-2 rounded-full bg-white px-6 py-2.5 text-sm font-semibold text-brand-dark transition hover:bg-brand-mist disabled:opacity-50"
                disabled={!token.trim()}
              >
                {signedIn ? 'Switch session' : 'Sign in'} <ArrowRight className="h-4 w-4" />
              </motion.button>
            </form>
          </Card>
        </Io>
        <div className="grid content-start gap-4">
          <Io delay={100}>
            <Card flat>
              <Accordion title="At the demo station" defaultOpen>
                <p className="mb-0">
                  Start at <span className="font-mono text-white">Schemes</span>, open the live offer and place a bid
                  — funds are blocked in the investor's own account, never collected. Then switch to the{' '}
                  <span className="font-mono text-white">Manager</span> view to freeze the book, run the commit–reveal
                  ceremony and settle, watching readiness drive every step.
                </p>
              </Accordion>
            </Card>
          </Io>
          <Io delay={160}>
            <Card flat>
              <p className="mb-0 flex items-center gap-2 text-sm text-white/70">
                <ScanEye className="h-4 w-4 text-brand-blue" />
                <Link to="/admin" className="font-medium text-white underline-offset-4 hover:underline">
                  Operator console →
                </Link>
                <span className="text-white/40">(needs a manager or trustee view)</span>
              </p>
            </Card>
          </Io>
        </div>
      </div>
    </>
  );
}
