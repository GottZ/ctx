// ctx-goldset — builds stage 1 of the retrieval gold set (design 04 §4.5):
// the query sets G-KI, G-Q and G-REAL plus the provenance stamp.
//
// Subcommands:
//
//	ki           draw known-item cases (title paraphrase + constructive label)
//	q            generate content-derived questions on-prem (raw + hand-check sample)
//	qfinal       drop hand-check rejects, trim to n, apply the seeded DERIV/HOLD split
//	real         draw real access-log queries with the redaction sweep
//	sess         build G-SESS: session-window questions, gold from reports + timestamps
//	mh           build G-MH: multi-hop questions over dream links at confidence >= 0.7
//	glob         build G-GLOB: aggregating questions over corpus tags, gold judged later
//	glob-konstr  build the G-GLOB floor check with gold from graph_cluster_member
//	pool         build the blind judgement template of a judged slice (-slice; default G-REAL)
//	judge        machine-judge the open cells on-prem (-llm) and calibrate them (-kappa)
//	ingest       read the filled-in judgements back in as relevance labels of that slice
//	stamp        refresh file digests and the corpus contamination stamp
//
// The four multi-gold slices (design/05 §4.5) exist because a one-gold slice
// cannot show the use of an aggregating layer — it can only punish it. Their
// questions are written by the same on-prem chain as `q`, and `glob-konstr` is
// a declared FLOOR CHECK: its gold is circular against the graph layer, so it
// is reported but never a rollout criterion.
//
// Every write is confined to the gold directory; the only override is
// -allow-outside-goldset and it is recorded in the stamp. The database
// connection is read-only.
//
// Part of ctx by GottZ — The memory your LLM pretends to have.
// Source: https://github.com/GottZ/ctx
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"math"
	"os"
	"runtime/debug"
	"strings"
	"time"

	"github.com/GottZ/ctx/internal/goldset"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "ctx-goldset:", err)
		if errors.Is(err, goldset.ErrNotOnPrem) || errors.Is(err, goldset.ErrOutsideGoldset) {
			os.Exit(2)
		}
		os.Exit(1)
	}
}

// common carries the flags every subcommand shares.
type common struct {
	dir          string
	allowOutside bool
	envFile      string
	dsn          string
	seed         int64
	splitSeed    int64
}

// defaultSliceN is the target case count per new slice (design/05 §4.5).
//
// The floor check gets the smallest target, and that is a corpus fact rather
// than a preference: the live graph holds 54 clusters, 37 of them with at least
// three retrievable members, and the gold cap leaves 23 — three aspects each.
// A floor check only has to be big enough to read.
func defaultSliceN(cmd string) int {
	switch cmd {
	case "sess":
		return 120
	case "mh":
		return 100
	case "glob":
		return 80
	default:
		return 50
	}
}

// bindSlices binds the flags the four multi-gold generators share.
func bindSlices(fs *flag.FlagSet, o *slicesOpts, cmd string) {
	fs.IntVar(&o.n, "n", defaultSliceN(cmd), "target case count")
	fs.IntVar(&o.minContent, "min-content", 400, "minimum block content length")
	fs.StringVar(&o.backend, "backend", "spark-chat", "context_backends row supplying the generator")
	fs.StringVar(&o.model, "model", "", "model id override (default: model_map.default)")
	fs.IntVar(&o.concurrency, "concurrency", 2, "parallel LLM calls (capped at 2 — the endpoint is production serving)")
	fs.IntVar(&o.timeoutSec, "timeout", 180, "per-call timeout in seconds")
	fs.IntVar(&o.maxGold, "max-gold", 40, "drop a case whose constructive gold set exceeds this (it would measure coverage, not ranking)")
	fs.BoolVar(&o.dryRun, "dry-run", false, "draw and count the candidates, make no model call, write nothing")
}

func (c *common) bind(fs *flag.FlagSet) {
	fs.StringVar(&c.dir, "dir", "", "gold directory (default: .project/"+goldset.DirName+" next to the repo)")
	fs.BoolVar(&c.allowOutside, "allow-outside-goldset", false, "permit writes outside the gold directory (recorded in the stamp)")
	fs.StringVar(&c.envFile, "env-file", "/compose/n8n/.env", "env file supplying CONTEXT_DB_*")
	fs.StringVar(&c.dsn, "dsn", "", "postgres DSN (default: built from CONTEXT_DB_*)")
	fs.Int64Var(&c.seed, "seed", 20260812, "sampling seed")
	fs.Int64Var(&c.splitSeed, "split-seed", 20260825, "seed for the G-Q DERIV/HOLD partition")
}

func run() error {
	if len(os.Args) < 2 {
		return fmt.Errorf("usage: ctx-goldset <ki|q|qfinal|real|sess|mh|glob|glob-konstr|pool|judge|ingest|stamp> [flags]")
	}
	cmd := os.Args[1]
	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	var c common
	c.bind(fs)

	switch cmd {
	case "ki":
		n := fs.Int("n", 300, "target case count")
		minContent := fs.Int("min-content", 200, "minimum block content length")
		if err := fs.Parse(os.Args[2:]); err != nil {
			return err
		}
		return cmdKI(&c, *n, *minContent)
	case "q":
		n := fs.Int("n", 225, "blocks to attempt (target n plus reserve)")
		minContent := fs.Int("min-content", 400, "minimum block content length")
		backend := fs.String("backend", "spark-chat", "context_backends row supplying the generator")
		model := fs.String("model", "", "model id override (default: model_map.default)")
		conc := fs.Int("concurrency", 2, "parallel LLM calls (capped at 2 — the endpoint is production serving)")
		timeout := fs.Int("timeout", 180, "per-call timeout in seconds")
		check := fs.Float64("handcheck-frac", 0.10, "fraction of raw cases written to the hand-check sample")
		if err := fs.Parse(os.Args[2:]); err != nil {
			return err
		}
		return cmdQ(&c, qOpts{n: *n, minContent: *minContent, backend: *backend, model: *model,
			concurrency: *conc, timeout: time.Duration(*timeout) * time.Second, checkFrac: *check})
	case "qfinal":
		n := fs.Int("n", 200, "final case count")
		drop := fs.String("drop", "", "comma-separated raw indices rejected by the hand check")
		if err := fs.Parse(os.Args[2:]); err != nil {
			return err
		}
		return cmdQFinal(&c, *n, *drop)
	case "real":
		n := fs.Int("n", 150, "target case count")
		days := fs.Int("days", 180, "access-log window in days")
		minLen := fs.Int("min-len", 3, "minimum query length (exclusive)")
		if err := fs.Parse(os.Args[2:]); err != nil {
			return err
		}
		return cmdReal(&c, *n, *days, *minLen)
	case "sess", "mh", "glob", "glob-konstr":
		o := slicesOpts{}
		bindSlices(fs, &o, cmd)
		spanLen := fs.Int("span", 3, "sess: reported days per span window (span windows fill the gap between reports and target n)")
		minBlocks := fs.Int("min-blocks", 8, "glob: minimum retrievable blocks a tag must carry")
		minMembers := fs.Int("min-members", 3, "glob-konstr: minimum retrievable members a cluster must carry")
		titles := fs.Int("titles", 12, "glob/glob-konstr: member titles shown to the generator")
		if err := fs.Parse(os.Args[2:]); err != nil {
			return err
		}
		switch cmd {
		case "sess":
			return cmdSess(&c, o, *spanLen)
		case "mh":
			return cmdMH(&c, o)
		case "glob":
			return cmdGlob(&c, o, *minBlocks, *titles)
		default:
			return cmdGlobKonstr(&c, o, *minMembers, *titles)
		}
	case "pool":
		poolFile := fs.String("pool", "", "Pool-Datei aus `ctx-armsweep prime` (Vorgabe: die einzige pool-*.jsonl im Gold-Verzeichnis)")
		control := fs.Int("control", 5, "gleichverteilt gezogene Kontroll-Blöcke je Query (deklarierte Rest-Verzerrungs-Sonde)")
		excerpt := fs.Int("excerpt", 600, "Zeichen Blockinhalt je Kandidat in der Vorlage")
		out := fs.String("out", "", "Basisname der Urteils-Vorlage (Vorgabe: judge-<Lauf-ID>, "+
			"bei anderen Slices judge-<slice>-<Lauf-ID>)")
		slice := fs.String("slice", "", "geurteilter Slice, für den die Vorlage gebaut wird "+
			"(Vorgabe: "+goldset.SliceReal+"; poolbar außerdem: "+goldset.SliceGlob+")")
		dry := fs.Bool("dry-run", false, "nur die Kennzahlen melden, nichts schreiben")
		if err := fs.Parse(os.Args[2:]); err != nil {
			return err
		}
		return cmdPool(&c, poolOpts{poolFile: *poolFile, out: *out, slice: *slice,
			control: *control, excerpt: *excerpt, dryRun: *dry})
	case "judge":
		llm := fs.Bool("llm", false, "Urteilslauf über die on-prem-Kette (resume-fähig über das Journal)")
		kappa := fs.Bool("kappa", false, "Cohens κ + Kipp-Report aus dem ausgefüllten Kontrollbogen")
		tpl := fs.String("template", "", "Urteils-Vorlage aus `ctx-goldset pool`")
		key := fs.String("key", "", "Schlüsseldatei der Vorlage (Vorgabe: "+keyPrefix+"<Lauf-ID>.json)")
		controls := fs.String("controls", "", "ausgefüllter Kontrollbogen (bei -kappa)")
		out := fs.String("out", "", "Basisname der Ausgaben (Vorgabe: judged-<Lauf-ID> bzw. kappa-<Lauf-ID>)")
		backend := fs.String("backend", "spark-chat", "context_backends-Zeile, die den Urteiler stellt")
		model := fs.String("model", "", "Modell-ID (Vorgabe: model_map.default)")
		timeout := fs.Int("timeout", 180, "Zeitlimit je Aufruf in Sekunden")
		// No default: the threshold is a rule stated BEFORE the run, and a
		// default would be this tool inventing the rule it measures against.
		kappaMin := fs.Float64("kappa-min", math.NaN(), "κ-Schranke — PFLICHTANGABE bei -kappa/-calibrate, kein Vorgabewert (D-05 §4.5 (3))")
		stampName := fs.String("stamp", goldset.FileStamp, "Stempel, in den die Urteils-Provenienz gemischt wird")
		// Wave C3-4a (design/05a §C3-2-D05-8 l).
		draw := fs.Bool("draw", false, "C3-4a: Kern + Schicht-Stichprobe ziehen, blinden Bogen und Ziehungs-Schlüssel schreiben")
		calibrate := fs.Bool("calibrate", false, "C3-4a: ausgefüllten blinden Bogen zurückführen (κ, κ_w, ρ, π, `?`-Rate, Kipp-Report)")
		gold := fs.Bool("gold", false, "C3-4a: die zwei benannten Gold-Varianten schreiben (fable-kern + judge-uebertragen)")
		sliceFile := fs.String("slice", "", "zu labelnde Slice-Datei bei -gold (Vorgabe: "+goldset.FileReal+"); sie wird NUR gelesen")
		judged := fs.String("judged", "", "geurteilter Bestand aus `judge -llm` (bei -draw)")
		poolFile := fs.String("pool", "", "Pool-Datei zur Schichtung (Vorgabe: pool-<Lauf-ID>.jsonl)")
		labels := fs.String("labels", "", "X-W0-Regime-Labels (Vorgabe: "+goldset.FileRegimeLabels+")")
		sheet := fs.String("sheet", "", "ausgefüllter blinder Bogen (bei -calibrate)")
		drawKeyName := fs.String("draw-key", "", "Ziehungs-Schlüssel (Vorgabe: "+drawKeyPrefix+"<Lauf-ID>.json)")
		flip := fs.String("flip", "", "Metrik-Kipp-Ergebnisse je Slice aus `ctx-armsweep compare` (JSON); fehlt sie, ist das Gate nicht entschieden")
		coreQueries := fs.String("core-queries", "", "Kern-Ziehung als local,global (Vorgabe: 14,6; 0 zulässig für genau EIN Regime — Ein-Regime-Slices wie G-GLOB)")
		strata := fs.String("strata", "", "Schicht-Ziehung als S1,S2,S3,S4,S0 (Vorgabe: 120,140,140,80,60)")
		// No default: the seed fixes which queries become the metric anchor.
		drawSeed := fs.Int64("draw-seed", 0, "Ziehungs-Seed — PFLICHTANGABE bei -draw, sichtbare Lead-Entscheidung (§C3-2-D05-3)")
		rhoMin := fs.Float64("rho-min", -1, "ρ-Schranke (Vorgabe: 0.80)")
		piMin := fs.Float64("pi-min", -1, "π-Schranke (Vorgabe: 0.70)")
		unsureMax := fs.Float64("unsure-max", -1, "höchste zulässige `?`-Rate je Schicht (Vorgabe: 0.10)")
		if err := fs.Parse(os.Args[2:]); err != nil {
			return err
		}
		return cmdJudge(&c, judgeOpts{llm: *llm, kappa: *kappa, template: *tpl, key: *key,
			controls: *controls, out: *out, backend: *backend, model: *model,
			timeoutSec: *timeout, kappaMin: *kappaMin, stampName: *stampName,
			draw: *draw, calibrate: *calibrate, gold: *gold, slice: *sliceFile,
			judged: *judged, pool: *poolFile,
			labels: *labels, sheet: *sheet, drawKey: *drawKeyName, flip: *flip,
			coreQueries: *coreQueries, strata: *strata, drawSeed: *drawSeed,
			rhoMin: *rhoMin, piMin: *piMin, unsureMax: *unsureMax})
	case "ingest":
		judged := fs.String("judged", "", "ausgefüllte Urteils-Vorlage (JSONL oder Markdown)")
		key := fs.String("key", "", "Schlüsseldatei der Vorlage (Vorgabe: "+keyPrefix+"<Lauf-ID>.json)")
		out := fs.String("out", goldset.FileReal, "zu labelnde Slice-Datei")
		stampName := fs.String("stamp", goldset.FileStamp, "Stempel, in den der G-REAL-Steckbrief gemischt wird")
		if err := fs.Parse(os.Args[2:]); err != nil {
			return err
		}
		return cmdIngest(&c, *judged, *key, *out, *stampName)
	case "stamp":
		if err := fs.Parse(os.Args[2:]); err != nil {
			return err
		}
		return cmdStamp(&c)
	default:
		return fmt.Errorf("unknown subcommand %q", cmd)
	}
}

// ------------------------------------------------------------- plumbing.

func (c *common) guard() (*goldset.Guard, error) {
	dir := c.dir
	if dir == "" {
		dir = defaultGoldDir()
	}
	return goldset.NewGuard(dir, c.allowOutside)
}

// defaultGoldDir points at the private .project submodule. It is an absolute
// path on purpose: agent worktrees have no .project, and a relative default
// would quietly create a second gold directory inside a worktree.
func defaultGoldDir() string {
	if v := os.Getenv("CTX_GOLDSET_DIR"); v != "" {
		return v
	}
	return "/compose/n8n/.project/" + goldset.DirName
}

func (c *common) open(ctx context.Context) (*goldset.DB, error) {
	dsn := c.dsn
	if dsn == "" {
		var err error
		if dsn, err = dsnFromEnv(c.envFile); err != nil {
			return nil, err
		}
	}
	return goldset.Open(ctx, dsn)
}

// dsnFromEnv builds the read-only DSN from CONTEXT_DB_*, preferring the process
// environment and falling back to the env file.
func dsnFromEnv(envFile string) (string, error) {
	vals := map[string]string{}
	if b, err := os.ReadFile(envFile); err == nil {
		for _, line := range strings.Split(string(b), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			k, v, ok := strings.Cut(line, "=")
			if !ok {
				continue
			}
			vals[strings.TrimSpace(k)] = strings.Trim(strings.TrimSpace(v), `"'`)
		}
	}
	get := func(key, def string) string {
		if v := os.Getenv(key); v != "" {
			return v
		}
		if v := vals[key]; v != "" {
			return v
		}
		return def
	}
	user, pass := get("CONTEXT_DB_USER", ""), get("CONTEXT_DB_PASSWORD", "")
	db := get("CONTEXT_DB", "context_store")
	host := get("CONTEXT_GOLDSET_DB_HOST", get("CONTEXT_DB_HOST", "localhost"))
	port := get("CONTEXT_DB_PORT", "5432")
	if user == "" || pass == "" {
		return "", fmt.Errorf("CONTEXT_DB_USER/CONTEXT_DB_PASSWORD missing (env or %s)", envFile)
	}
	return fmt.Sprintf("postgres://%s:%s@%s:%s/%s?sslmode=disable",
		urlEscape(user), urlEscape(pass), host, port, db), nil
}

func urlEscape(s string) string {
	r := strings.NewReplacer("@", "%40", ":", "%3A", "/", "%2F", "?", "%3F", "#", "%23", "%", "%25")
	return r.Replace(s)
}

// buildRev reads the VCS stamp Go embedded at build time, with the dirty flag
// appended. Deliberately NOT `git rev-parse` in a subprocess: spawning one is
// an argued exception in this module (internal/llm/exec_ban_test.go) and a
// provenance field does not argue it.
//
// Caveat the stamp must not hide: Go resolves the repository by walking up from
// the package directory, and in a linked git worktree that walk can land on the
// enclosing checkout instead of the worktree. The value therefore identifies
// the BUILD, not necessarily the commit the gold set was drawn under — which is
// why the field is named for the build and carries "-dirty" verbatim.
func buildRev() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	rev, dirty := "", false
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			dirty = s.Value == "true"
		}
	}
	if rev != "" && dirty {
		return rev + "-dirty"
	}
	return rev
}

// updateStamp applies fn to the stamp on disk and writes it back.
func updateStamp(g *goldset.Guard, fn func(*goldset.Stamp)) error {
	p, err := g.Resolve(goldset.FileStamp)
	if err != nil {
		return err
	}
	s, err := goldset.ReadStamp(p)
	if err != nil {
		return err
	}
	if s.Version == 0 {
		s.Version = 1
	}
	if s.Slices == nil {
		s.Slices = map[string]goldset.SliceStamp{}
	}
	s.CreatedAt = time.Now().UTC().Format(time.RFC3339)
	s.BuildRev = buildRev()
	s.AllowOutsideGoldset = s.AllowOutsideGoldset || g.AllowOutside()
	fn(&s)
	return goldset.WriteStamp(p, s)
}

// writeSlice persists a slice file and records its digest and n in the stamp.
func writeSlice(g *goldset.Guard, c *common, name, file string, cases []goldset.Case, fill func(*goldset.SliceStamp)) error {
	p, err := g.Resolve(file)
	if err != nil {
		return err
	}
	if err := goldset.WriteJSONL(p, cases); err != nil {
		return err
	}
	digest, err := goldset.FileDigest(p)
	if err != nil {
		return err
	}
	return updateStamp(g, func(s *goldset.Stamp) {
		st := s.Slices[name]
		st.N, st.File, st.SHA256 = len(cases), file, digest
		if fill != nil {
			fill(&st)
		}
		s.Slices[name] = st
		s.SampleSeed, s.SplitSeed = c.seed, c.splitSeed
	})
}
