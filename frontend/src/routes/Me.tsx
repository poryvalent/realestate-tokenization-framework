import { Link } from 'react-router-dom';
import { useSession } from '../api/session';
import { usePoll } from '../api/usePoll';
import type { Me } from '../api/types';
import { Card, LoadingCard, ErrorBox, StatusPill, Empty } from '../components/ui';

export default function MeView() {
  const { token, signedIn } = useSession();
  const { data, loading, error, refresh } = usePoll<Me>(signedIn ? '/me' : null, { token });

  if (!signedIn) {
    return (
      <>
        <h1>My profile</h1>
        <Empty title="Not signed in" body="Investor records live behind a session token." action={<Link className="btn primary" to="/login">Sign in</Link>} />
      </>
    );
  }
  if (loading) return <LoadingCard label="Loading profile" />;
  if (error) return <ErrorBox error={error} retry={refresh} />;
  if (!data) return <Empty title="No profile" />;
  const m = data;

  return (
    <>
      <h1>{m.displayName ?? 'Investor'}</h1>
      <div className="grid2">
        <Card>
          <h3 style={{ marginTop: 0 }}>Identity</h3>
          <dl className="kv">
            <dt>Class</dt><dd>{m.investorClass ?? '—'}</dd>
            <dt>KYC</dt><dd><StatusPill kind="bid" status={m.kycStatus ?? 'UNKNOWN'} /></dd>
            <dt>PAN</dt><dd className="mono">{m.panMasked ?? '—'}</dd>
            <dt>Wallet</dt><dd className="mono hash">{m.walletAddress ?? '—'}</dd>
          </dl>
        </Card>
        <Card>
          <h3 style={{ marginTop: 0 }}>Accounts</h3>
          <h3>Demat</h3>
          {(m.dematAccounts ?? []).length === 0 && <p>No demat accounts on file.</p>}
          {(m.dematAccounts ?? []).map((d) => (
            <p key={d.id}>{d.depository} · <span className="mono">{d.maskedClientId}</span> · {d.verified ? 'verified' : 'unverified'}</p>
          ))}
          <h3>Bank</h3>
          {(m.bankAccounts ?? []).length === 0 && <p>No bank accounts on file.</p>}
          {(m.bankAccounts ?? []).map((b) => (
            <p key={b.id}><span className="mono">{b.ifsc}</span> · <span className="mono">{b.maskedAccountNumber}</span> · {b.verified ? 'verified' : 'unverified'}</p>
          ))}
        </Card>
      </div>
      <p><Link to="/portfolio">Portfolio →</Link> · <Link to="/bids">My bids →</Link></p>
    </>
  );
}
