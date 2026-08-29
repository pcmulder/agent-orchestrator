-- Runs once on a fresh postgres data volume (see cloud/dev/postgres/init.sql
-- for the same pattern). Creates the non-privileged owner role Forgejo
-- connects with, hands it ownership of the runtime database and schema, and
-- locks down the image bootstrap role.

CREATE ROLE ao_forgejo
    LOGIN
    PASSWORD 'ao_forgejo_local'
    NOSUPERUSER
    NOCREATEDB
    NOCREATEROLE
    NOINHERIT
    NOBYPASSRLS;

ALTER DATABASE forgejo OWNER TO ao_forgejo;
ALTER SCHEMA public OWNER TO ao_forgejo;

ALTER ROLE ao_forgejo_bootstrap NOLOGIN;
