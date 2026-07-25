# pgvector + TimescaleDB on PostgreSQL 18 (Debian trixie)
# Base: pgvector/pgvector:0.8.5-pg18-trixie (includes pgvector extension)
# Added: TimescaleDB from packagecloud timescale/timescaledb repo
# pgvector fest auf 0.8.5 gepinnt (E-01-4): 0.8.3+ fixt die PG18-Distanz-Regression
# und die HNSW-Vacuum-Korruption; recall_check (Mig 110) erwartet 0.8.5 als Baseline.
FROM pgvector/pgvector:0.8.5-pg18-trixie

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
