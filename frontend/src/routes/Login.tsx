import { useState } from 'react';
import { Link, useNavigate } from 'react-router-dom';
import { useSession, type OperatorRole } from '../api/session';
import { useToast } from '../components/Toast';
import { Card, Accordion } from '../components/ui';

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
      <h1>Sign in</h1>
      <p>
        Two identities stay separate on purpose: the login says <em>who is calling</em>, the investor
        record says <em>what they hold</em>. Use the event access token from your welcome pack —
        it opens a funded showcase investor profile.
      </p>
      <div className="grid2">
        <Card>
          <form onSubmit={submit}>
            <div className="field">
              <label htmlFor="token">Access token</label>
              <input id="token" className="input mono" value={token} onChange={(e) => setToken(e.target.value)} autoComplete="off" />
              <p className="hint">Pre-filled for this event — just press sign in.</p>
            </div>
            <div className="field">
              <label htmlFor="role">View as</label>
              <select id="role" className="input" value={role} onChange={(e) => setRole(e.target.value as OperatorRole)}>
                <option value="">Investor</option>
                <option value="MANAGER">Manager — runs the lifecycle</option>
                <option value="TRUSTEE">Trustee — second-party sign-off</option>
                <option value="COMPLIANCE">Compliance — full read access</option>
              </select>
              <p className="hint">Switch perspectives live: place a bid as an investor, then run the ballot as the manager.</p>
            </div>
            <div className="field">
              <label htmlFor="label">Display name (shown in the top bar)</label>
              <input id="label" className="input" value={label} onChange={(e) => setLabel(e.target.value)} placeholder="e.g. Guest investor" autoComplete="off" />
            </div>
            <button type="submit" className="btn primary" disabled={!token.trim()}>
              {signedIn ? 'Switch session' : 'Sign in'}
            </button>
          </form>
        </Card>
        <div>
          <Card flat>
            <Accordion title="At the demo station" defaultOpen>
              <p>
                Start at <span className="mono">Schemes</span>, open the live offer and place a bid —
                funds are blocked in the investor's own account, never collected. Then switch to the{' '}
                <span className="mono">Manager</span> view to freeze the book, run the commit–reveal
                ceremony and settle, watching readiness drive every step.
              </p>
            </Accordion>
          </Card>
          <Card flat>
            <p style={{ marginBottom: 0 }}><Link to="/admin">Operator console →</Link> (needs a manager or trustee view)</p>
          </Card>
        </div>
      </div>
    </>
  );
}
