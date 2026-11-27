package llmlog

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Recording (issue #571, migration 197). A model consumer with the switch
// record_bodies on has every request it sends through the gateway held in
// context_llm_recordings: one row per request, keyed by the request's own
// id, with the body as the gateway read it, byte for byte (bytea — no
// cleaning, no cut, no seal), the answer the client got (an embeddings
// answer without its vectors: the input is the record), and the outcome.
// The gateway writes the row synchronously and in order (internal/oai
// record.go): Open commits the request BEFORE the first backend attempt —
// a request that cannot be held is not served —, Finish ends it with its
// outcome (a non-streamed answer before it is written to the client), and
// Insert writes the row of a request the gateway refused before it opened
// one. Nothing here is queued: an error is the caller's, who answers the
// request with 503 recording_unavailable or counts the request as one it
// could not hold.
//
// A row can carry credentials in clear text. No ctx surface reads its
// bodies — no REST, MCP, CLI or web route, no export, no pipeline
// (TestRecordingsReadersCensus); a server operator reads them in the
// database (docs/operations.md "Recording a consumer"). The retention
// janitor NULLs them after the LLM log's retention (EvictBodies).

// Recording states (context_llm_recordings.state).
const (
	// RecordingOpen: the request is held; its outcome is not recorded (yet).
	RecordingOpen = "open"
	// RecordingDone: the outcome is recorded.
	RecordingDone = "done"
)

// ProcessID names this ctxd process on every recording row it writes
// (context_llm_recordings.process_id): GET /api/status counts the same
// process's admissions, so the rows of a process can be checked against its
// count (docs/operations.md "Recording a consumer").
var ProcessID = newProcessID()

func newProcessID() string {
	id, err := uuid.NewV7()
	if err != nil {
		return uuid.NewString()
	}
	return id.String()
}

// RecordedRequest is a recording row's request side — what the door and the
// route know of the request.
type RecordedRequest struct {
	// ID is the request's id (a UUID; the route's answer id carries it).
	ID string
	// ConsumerID and ModelKeyID come from the door (UUIDs; ModelKeyID may
	// be empty).
	ConsumerID, ModelKeyID string
	// Path is the request's path; Op the route that took it (GatewayOp*),
	// "" when no route did (a refusal of a stage, an unknown path).
	Path, Op string
	// ClientRequestID is the client's request id header as sent ("" none).
	ClientRequestID string
	// Model and Stream are what the route's parse read (nil Stream and ""
	// Model: not parsed).
	Model  string
	Stream *bool
	// Declared is the request's Content-Length (-1: none declared).
	Declared int64
	// Body is what the gateway read of the body (nil: nothing); Complete
	// reports that it is the whole body.
	Body     []byte
	Complete bool
}

// RecordedOutcome is a recording row's outcome.
type RecordedOutcome struct {
	// Status is the HTTP status the client got (0: the gateway wrote
	// nothing — stored NULL).
	Status int
	// Code is the gateway's code of a refusal or an error ("" none).
	Code string
	// Response is the answer as the client got it (nil: none recorded).
	Response []byte
	// Supersedes: this outcome replaces one committed before — an answer
	// whose commit went through (or may have: its acknowledgement was lost)
	// and which the client then did not get (internal/oai record.go
	// withhold). Finish writes it over a done row as over an open one.
	Supersedes bool
}

// Recordings writes context_llm_recordings over Pool.
type Recordings struct{ Pool *pgxpool.Pool }

// errRecordingPool is the error of a Recordings without a pool.
var errRecordingPool = errors.New("llmlog: recordings without a database pool")

func nullText(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func nullUUIDText(s string) *string {
	if _, err := uuid.Parse(s); err != nil {
		return nil
	}
	return &s
}

func nullStatus(n int) *int16 {
	if n <= 0 || n > 999 {
		return nil
	}
	v := int16(n)
	return &v
}

// bodyBytes is b, NULL when empty: an empty body is no body.
func bodyBytes(b []byte) []byte {
	if len(b) == 0 {
		return nil
	}
	return b
}

func declared(n int64) *int64 {
	if n < 0 {
		return nil
	}
	return &n
}

// Open inserts the request's row in state open, its body whole; nil once the
// insert is committed. ctx bounds it (the caller's deadline).
func (r Recordings) Open(ctx context.Context, q RecordedRequest) error {
	if r.Pool == nil {
		return errRecordingPool
	}
	_, err := r.Pool.Exec(ctx, `INSERT INTO context_llm_recordings
		(id, process_id, consumer_id, model_key_id, path, op, client_request_id, model, stream,
		 declared_bytes, request_bytes, request_complete, request_body, state)
		VALUES ($1::uuid, $2::uuid, $3::uuid, $4::uuid, $5, $6, $7, $8, $9, $10, $11, $12, $13, 'open')`,
		q.ID, ProcessID, q.ConsumerID, nullUUIDText(q.ModelKeyID), q.Path, nullText(q.Op), nullText(q.ClientRequestID),
		nullText(q.Model), q.Stream, declared(q.Declared), int64(len(q.Body)), q.Complete, bodyBytes(q.Body))
	if err != nil {
		return fmt.Errorf("llmlog: open recording: %w", err)
	}
	return nil
}

// Finish ends the open row id with its outcome (state done); nil once the
// update is committed. A row that is not open is an error: an outcome is
// written once — unless it supersedes a withheld answer (o.Supersedes),
// which it replaces on the done row.
func (r Recordings) Finish(ctx context.Context, id string, o RecordedOutcome) error {
	if r.Pool == nil {
		return errRecordingPool
	}
	tag, err := r.Pool.Exec(ctx, `UPDATE context_llm_recordings
		SET state = 'done', finished_at = now(), status = $2, code = $3, response_bytes = $4, response_body = $5
		WHERE id = $1::uuid AND (state = 'open' OR $6)`,
		id, nullStatus(o.Status), nullText(o.Code), int64(len(o.Response)), bodyBytes(o.Response), o.Supersedes)
	if err != nil {
		return fmt.Errorf("llmlog: finish recording: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("llmlog: finish recording %s: no open row", id)
	}
	return nil
}

// Insert writes the row of a request that was never opened — refused before
// its walk —, already done with its outcome; nil once committed. An open row
// of the same id (an Open whose commit the caller did not see) is ended
// instead, its request kept.
func (r Recordings) Insert(ctx context.Context, q RecordedRequest, o RecordedOutcome) error {
	if r.Pool == nil {
		return errRecordingPool
	}
	_, err := r.Pool.Exec(ctx, `INSERT INTO context_llm_recordings AS t
		(id, process_id, consumer_id, model_key_id, path, op, client_request_id, model, stream,
		 declared_bytes, request_bytes, request_complete, request_body, state, finished_at,
		 status, code, response_bytes, response_body)
		VALUES ($1::uuid, $2::uuid, $3::uuid, $4::uuid, $5, $6, $7, $8, $9, $10, $11, $12, $13, 'done', now(),
		        $14, $15, $16, $17)
		ON CONFLICT (id) DO UPDATE
		   SET state = 'done', finished_at = now(), status = EXCLUDED.status, code = EXCLUDED.code,
		       response_bytes = EXCLUDED.response_bytes, response_body = EXCLUDED.response_body
		 WHERE t.state = 'open'`,
		q.ID, ProcessID, q.ConsumerID, nullUUIDText(q.ModelKeyID), q.Path, nullText(q.Op), nullText(q.ClientRequestID),
		nullText(q.Model), q.Stream, declared(q.Declared), int64(len(q.Body)), q.Complete, bodyBytes(q.Body),
		nullStatus(o.Status), nullText(o.Code), int64(len(o.Response)), bodyBytes(o.Response))
	if err != nil {
		return fmt.Errorf("llmlog: insert recording: %w", err)
	}
	return nil
}

// evictRecordingBodies is EvictBodies' statement over the recordings: the two
// bodies of every row past the retention that still carries its request
// (idx_llm_recordings_bodies holds exactly those); the sizes stay.
const evictRecordingBodies = `UPDATE context_llm_recordings
	SET request_body = NULL, response_body = NULL
	WHERE created_at < now() - make_interval(days => $1)
	  AND request_body IS NOT NULL`
