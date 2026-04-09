-- OAuth 2.1 client registration for MCP remote auth.
-- Each integration (claude.ai, Claude Code, etc.) gets its own client credentials.
-- Users authenticate with their API key during the authorize flow.

CREATE TABLE IF NOT EXISTS context_oauth_clients (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    client_id       VARCHAR(64) NOT NULL UNIQUE,
    client_secret_hash VARCHAR(128) NOT NULL,
    label           VARCHAR(200) NOT NULL,
    created_by      UUID REFERENCES context_api_keys(id) ON DELETE SET NULL,
    active          BOOLEAN NOT NULL DEFAULT true,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_oauth_clients_active ON context_oauth_clients(client_id) WHERE active = true;
