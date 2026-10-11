import { Link } from 'react-router-dom';
import { ArrowRight, Fingerprint, Landmark, ShieldCheck } from 'lucide-react';
import { useSession } from '../api/session';
import { usePoll } from '../api/usePoll';
import type { Me } from '../api/types';
import { Card, LoadingCard, ErrorBox, StatusPill, Empty, Reveal } from '../components/ui';
import { Io } from '../components/motion';

type MeWithAnchor = Me & { investorAnchor?: string };

export default function MeView() {
  const { token, signedIn } = useSession();
  const { data, loading, error, refresh } = usePoll<Me>(signedIn ? '/me' : null, { token });

  if (!signedIn) {
    return (
      <>
        <Reveal
          title={
            <>
              My profile<span className="text-brand-blue">.</span>
            </>
          }
          lede="Your investor record — identity, demat and bank accounts on file."
        />
        <div className="mt-8">
          <Empty
            title="Not signed in"
            body="Investor records live behind a session token."
            action={
              <Link
                to="/login"
                className="rounded-none bg-white px-6 py-2.5 text-xs font-bold uppercase tracking-wider text-[#040916] transition hover:bg-slate-200 shadow-sm"
              >
                Sign in
              </Link>
            }
          />
        </div>
      </>
    );
  }
  if (loading) {
    return (
      <>
        <Reveal title={<>My profile<span className="text-brand-blue">.</span></>} />
        <div className="mt-8">
          <LoadingCard label="Loading profile" />
        </div>
      </>
    );
  }
  if (error) {
    return (
      <>
        <Reveal title={<>My profile<span className="text-brand-blue">.</span></>} />
        <div className="mt-8">
          <ErrorBox error={error} retry={refresh} />
        </div>
      </>
    );
  }
  if (!data) {
    return (
      <>
        <Reveal title={<>My profile<span className="text-brand-blue">.</span></>} />
        <div className="mt-8">
          <Empty title="No profile" />
        </div>
      </>
    );
  }
  const m = data as MeWithAnchor;

  return (
    <>
      <Reveal
        title={<>{m.displayName ?? 'Investor'}</>}
        lede="Your investor record — identity, demat and bank accounts on file. Anchors are public commitments; raw identifiers never leave the vault."
      />
      <div className="mt-8 grid gap-4 md:grid-cols-2">
        <Io>
          <Card>
            <p className="mb-3 flex items-center gap-2 text-xs font-medium uppercase tracking-[0.2em] text-white/40">
              <ShieldCheck className="h-4 w-4 text-brand-blue" /> Identity
            </p>
            <dl className="grid grid-cols-[minmax(120px,160px)_1fr] gap-x-4 gap-y-3">
              <dt className="text-sm text-white/40">Class</dt>
              <dd className="font-medium text-white">{m.investorClass ?? '—'}</dd>
              <dt className="text-sm text-white/40">KYC</dt>
              <dd>
                <StatusPill kind="bid" status={m.kycStatus ?? 'UNKNOWN'} />
              </dd>
              <dt className="text-sm text-white/40">PAN</dt>
              <dd className="font-mono text-sm text-white/80">{m.panMasked ?? '—'}</dd>
              <dt className="text-sm text-white/40">Wallet</dt>
              <dd className="break-all font-mono text-sm text-white/80">{m.walletAddress ?? '—'}</dd>
              {m.investorAnchor && (
                <>
                  <dt className="text-sm text-white/40">Anchor</dt>
                  <dd className="break-all font-mono text-sm text-white/80">{m.investorAnchor}</dd>
                </>
              )}
            </dl>
          </Card>
        </Io>
        <Io delay={100}>
          <Card>
            <p className="mb-3 flex items-center gap-2 text-xs font-medium uppercase tracking-[0.2em] text-white/40">
              <Landmark className="h-4 w-4 text-brand-blue" /> Accounts
            </p>
            <h3 className="mb-2 mt-0 flex items-center gap-2 text-sm font-semibold text-white">
              <Fingerprint className="h-4 w-4 text-white/50" /> Demat
            </h3>
            {(m.dematAccounts ?? []).length === 0 && <p className="text-sm text-slate-400">No demat accounts on file.</p>}
            {(m.dematAccounts ?? []).map((d) => (
              <p key={d.id} className="text-sm text-slate-300">
                {d.depository} · <span className="font-mono">{d.maskedClientId}</span> ·{' '}
                <span className={d.verified ? 'text-emerald-400 font-mono text-xs uppercase' : 'text-slate-400 font-mono text-xs uppercase'}>
                  {d.verified ? 'verified' : 'unverified'}
                </span>
              </p>
            ))}
            <h3 className="mb-2 mt-5 text-sm font-semibold text-white">Bank</h3>
            {(m.bankAccounts ?? []).length === 0 && <p className="text-sm text-slate-400">No bank accounts on file.</p>}
            {(m.bankAccounts ?? []).map((b) => (
              <p key={b.id} className="text-sm text-slate-300">
                <span className="font-mono">{b.ifsc}</span>
                {b.maskedAccountNumber && (
                  <> · <span className="font-mono">{b.maskedAccountNumber}</span></>
                )}{' '}
                ·{' '}
                <span className={b.verified ? 'text-emerald-400 font-mono text-xs uppercase' : 'text-slate-400 font-mono text-xs uppercase'}>
                  {b.verified ? 'verified' : 'unverified'}
                </span>
              </p>
            ))}
          </Card>
        </Io>
      </div>
      <Io className="mt-6">
        <p className="flex flex-wrap gap-3">
          <Link
            to="/portfolio"
            className="inline-flex items-center gap-2 rounded-none bg-white px-5 py-2.5 text-xs font-bold uppercase tracking-wider text-[#040916] transition hover:bg-slate-200 shadow-sm"
          >
            Portfolio <ArrowRight className="h-4 w-4" />
          </Link>
          <Link
            to="/bids"
            className="inline-flex items-center gap-2 rounded-none border border-white/25 bg-[#050c1f] px-5 py-2.5 text-xs font-bold uppercase tracking-wider text-white transition hover:bg-white/10"
          >
            My bids <ArrowRight className="h-4 w-4" />
          </Link>
        </p>
      </Io>
    </>
  );
}
