-- AcreSync :: cluster role bootstrap
--
-- Run ONCE against a plain Postgres instance before applying migrations.
-- On Supabase this is unnecessary and does nothing: the three roles already exist.
--
-- # Why this is not a migration
--
-- 0009_rls revokes privileges from anon and authenticated and relies on service_role
-- carrying BYPASSRLS. Those roles are part of the Supabase environment rather than part
-- of this schema, and on Supabase a migration could not create them anyway because it
-- does not run as a superuser.
--
-- So they are a prerequisite of the cluster, in the same category as the database itself
-- existing. Putting them in a migration would also mean editing 0009 or adding a
-- migration that runs after the one that fails, neither of which works: 0009 aborts the
-- run before anything later is reached, and rewriting an applied migration invalidates
-- its recorded checksum, which is the habit the checksum check exists to prevent.
--
-- # Why the roles are created with these attributes
--
-- NOLOGIN: nothing should ever connect as these. They exist to be the target of grants
-- and revokes, which is how Supabase's PostgREST maps an HTTP caller to a database role.
--
-- BYPASSRLS on service_role only: 0009 enables RLS on every table with zero permissive
-- policies, which denies all rows to everyone except superusers and roles that bypass.
-- The orchestrator connects as the owner or as service_role, so without BYPASSRLS the
-- application would lock itself out of its own database. That asymmetry is the entire
-- security model: one role can read everything, the two web-facing roles can read nothing.

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'anon') THEN
        CREATE ROLE anon NOLOGIN NOINHERIT;
    END IF;

    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'authenticated') THEN
        CREATE ROLE authenticated NOLOGIN NOINHERIT;
    END IF;

    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'service_role') THEN
        CREATE ROLE service_role NOLOGIN NOINHERIT BYPASSRLS;
    END IF;
END
$$;

-- Verification, so a silent partial bootstrap cannot be mistaken for success.
DO $$
DECLARE
    missing TEXT;
BEGIN
    SELECT string_agg(r, ', ') INTO missing
      FROM unnest(ARRAY['anon', 'authenticated', 'service_role']) AS r
     WHERE NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = r);

    IF missing IS NOT NULL THEN
        RAISE EXCEPTION 'bootstrap incomplete, missing role(s): %', missing;
    END IF;

    IF NOT EXISTS (
        SELECT 1 FROM pg_roles WHERE rolname = 'service_role' AND rolbypassrls
    ) THEN
        RAISE EXCEPTION
            'service_role exists but lacks BYPASSRLS; 0009 enables RLS with no policies, '
            'so the orchestrator would be denied every row in its own database';
    END IF;
END
$$;
