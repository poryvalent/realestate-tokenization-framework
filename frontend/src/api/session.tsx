import { createContext, useCallback, useContext, useEffect, useMemo, useState, type ReactNode } from 'react';
import { supabase } from './supabase';
import type { User } from '@supabase/supabase-js';

export type OperatorRole = 'MANAGER' | 'TRUSTEE' | 'COMPLIANCE' | '';
export type AuthSource = 'supabase' | 'devtoken';
export type PersonaType = 'investor' | 'operator';

interface Session {
  token: string | null;
  mockRole: OperatorRole;
  investorLabel: string;
  supabaseUser: User | null;
  authSource: AuthSource | null;
  persona: PersonaType | null;
  isOperator: boolean;
  isInvestor: boolean;
  signedIn: boolean;
  signIn: (token: string, opts?: { persona?: PersonaType; mockRole?: OperatorRole; investorLabel?: string; authSource?: AuthSource; user?: User }) => void;
  signOut: () => Promise<void>;
  setMockRole: (r: OperatorRole) => void;
}

const Ctx = createContext<Session | null>(null);

const LS_TOKEN = 'acresync.token';
const LS_ROLE = 'acresync.mockRole';
const LS_LABEL = 'acresync.investorLabel';
const LS_SOURCE = 'acresync.authSource';
const LS_PERSONA = 'acresync.persona';

export function SessionProvider({ children }: { children: ReactNode }) {
  const [token, setToken] = useState<string | null>(() => localStorage.getItem(LS_TOKEN));
  const [mockRole, setMockRoleState] = useState<OperatorRole>(
    () => (localStorage.getItem(LS_ROLE) as OperatorRole) || '',
  );
  const [investorLabel, setInvestorLabel] = useState<string>(
    () => localStorage.getItem(LS_LABEL) || '',
  );
  const [authSource, setAuthSource] = useState<AuthSource | null>(
    () => (localStorage.getItem(LS_SOURCE) as AuthSource) || null,
  );
  const [persona, setPersonaState] = useState<PersonaType | null>(() => {
    const saved = localStorage.getItem(LS_PERSONA) as PersonaType;
    if (saved) return saved;
    const r = localStorage.getItem(LS_ROLE);
    if (r) return 'operator';
    if (localStorage.getItem(LS_TOKEN)) return 'investor';
    return null;
  });
  const [supabaseUser, setSupabaseUser] = useState<User | null>(null);

  useEffect(() => {
    if (!supabase) return;
    supabase.auth.getUser().then(({ data }) => {
      if (data?.user) setSupabaseUser(data.user);
    }).catch(() => {});

    const { data: authListener } = supabase.auth.onAuthStateChange((_event, session) => {
      setSupabaseUser(session?.user ?? null);
    });
    return () => {
      authListener?.subscription?.unsubscribe();
    };
  }, []);

  const signIn = useCallback((t: string, opts?: { persona?: PersonaType; mockRole?: OperatorRole; investorLabel?: string; authSource?: AuthSource; user?: User }) => {
    setToken(t);
    localStorage.setItem(LS_TOKEN, t);
    
    // Explicit persona determination
    const determinedPersona: PersonaType = opts?.persona ?? (opts?.mockRole ? 'operator' : 'investor');
    setPersonaState(determinedPersona);
    localStorage.setItem(LS_PERSONA, determinedPersona);

    if (opts?.mockRole !== undefined) {
      setMockRoleState(opts.mockRole);
      localStorage.setItem(LS_ROLE, opts.mockRole);
    } else if (determinedPersona === 'investor') {
      setMockRoleState('');
      localStorage.removeItem(LS_ROLE);
    }

    if (opts?.investorLabel !== undefined) {
      setInvestorLabel(opts.investorLabel);
      localStorage.setItem(LS_LABEL, opts.investorLabel);
    }
    if (opts?.authSource !== undefined) {
      setAuthSource(opts.authSource);
      localStorage.setItem(LS_SOURCE, opts.authSource);
    }
    if (opts?.user !== undefined) {
      setSupabaseUser(opts.user);
    }
  }, []);

  const signOut = useCallback(async () => {
    setToken(null);
    setSupabaseUser(null);
    setAuthSource(null);
    setPersonaState(null);
    setMockRoleState('');
    setInvestorLabel('');
    localStorage.removeItem(LS_TOKEN);
    localStorage.removeItem(LS_ROLE);
    localStorage.removeItem(LS_LABEL);
    localStorage.removeItem(LS_SOURCE);
    localStorage.removeItem(LS_PERSONA);
    if (supabase) {
      try {
        await supabase.auth.signOut();
      } catch {
        // Ignore sign-out errors
      }
    }
  }, []);

  const setMockRole = useCallback((r: OperatorRole) => {
    setMockRoleState(r);
    if (r) {
      setPersonaState('operator');
      localStorage.setItem(LS_PERSONA, 'operator');
      localStorage.setItem(LS_ROLE, r);
    } else {
      localStorage.removeItem(LS_ROLE);
    }
  }, []);

  const signedIn = !!token;
  const isOperator = signedIn && (persona === 'operator' || mockRole !== '');
  const isInvestor = signedIn && !isOperator;

  const value = useMemo<Session>(
    () => ({
      token,
      mockRole,
      investorLabel,
      supabaseUser,
      authSource,
      persona,
      isOperator,
      isInvestor,
      signedIn,
      signIn,
      signOut,
      setMockRole,
    }),
    [token, mockRole, investorLabel, supabaseUser, authSource, persona, isOperator, isInvestor, signedIn, signIn, signOut, setMockRole],
  );
  return <Ctx.Provider value={value}>{children}</Ctx.Provider>;
}

export function useSession(): Session {
  const s = useContext(Ctx);
  if (!s) throw new Error('useSession must be used inside SessionProvider');
  return s;
}
