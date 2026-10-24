# PostgreSQL 18 + pgvector + TimescaleDB

Custom Docker image combining pgvector and TimescaleDB on PostgreSQL 18 (Debian trixie).

## Base Image

`pgvector/pgvector:pg18-trixie` -- PostgreSQL 18 on Debian 13 (trixie) with pgvector pre-installed.

Trixie is required because TimescaleDB PG18 packages are only available for Debian trixie.

## What's Included

| Extension    | Source                              | Loaded via                     |
|-------------|-------------------------------------|--------------------------------|
| pgvector    | Base image (pre-built)              | `CREATE EXTENSION vector`      |
| TimescaleDB | packagecloud timescale/timescaledb  | `shared_preload_libraries` + `CREATE EXTENSION timescaledb` |

The TimescaleDB apt source is written directly with `signed-by=` on the packagecloud
repository key — no install script piped into a shell. The key's fingerprints are pinned
in the Dockerfile (`TIMESCALE_REPO_FPRS`): a different, additional or rotated key fails
the build instead of being trusted on first use.

## Build

```bash
cd /compose/n8n
docker compose build db
```

Or standalone:

```bash
docker build -t pgvector-timescaledb:pg18 /compose/n8n/db-image/
```

## Data Directory

Uses the same `pg_ctlcluster` layout as the base image: data lives under `/var/lib/postgresql/18/main`.
The compose mount `./db:/var/lib/postgresql` remains unchanged.

## shared_preload_libraries

TimescaleDB requires preloading via `shared_preload_libraries = 'timescaledb'`.
This is appended to `postgresql.conf.sample` in the Dockerfile so it applies to both
fresh `initdb` runs and existing clusters.

pgvector does NOT require preloading -- it is loaded on-demand via `CREATE EXTENSION`.

## Extension Creation

Extensions are created in `init-data.sh` (runs on first `initdb` only):

```sql
-- As superuser, in context_store DB:
CREATE EXTENSION IF NOT EXISTS vector;
CREATE EXTENSION IF NOT EXISTS timescaledb;
```

For existing databases, run manually:

```bash
docker compose exec db psql -U admin -d context_store -c "CREATE EXTENSION IF NOT EXISTS timescaledb;"
```
