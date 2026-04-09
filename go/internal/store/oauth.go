package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// OAuthClient represents a registered OAuth client.
type OAuthClient struct {
	ID         string    `json:"id"`
	ClientID   string    `json:"client_id"`
	Label      string    `json:"label"`
	Active     bool      `json:"active"`
	CreatedBy  *string   `json:"created_by,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
}

// CreateOAuthClient generates a new client_id + client_secret pair and stores it.
// Returns the client and the plaintext secret (shown once, then only hash stored).
func CreateOAuthClient(ctx context.Context, pool *pgxpool.Pool, label string, createdBy string) (*OAuthClient, string, error) {
	clientIDBytes := make([]byte, 16)
	secretBytes := make([]byte, 32)
	if _, err := rand.Read(clientIDBytes); err != nil {
		return nil, "", fmt.Errorf("oauth: generate client_id: %w", err)
	}
	if _, err := rand.Read(secretBytes); err != nil {
		return nil, "", fmt.Errorf("oauth: generate secret: %w", err)
	}

	clientID := "ctx_" + hex.EncodeToString(clientIDBytes)
	secret := hex.EncodeToString(secretBytes)
	secretHash := hashSecret(secret)

	var client OAuthClient
	err := pool.QueryRow(ctx,
		`INSERT INTO context_oauth_clients (client_id, client_secret_hash, label, created_by)
		VALUES ($1, $2, $3, $4::uuid)
		RETURNING id, client_id, label, active, created_at`,
		clientID, secretHash, label, createdBy,
	).Scan(&client.ID, &client.ClientID, &client.Label, &client.Active, &client.CreatedAt)
	if err != nil {
		return nil, "", fmt.Errorf("oauth: create client: %w", err)
	}

	return &client, secret, nil
}

// ListOAuthClients returns all active OAuth clients.
func ListOAuthClients(ctx context.Context, pool *pgxpool.Pool) ([]OAuthClient, error) {
	rows, err := pool.Query(ctx,
		`SELECT id, client_id, label, active, created_at
		FROM context_oauth_clients
		ORDER BY created_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("oauth: list clients: %w", err)
	}
	defer rows.Close()

	var clients []OAuthClient
	for rows.Next() {
		var c OAuthClient
		if err := rows.Scan(&c.ID, &c.ClientID, &c.Label, &c.Active, &c.CreatedAt); err != nil {
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

// ValidateOAuthClient checks if a client_id exists and is active.
func ValidateOAuthClient(ctx context.Context, pool *pgxpool.Pool, clientID string) (bool, error) {
	var exists bool
	err := pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM context_oauth_clients WHERE client_id = $1 AND active = true)`,
		clientID,
	).Scan(&exists)
	return exists, err
}

// ValidateOAuthClientSecret checks client_id + secret pair.
func ValidateOAuthClientSecret(ctx context.Context, pool *pgxpool.Pool, clientID, secret string) (bool, error) {
	secretHash := hashSecret(secret)
	var exists bool
	err := pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM context_oauth_clients WHERE client_id = $1 AND client_secret_hash = $2 AND active = true)`,
		clientID, secretHash,
	).Scan(&exists)
	return exists, err
}

func hashSecret(secret string) string {
	h := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(h[:])
}
