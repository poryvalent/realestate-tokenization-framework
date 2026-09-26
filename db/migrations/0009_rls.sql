-- AcreSync M0 :: 0009 :: row level security, deny by default
--
-- Every table in this schema is either operational state owned by the Go
-- orchestrator or personal data. None of it is safe to expose through Supabase's
-- auto-generated REST API to an anon or authenticated client.
--
-- RLS is therefore enabled everywhere with no permissive policies at all. In
-- Postgres, RLS with zero policies denies every row to every non-superuser,
-- non-owner role. Supabase's service_role carries BYPASSRLS, so the orchestrator
-- continues to work while anon and authenticated see nothing.
--
-- Investor-facing read policies belong to M8, when the frontend exists and the
-- exact rows an authenticated investor may see have been decided. Adding them
-- now would be guessing at a surface that does not yet have a consumer, and a
-- premature policy is worse than no policy: it looks reviewed.


DO $$
DECLARE
    t TEXT;
BEGIN
    FOR t IN
        SELECT tablename FROM pg_tables WHERE schemaname = 'public'
    LOOP
        EXECUTE format('ALTER TABLE public.%I ENABLE ROW LEVEL SECURITY', t);
        EXECUTE format('ALTER TABLE public.%I FORCE ROW LEVEL SECURITY', t);
        EXECUTE format('REVOKE ALL ON public.%I FROM anon, authenticated', t);
    END LOOP;
END
$$;

-- FORCE ROW LEVEL SECURITY above matters more than it looks. Without it, the
-- table owner bypasses RLS, and the migration role is the owner. With it, even
-- the owner is subject to policy, so a future accidental grant to anon cannot
-- quietly open a table that appears protected.

-- Sequences and functions are equally not part of the public API surface.
DO $$
DECLARE
    s TEXT;
BEGIN
    FOR s IN
        SELECT sequencename FROM pg_sequences WHERE schemaname = 'public'
    LOOP
        EXECUTE format('REVOKE ALL ON SEQUENCE public.%I FROM anon, authenticated', s);
    END LOOP;
END
$$;

REVOKE ALL ON FUNCTION public.check_settlement_invariants(UUID)      FROM anon, authenticated;
REVOKE ALL ON FUNCTION public.assert_period_entitlements_exact(UUID) FROM anon, authenticated;

REVOKE USAGE ON SCHEMA public FROM anon;

