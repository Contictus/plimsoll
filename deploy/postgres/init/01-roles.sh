#!/bin/sh
# Runs once, as the superuser, on an empty data directory.
#
# Two roles, because a table owner bypasses RLS in Postgres (K15). goose migrates as
# plimsoll_owner; the api and worker connect as plimsoll_app. Neither is a superuser --
# only the container's `postgres` role is, and nothing in the application uses it.
set -eu

psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname postgres \
     -v owner_pw="$PLIMSOLL_OWNER_PASSWORD" \
     -v app_pw="$PLIMSOLL_APP_PASSWORD" <<-'EOSQL'
	CREATE ROLE plimsoll_owner LOGIN PASSWORD :'owner_pw';
	CREATE ROLE plimsoll_app   LOGIN PASSWORD :'app_pw';
	CREATE DATABASE plimsoll OWNER plimsoll_owner;
EOSQL

psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname plimsoll <<-'EOSQL'
	-- The app role gets to USE the schema and nothing more. Table privileges are granted
	-- explicitly per migration, so a new table is unreachable until someone decides it
	-- should be.
	REVOKE ALL ON SCHEMA public FROM PUBLIC;
	ALTER SCHEMA public OWNER TO plimsoll_owner;
	GRANT USAGE ON SCHEMA public TO plimsoll_app;
EOSQL
