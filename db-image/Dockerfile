# pgvector + TimescaleDB on PostgreSQL 18 (Debian trixie)
# Base: pgvector/pgvector:pg18-trixie (includes pgvector extension)
# Added: TimescaleDB from packagecloud timescale/timescaledb repo
FROM pgvector/pgvector:pg18-trixie

# Install TimescaleDB from official packagecloud repo (trixie packages for PG18)
RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        ca-certificates \
        curl \
        gnupg \
        lsb-release \
    && curl -s https://packagecloud.io/install/repositories/timescale/timescaledb/script.deb.sh | bash \
    && apt-get install -y --no-install-recommends \
        timescaledb-2-postgresql-18 \
    && apt-get purge -y --auto-remove \
        curl \
        gnupg \
        lsb-release \
    && rm -rf /var/lib/apt/lists/* /tmp/* /var/tmp/*

# TimescaleDB must be preloaded; pgvector does NOT need preloading
RUN echo "shared_preload_libraries = 'timescaledb'" >> /usr/share/postgresql/postgresql.conf.sample
