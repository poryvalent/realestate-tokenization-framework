import { createContext, useCallback, useContext, useMemo, useState, type ReactNode } from 'react';

export type OperatorRole = 'MANAGER' | 'TRUSTEE' | 'COMPLIANCE' | '';

interface Session {
  token: string | null;
  mockRole: OperatorRole;
  investorLabel: string;
  signedIn: boolean;
  signIn: (token: string, opts?: { mockRole?: OperatorRole; investorLabel?: string }) => void;
  signOut: () => void;
  setMockRole: (r: OperatorRole) => void;
}

const Ctx = createContext<Session | null>(null);

const LS_TOKEN = 'acresync.token';
const LS_ROLE = 'acresync.mockRole';
const LS_LABEL = 'acresync.investorLabel';

export function SessionProvider({ children }: { children: ReactNode }) {
  const [token, setToken] = useState<string | null>(() => localStorage.getItem(LS_TOKEN));
  const [mockRole, setMockRoleState] = useState<OperatorRole>(
    () => (localStorage.getItem(LS_ROLE) as OperatorRole) || '',
  );
  const [investorLabel, setInvestorLabel] = useState<string>(
    () => localStorage.getItem(LS_LABEL) || '',
  );

  const signIn = useCallback((t: string, opts?: { mockRole?: OperatorRole; investorLabel?: string }) => {
    setToken(t);
    localStorage.setItem(LS_TOKEN, t);
    if (opts?.mockRole !== undefined) {
      setMockRoleState(opts.mockRole);
      localStorage.setItem(LS_ROLE, opts.mockRole);
    }
    if (opts?.investorLabel !== undefined) {
      setInvestorLabel(opts.investorLabel);
      localStorage.setItem(LS_LABEL, opts.investorLabel);
    }
  }, []);

  const signOut = useCallback(() => {
    setToken(null);
    localStorage.removeItem(LS_TOKEN);
  }, []);

  const setMockRole = useCallback((r: OperatorRole) => {
    setMockRoleState(r);
    localStorage.setItem(LS_ROLE, r);
  }, []);

  const value = useMemo<Session>(
    () => ({ token, mockRole, investorLabel, signedIn: !!token, signIn, signOut, setMockRole }),
    [token, mockRole, investorLabel, signIn, signOut, setMockRole],
  );
  return <Ctx.Provider value={value}>{children}</Ctx.Provider>;
}

export function useSession(): Session {
  const s = useContext(Ctx);
  if (!s) throw new Error('useSession must be used inside SessionProvider');
  return s;
}
