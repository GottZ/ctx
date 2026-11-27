package store

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/pgxdb"
)

// RecordingAuditEntity is the context_settings_audit entity_type of a read of
// a recording's content (issue #575): every answer of model-recording-get,
// model-recording-read and model-recording-translate leaves one row before it
// is written — who (the acting key, its label and principal through
// audit_actor_meta), when, which recording (entity_key), which block, which
// action. Never the content: new_value carries the address and the window or
// the target language, nothing the recording holds. Server-admin only on the
// audit read surface (auditEntities): the reads are a server-admin's.
const RecordingAuditEntity = "model_recording"

// The actions of a recording audit row.
const (
	// RecordingAuditRead: the detail view — the blocks with their previews.
	RecordingAuditRead = "read"
	// RecordingAuditReadRaw: one window of a block or a body.
	RecordingAuditReadRaw = "read_raw"
	// RecordingAuditTranslate: one chunk of a block sent to a model of this
	// instance for a translation.
	RecordingAuditTranslate = "translate"
)

// RecordingAudit is one read of a recording's content.
type RecordingAudit struct {
	Stamp       AuditStamp
	RecordingID string
	ConsumerID  string
	Action      string
	// Detail is what the action addressed — the block address, the window,
	// the chunk and language; never content.
	Detail map[string]any
}

// WriteRecordingAudit commits one recording audit row; nil once committed.
// The caller answers with the content only after it returned nil.
func WriteRecordingAudit(ctx context.Context, pool *pgxpool.Pool, a RecordingAudit) error {
	nv, err := json.Marshal(a.Detail)
	if err != nil {
		return fmt.Errorf("store: recording audit value: %w", err)
	}
	return pgxdb.Write(ctx, pool, pgxdb.At("store: recording audit"), func(tx pgx.Tx) error {
		if err := stampAudit(ctx, tx, a.Stamp); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO context_settings_audit
			        (entity_type, entity_key, scope, action, new_value, api_key_id, actor_label, metadata)
			 SELECT $1::text, $2::text, '_global', $3::text, $4::jsonb, a.actor, a.actor_label,
			        a.meta || jsonb_build_object('consumer_id', $5::text)
			   FROM audit_actor_meta() a`,
			RecordingAuditEntity, a.RecordingID, a.Action, nv, a.ConsumerID); err != nil {
			return fmt.Errorf("store: recording audit: %w", err)
		}
		return nil
	})
}
