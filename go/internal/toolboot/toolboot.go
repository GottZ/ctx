package toolboot

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/store"
)

// Session is what a completed boot hands back: the env-layer config, the pool
// built from its DSN, and the one thing that has to happen on the way out.
//
// Stop is pool.Close and NOTHING else. A caller that also has a signal
// context to cancel registers that defer itself, in its own order — the two
// deferred calls are not the same concern and they are not always in the same
// order at the call sites.
type Session struct {
	Cfg  *config.Config
	Pool *pgxpool.Pool
	Stop func()
}

// Open runs the boot contract and hands the findings to the caller.
//
// report is called EXACTLY ONCE, before anything can abort: with every issue
// FromEnv and Validate produced (nil when there were none) and with the
// verdict of config.HasErrors as aborting. poolErr is called at most once,
// and only on the path where the config was fine and store.NewPool still
// failed — so a caller can tell the two failures apart without inspecting
// anything.
//
// The bool is the only thing to branch on: false means the boot did not
// happen and the reason already went to one of the two callbacks. There is no
// Session on that path and no pool to close.
func Open(ctx context.Context, report func(issues []config.Issue, aborting bool), poolErr func(error)) (*Session, bool) {
	cfg, issues := config.FromEnv()
	issues = append(issues, config.Validate(cfg)...)

	aborting := config.HasErrors(issues)
	report(issues, aborting)
	if aborting {
		return nil, false
	}

	pool, err := store.NewPool(ctx, cfg.DSN())
	if err != nil {
		poolErr(err)
		return nil, false
	}
	return &Session{Cfg: cfg, Pool: pool, Stop: pool.Close}, true
}
