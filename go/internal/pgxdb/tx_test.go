package pgxdb

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// fakeTx embeds the interface so only the three methods the bracket actually
// drives need a body — every other call would panic and thereby prove that
// the helper does more than begin, rollback and commit.
type fakeTx struct {
	pgx.Tx
	commitErr error
	commits   int
	rollbacks int
	// Der Rollback-Context wird IM Rollback ausgewertet, nicht danach: der
	// Helfer cancelt ihn beim Verlassen des defer, wie es sich gehört.
	rollbackErr      error
	rollbackDeadline time.Time
	rollbackHadFrist bool
}

func (f *fakeTx) Commit(context.Context) error {
	f.commits++
	return f.commitErr
}

func (f *fakeTx) Rollback(ctx context.Context) error {
	f.rollbacks++
	f.rollbackErr = ctx.Err()
	f.rollbackDeadline, f.rollbackHadFrist = ctx.Deadline()
	return nil
}

type fakeBeginner struct {
	tx       *fakeTx
	beginErr error
	begins   int
	gotOpts  pgx.TxOptions
}

func (f *fakeBeginner) BeginTx(_ context.Context, opts pgx.TxOptions) (pgx.Tx, error) {
	f.begins++
	f.gotOpts = opts
	if f.beginErr != nil {
		return nil, f.beginErr
	}
	return f.tx, nil
}

func newFake() *fakeBeginner { return &fakeBeginner{tx: &fakeTx{}} }

var errBoom = errors.New("boom")

func TestAtBildetDasHaeufigePaar(t *testing.T) {
	s := At("dream")
	if s.Begin != "dream: begin" || s.Commit != "dream: commit" {
		t.Fatalf("At(dream) = %+v, erwartet {dream: begin dream: commit}", s)
	}
}

// TestBeginFehlerTraegtDenAufruferText pinnt die Form, mit der die beiden
// abgelösten readOnly-Kopien ihren Begin-Fehler beschriftet haben.
func TestBeginFehlerTraegtDenAufruferText(t *testing.T) {
	db := newFake()
	db.beginErr = errBoom
	err := Read(context.Background(), db, Stages{Begin: "begin"}, func(pgx.Tx) error {
		t.Fatal("fn darf bei einem Begin-Fehler nicht laufen")
		return nil
	})
	if err == nil || err.Error() != "begin: boom" {
		t.Fatalf("Fehlertext = %v, erwartet \"begin: boom\"", err)
	}
	if !errors.Is(err, errBoom) {
		t.Fatal("errors.Is über die Verpackung hinweg gebrochen")
	}
	// Genau EIN %w: errors.Unwrap muss den Basisfehler selbst liefern. Ein
	// Umbau auf errors.Join überlebte errors.Is, bräche diesen Vertrag aber.
	if errors.Unwrap(err) != errBoom { //nolint:errorlint // Identität ist genau die Zusage
		t.Fatalf("errors.Unwrap = %v, erwartet exakt errBoom (ein %%w, kein Join)", errors.Unwrap(err))
	}
}

// TestPanikInFnRollbacktUndPropagiert pinnt das Verhalten der abgelösten
// Kopien: eine Panik aus fn läuft unverändert durch, der deferierte Rollback
// fährt genau einmal, committet wird nicht.
func TestPanikInFnRollbacktUndPropagiert(t *testing.T) {
	db := newFake()
	defer func() {
		r := recover()
		if r != "boom-panic" {
			t.Fatalf("recover() = %v, erwartet \"boom-panic\" — die Panik muss unverändert durch", r)
		}
		if db.tx.commits != 0 {
			t.Fatal("nach einer Panik darf nicht committet werden")
		}
		if db.tx.rollbacks != 1 {
			t.Fatalf("rollbacks = %d, erwartet 1", db.tx.rollbacks)
		}
	}()
	_ = Write(context.Background(), db, At("dream"), func(pgx.Tx) error { panic("boom-panic") })
	t.Fatal("die Panik wurde verschluckt")
}

func TestLeeresBeginFeldReichtUnverpacktDurch(t *testing.T) {
	db := newFake()
	db.beginErr = errBoom
	err := Write(context.Background(), db, Stages{}, func(pgx.Tx) error { return nil })
	if err != errBoom { //nolint:errorlint // Identität ist genau die Zusage
		t.Fatalf("Fehler = %v (%T), erwartet exakt errBoom", err, err)
	}
}

func TestCommitFehlerTraegtDenAufruferText(t *testing.T) {
	db := newFake()
	db.tx.commitErr = errBoom
	err := Write(context.Background(), db, At("dream"), func(pgx.Tx) error { return nil })
	if err == nil || err.Error() != "dream: commit: boom" {
		t.Fatalf("Fehlertext = %v, erwartet \"dream: commit: boom\"", err)
	}
}

func TestLeeresCommitFeldReichtUnverpacktDurch(t *testing.T) {
	db := newFake()
	db.tx.commitErr = errBoom
	err := Read(context.Background(), db, Stages{Begin: "begin"}, func(pgx.Tx) error { return nil })
	if err != errBoom { //nolint:errorlint // Identität ist genau die Zusage
		t.Fatalf("Fehler = %v (%T), erwartet exakt errBoom", err, err)
	}
}

// TestErfolgreicherCommitLiefertNil ist die Regressions-Probe gegen die
// Verpackungs-Falle: fmt.Errorf("%s: %w", label, nil) baut einen NICHT-nil
// Fehler aus einem geglückten Commit.
func TestErfolgreicherCommitLiefertNil(t *testing.T) {
	db := newFake()
	if err := Write(context.Background(), db, At("dream"), func(pgx.Tx) error { return nil }); err != nil {
		t.Fatalf("geglückter Commit lieferte %v, erwartet nil", err)
	}
	if db.tx.commits != 1 {
		t.Fatalf("commits = %d, erwartet 1", db.tx.commits)
	}
}

func TestFnFehlerWirdUnveraendertDurchgereicht(t *testing.T) {
	db := newFake()
	err := Read(context.Background(), db, At("armcost"), func(pgx.Tx) error { return errBoom })
	if err != errBoom { //nolint:errorlint // Identität ist genau die Zusage
		t.Fatalf("Fehler = %v (%T), erwartet exakt errBoom — kein %%w-Wrapping um fn", err, err)
	}
	if db.tx.commits != 0 {
		t.Fatal("nach einem fn-Fehler darf nicht committet werden")
	}
	if db.tx.rollbacks != 1 {
		t.Fatalf("rollbacks = %d, erwartet 1", db.tx.rollbacks)
	}
}

// TestRollbackLaeuftAufEinemLebendenContext belegt die Rollback-Politik: der
// deferierte Rollback darf NICHT auf dem gecancelten Aufrufer-Context fahren,
// sonst schließt pgx die Verbindung statt sie zurückzurollen.
func TestRollbackLaeuftAufEinemLebendenContext(t *testing.T) {
	db := newFake()
	ctx, cancel := context.WithCancel(context.Background())
	err := Write(ctx, db, At("dream"), func(pgx.Tx) error {
		cancel()
		return errBoom
	})
	if err != errBoom { //nolint:errorlint // Identität ist genau die Zusage
		t.Fatalf("Fehler = %v, erwartet exakt errBoom", err)
	}
	if db.tx.rollbacks != 1 {
		t.Fatalf("rollbacks = %d, erwartet 1", db.tx.rollbacks)
	}
	if db.tx.rollbackErr != nil {
		t.Fatalf("Rollback fuhr auf einem toten Context: %v", db.tx.rollbackErr)
	}
	if !db.tx.rollbackHadFrist {
		t.Fatal("Rollback-Context ohne Frist — rollbackGrace greift nicht")
	}
	if rest := time.Until(db.tx.rollbackDeadline); rest <= 0 || rest > rollbackGrace {
		t.Fatalf("Rollback-Frist = %v, erwartet (0, %v]", rest, rollbackGrace)
	}
}

func TestReadOeffnetReadOnlyUndWriteNicht(t *testing.T) {
	db := newFake()
	if err := Read(context.Background(), db, Stages{}, func(pgx.Tx) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if db.gotOpts.AccessMode != pgx.ReadOnly {
		t.Fatalf("Read AccessMode = %q, erwartet %q", db.gotOpts.AccessMode, pgx.ReadOnly)
	}
	if db.gotOpts.IsoLevel != "" {
		t.Fatalf("Read setzt ungefragt IsoLevel = %q", db.gotOpts.IsoLevel)
	}

	db2 := newFake()
	if err := Write(context.Background(), db2, Stages{}, func(pgx.Tx) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if db2.gotOpts != (pgx.TxOptions{}) {
		t.Fatalf("Write TxOptions = %+v, erwartet den Nullwert", db2.gotOpts)
	}
}

func TestWriteOptsReichtDieOptionenDurch(t *testing.T) {
	db := newFake()
	opts := pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}
	if err := WriteOpts(context.Background(), db, opts, Stages{}, func(pgx.Tx) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if db.gotOpts != opts {
		t.Fatalf("TxOptions = %+v, erwartet %+v", db.gotOpts, opts)
	}
}
