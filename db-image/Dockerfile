# pgvector + TimescaleDB on PostgreSQL 18 (Debian trixie)
# Base: pgvector/pgvector:0.8.7-pg18-trixie (includes pgvector extension)
# Added: TimescaleDB from packagecloud timescale/timescaledb repo
# pgvector fest gepinnt (E-01-4): 0.8.3+ fixt die PG18-Distanz-Regression und die
# HNSW-Vacuum-Korruption. 0.8.6 (Sweep 2026-09-19) bringt nur IVFFlat-/sparsevec-
# Fixes, die wir nicht nutzen — der Bump hält den Rückstand bei null. 0.8.7
# (Sweep 2026-10-22) ebenso: IVFFlat-Build-Overflow + avg ohne Zeilen; live
# gibt es nur HNSW-Indizes. recall_check
# stempelt die pgvector-Version je Lauf (recall.go), ein Versionswechsel ist dort
# als Drift-Marker sichtbar, kein Gate.
# Digest-Pin: der Tag allein floatet die PostgreSQL-Minor mit (18.4 → 18.6 bei
# unverändertem Dockerfile gemessen). Tag bleibt als lesbares Label stehen; beim
# Bump Tag UND Digest gemeinsam ändern
# (`docker buildx imagetools inspect <tag> --format '{{.Manifest.Digest}}'`).
FROM pgvector/pgvector:0.8.7-pg18-trixie@sha256:9d9c930220cb9bf2f956d10a8f909cf9973d9672ca0278aef2c1e6facccad2e0

# TimescaleDB fest gepinnt (Dependency-Sweep 2026-09-19): ohne Versionsangabe zog
# ein Rebuild des unveränderten Dockerfiles 2.28.3 → 2.30.1 (gemessen). TimescaleDB
# belegt shared_preload_libraries — ein Minor-Sprung ist eine bewusste Entscheidung
# mit `ALTER EXTENSION timescaledb UPDATE` als Operator-Schritt, kein Build-Zufall.
# Loader mitgepinnt: das Hauptpaket verlangt ihn nur per `>=`, er würde sonst floaten.
# 2.30.1 verlangt postgresql-18 >= 18.6 — gekoppelt an den Digest der Basis oben.
# 2.31.0 (Sweep 2026-10-22): die zwei umbenannten GUCs (enable_columnarscan,
# direct-compress-Sortierung) sind live nicht gesetzt; der neue Loader braucht
# einen Neustart, den der Image-Tausch ohnehin bringt. Dieselbe Version fährt
# das Integrations-Image timescale/timescaledb-ha:pg18.
ARG TIMESCALEDB_DEB_VERSION=2.31.0~debian13-1806

# Signierschlüssel des packagecloud-Repos timescale/timescaledb, als
# Fingerprint-Liste gepinnt: Primärschlüssel, dann Signier-Subkey (rsa4096,
# 2018-10-19, ohne Ablaufdatum). Kein `curl … script.deb.sh | bash` mehr
# (#49 Punkt 1): das Skript war ungepinnt und vertraute dem Schlüssel beim
# ersten Abruf. Jetzt bricht der Build, sobald die Datei hinter der URL eine
# andere Liste trägt — anderer Schlüssel, zusätzlicher Schlüssel oder Subkey.
# Ein Schlüsselwechsel ist damit eine bewusste Pin-Änderung, kein Build-Zufall.
# Gemessen am 22.10.2026 über drei Wege mit identischem Ergebnis: die URL
# selbst; die Keyrings, die das alte Skript in die Images vom 25.07., 19.09.
# und 22.10.2026 geschrieben hat (byte-gleich untereinander und mit dem
# dearmorten Download); keyserver.ubuntu.com und keys.openpgp.org, die den
# Schlüssel mit der packagecloud-UID und Erstelldatum 2018-10-19 führen.
# Prüfen: `gpg --show-keys --with-colons <datei> | awk -F: '$1=="fpr"{print $10}'`.
ARG TIMESCALE_REPO_FPRS="1005FB68604CE9B8F6879CF759F18EDF47F24417 0641009A8366FDE4444A7B62E7391C94080429FF"

# ADD statt curl: BuildKit holt die Datei (TLS-geprüft, folgt dem
# CloudFront-Redirect), das Image braucht dafür kein curl. Vertraut wird ihr
# erst nach dem Fingerprint-Abgleich im RUN darunter.
ADD --chmod=644 https://packagecloud.io/timescale/timescaledb/gpgkey /tmp/timescale-gpgkey.asc

# apt-Source direkt mit signed-by= auf genau diesen Schlüssel — dieselbe Zeile,
# die das alte Skript von packagecloud bezog, ohne deb-src. Die Dist ist der
# Codename der Basis (trixie); das Paket-Pin ~debian13 hängt daran. gpg (nicht
# das Metapaket gnupg) reicht für show-keys und dearmor und fliegt danach
# wieder raus; /etc/os-release ersetzt lsb-release.
RUN set -eu \
    && apt-get update \
    && apt-get install -y --no-install-recommends \
        ca-certificates \
        gpg \
    && export GNUPGHOME="$(mktemp -d)" \
    && fprs="$(gpg --batch --show-keys --with-colons /tmp/timescale-gpgkey.asc \
        | awk -F: '$1 == "fpr" { printf "%s%s", sep, $10; sep = " " }')" \
    && if [ "$fprs" != "$TIMESCALE_REPO_FPRS" ]; then \
        echo "timescale gpgkey: fingerprints [$fprs] != pinned [$TIMESCALE_REPO_FPRS]" >&2; \
        exit 1; \
    fi \
    && install -d -m 0755 /etc/apt/keyrings \
    && gpg --batch --dearmor -o /etc/apt/keyrings/timescale_timescaledb-archive-keyring.gpg \
        /tmp/timescale-gpgkey.asc \
    && chmod 0644 /etc/apt/keyrings/timescale_timescaledb-archive-keyring.gpg \
    && . /etc/os-release \
    && echo "deb [signed-by=/etc/apt/keyrings/timescale_timescaledb-archive-keyring.gpg] https://packagecloud.io/timescale/timescaledb/debian/ ${VERSION_CODENAME:?} main" \
        > /etc/apt/sources.list.d/timescale_timescaledb.list \
    && apt-get update \
    && apt-get install -y --no-install-recommends \
        timescaledb-2-postgresql-18=${TIMESCALEDB_DEB_VERSION} \
        timescaledb-2-loader-postgresql-18=${TIMESCALEDB_DEB_VERSION} \
    && apt-get purge -y --auto-remove \
        gpg \
    && rm -rf "$GNUPGHOME" /var/lib/apt/lists/* /tmp/* /var/tmp/*

# TimescaleDB must be preloaded; pgvector does NOT need preloading
RUN echo "shared_preload_libraries = 'timescaledb'" >> /usr/share/postgresql/postgresql.conf.sample
