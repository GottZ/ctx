package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// OAuthClient represents a registered OAuth client. The constraint fields
// (migration 097, design 02 §3) are data only until 03 enforces them at
// /authorize + /token: scopes is a requestable CEILING, never authoritative
// (INV-B — the key authority stays the hard cap), redirect_uris is the exact
// allowlist 03 will match (empty = pre-097 client, static S2 list applies).
type OAuthClient struct {
	ID                      string    `json:"id"`
	ClientID                string    `json:"client_id"`
	Label                   string    `json:"label"`
	Active                  bool      `json:"active"`
	CreatedBy               *string   `json:"created_by,omitempty"`
	CreatedAt               time.Time `json:"created_at"`
	RedirectURIs            []string  `json:"redirect_uris"`
	Scopes                  []string  `json:"scopes"`
	GrantTypes              []string  `json:"grant_types"`
	ResponseTypes           []string  `json:"response_types"`
	TokenEndpointAuthMethod string    `json:"token_endpoint_auth_method"`
	RegistrationSource      string    `json:"registration_source"`
	UpdatedAt               time.Time `json:"updated_at"`
}

// RegisterOAuthClientSpec carries an explicit client registration (CLI
// pre-registration with --redirect-uri today, DCR /register in 02-W4).
// Zero-value slices fall back to the schema defaults of migration 097.
type RegisterOAuthClientSpec struct {
	Label                   string
	RedirectURIs            []string
	Scopes                  []string
	GrantTypes              []string
	ResponseTypes           []string
	TokenEndpointAuthMethod string // none | client_secret_basic | client_secret_post
	Source                  string // admin | dcr | cimd
	CreatedBy               string // acting api_key id ("" for open DCR)
	// Metadata carries the RFC 7591 low-priority fields (client_uri,
	// logo_uri, contacts, tos_uri, policy_uri, software_id) into the 097
	// jsonb column. nil folds onto the schema default '{}' — the label-only
	// admin path stays behavior-identical.
	Metadata map[string]any
}

// RegisterOAuthClient stores an explicit client registration (design 02 §4a).
// A secret is minted for every registration except explicit public clients
// (TokenEndpointAuthMethod == "none" via DCR keeps client_secret_hash = ''
// — an empty hash is never a valid secret); the admin path passes "" and
// keeps today's mint-always behavior via the schema default 'none'.
func RegisterOAuthClient(ctx context.Context, pool *pgxpool.Pool, spec RegisterOAuthClientSpec) (*OAuthClient, string, error) {
	clientIDBytes := make([]byte, 16)
	if _, err := rand.Read(clientIDBytes); err != nil {
		return nil, "", fmt.Errorf("oauth: generate client_id: %w", err)
	}
	clientID := "ctx_" + hex.EncodeToString(clientIDBytes)

	secret := ""
	secretHash := ""
	if spec.TokenEndpointAuthMethod != "none" {
		secretBytes := make([]byte, 32)
		if _, err := rand.Read(secretBytes); err != nil {
			return nil, "", fmt.Errorf("oauth: generate secret: %w", err)
		}
		secret = hex.EncodeToString(secretBytes)
		secretHash = hashSecret(secret)
	}

	// COALESCE(NULLIF(...)) folds zero values onto the 097 schema defaults so
	// the label-only path stays byte-identical to the pre-C2 INSERT.
	source := spec.Source
	if source == "" {
		source = "admin"
	}
	authMethod := spec.TokenEndpointAuthMethod
	if authMethod == "" {
		authMethod = "none"
	}
	grantTypes := spec.GrantTypes
	if len(grantTypes) == 0 {
		grantTypes = []string{"authorization_code"}
	}
	responseTypes := spec.ResponseTypes
	if len(responseTypes) == 0 {
		responseTypes = []string{"code"}
	}
	redirectURIs := spec.RedirectURIs
	if redirectURIs == nil {
		redirectURIs = []string{}
	}
	scopes := spec.Scopes
	if scopes == nil {
		scopes = []string{}
	}
	metadata := spec.Metadata
	if metadata == nil {
		// nil would encode as jsonb `null`; the 097 column default is '{}'.
		metadata = map[string]any{}
	}

	var client OAuthClient
	err := pool.QueryRow(ctx,
		`INSERT INTO context_oauth_clients
		    (client_id, client_secret_hash, label, created_by, created_by_principal,
		     redirect_uris, scopes, grant_types, response_types,
		     token_endpoint_auth_method, registration_source, metadata)
		VALUES ($1, $2, $3, NULLIF($4, '')::uuid,
		        (SELECT k.principal_id FROM context_api_keys k WHERE k.id = NULLIF($4, '')::uuid),
		        $5, $6, $7, $8, $9, $10, $11)
		RETURNING id, client_id, label, active, created_at,
		          redirect_uris, scopes, grant_types, response_types,
		          token_endpoint_auth_method, registration_source, updated_at`,
		clientID, secretHash, spec.Label, spec.CreatedBy,
		redirectURIs, scopes, grantTypes, responseTypes, authMethod, source, metadata,
	).Scan(&client.ID, &client.ClientID, &client.Label, &client.Active, &client.CreatedAt,
		&client.RedirectURIs, &client.Scopes, &client.GrantTypes, &client.ResponseTypes,
		&client.TokenEndpointAuthMethod, &client.RegistrationSource, &client.UpdatedAt)
	if err != nil {
		return nil, "", fmt.Errorf("oauth: create client: %w", err)
	}

	return &client, secret, nil
}

// GetOAuthClientByClientID returns the full client row (design 02 §4a) —
// the lookup 03 uses at /authorize + /token. Inactive clients report
// found=false (fail-closed): the WHERE clause carries `active = true`, so a
// deactivated row is indistinguishable from a missing one for every caller.
func GetOAuthClientByClientID(ctx context.Context, pool *pgxpool.Pool, clientID string) (*OAuthClient, bool, error) {
	var client OAuthClient
	err := pool.QueryRow(ctx,
		`SELECT id, client_id, label, active, created_at,
		        redirect_uris, scopes, grant_types, response_types,
		        token_endpoint_auth_method, registration_source, updated_at
		FROM context_oauth_clients
		WHERE client_id = $1 AND active = true`,
		clientID,
	).Scan(&client.ID, &client.ClientID, &client.Label, &client.Active, &client.CreatedAt,
		&client.RedirectURIs, &client.Scopes, &client.GrantTypes, &client.ResponseTypes,
		&client.TokenEndpointAuthMethod, &client.RegistrationSource, &client.UpdatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("oauth: get client: %w", err)
	}
	return &client, true, nil
}

// ClientAllowsRedirectURI reports whether uri is in the client's registered
// exact allowlist (OAuth 2.1 §5.4 — array membership, no substring/wildcard
// semantics). An empty allowlist matches NOTHING; 03 decides the fallback
// (static S2 list) at the call site, not here.
func ClientAllowsRedirectURI(ctx context.Context, pool *pgxpool.Pool, clientID, uri string) (bool, error) {
	var allowed bool
	err := pool.QueryRow(ctx,
		`SELECT EXISTS(
		    SELECT 1 FROM context_oauth_clients
		    WHERE client_id = $1 AND active = true AND $2 = ANY(redirect_uris))`,
		clientID, uri,
	).Scan(&allowed)
	if err != nil {
		return false, fmt.Errorf("oauth: check redirect uri: %w", err)
	}
	return allowed, nil
}

// ListOAuthClients returns all OAuth clients (existing wire fields unchanged,
// the 097 constraint fields ride along additively).
func ListOAuthClients(ctx context.Context, pool *pgxpool.Pool) ([]OAuthClient, error) {
	rows, err := pool.Query(ctx,
		`SELECT id, client_id, label, active, created_at,
		        redirect_uris, scopes, grant_types, response_types,
		        token_endpoint_auth_method, registration_source, updated_at
		FROM context_oauth_clients
		ORDER BY created_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("oauth: list clients: %w", err)
	}
	defer rows.Close()

	var clients []OAuthClient
	for rows.Next() {
		var c OAuthClient
		if err := rows.Scan(&c.ID, &c.ClientID, &c.Label, &c.Active, &c.CreatedAt,
			&c.RedirectURIs, &c.Scopes, &c.GrantTypes, &c.ResponseTypes,
			&c.TokenEndpointAuthMethod, &c.RegistrationSource, &c.UpdatedAt); err != nil {
			return nil, fmt.Errorf("oauth: scan client: %w", err)
		}
		clients = append(clients, c)
	}
	return clients, rows.Err()
}

// DeleteOAuthClient deactivates an OAuth client by client_id.
func DeleteOAuthClient(ctx context.Context, pool *pgxpool.Pool, clientID string) (bool, error) {
	tag, err := pool.Exec(ctx,
		`UPDATE context_oauth_clients SET active = false WHERE client_id = $1 AND active = true`,
		clientID,
	)
	if err != nil {
		return false, fmt.Errorf("oauth: delete client: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// ValidateOAuthClientSecret checks a client_id + secret pair (wired at
// /token in S6/W03-7). The stored SHA-256 hash is fetched and compared in
// Go via subtle.ConstantTimeCompare — NOT matched in SQL — so comparison
// cost never depends on how many bytes agree (design 03 RVW-Vollst-F4;
// design 02 §39 relies on this). An empty stored hash (public `none`
// client, 097 default '') never validates: hashSecret output is never
// empty, and the explicit guard keeps that invariant visible.
func ValidateOAuthClientSecret(ctx context.Context, pool *pgxpool.Pool, clientID, secret string) (bool, error) {
	var storedHash string
	err := pool.QueryRow(ctx,
		`SELECT client_secret_hash FROM context_oauth_clients WHERE client_id = $1 AND active = true`,
		clientID,
	).Scan(&storedHash)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("oauth: validate client secret: %w", err)
	}
	if storedHash == "" {
		return false, nil
	}
	return subtle.ConstantTimeCompare([]byte(hashSecret(secret)), []byte(storedHash)) == 1, nil
}

func hashSecret(secret string) string {
	h := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(h[:])
}
