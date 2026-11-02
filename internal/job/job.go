// Package job orchestrates one fetch-merge-publish run and writes
// health.json.
package job

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"sync"
	"time"

	"github.com/justskiv/rates/internal/catalog"
	"github.com/justskiv/rates/internal/merge"
	"github.com/justskiv/rates/internal/policy"
	"github.com/justskiv/rates/internal/publish"
	"github.com/justskiv/rates/internal/rates"
	"github.com/justskiv/rates/internal/source"
	"github.com/justskiv/rates/internal/state"
	"github.com/justskiv/rates/internal/validate"
)

// Outcome is the result of a run as reported in health.json.
type Outcome string

// Outcomes of a run. Skipped means another run held the lock; nothing was
// written.
const (
	OutcomePublished Outcome = "published"
	OutcomeUnchanged Outcome = "unchanged"
	OutcomeAborted   Outcome = "aborted"
	OutcomeFailed    Outcome = "failed"
	OutcomeSkipped   Outcome = "skipped"
)

// Gate reasons on top of merge.AbortReason.
const (
	ReasonSchemaViolation = "schema_violation"
	ReasonStaleCandidate  = "stale_candidate"
	ReasonDegraded        = "degraded"
)

// Fetcher downloads one upstream; *source.Fetcher implements it.
type Fetcher interface {
	Fetch(ctx context.Context, c source.Cache) (source.Quote, error)
}

// Config is the runtime configuration of a run.
type Config struct {
	PublicDir     string
	StateDir      string
	ArchiveDir    string
	OverridesFile string
	Precompress   bool
	Timeout       time.Duration
	Version       string
}

// Deps are the collaborators of a run.
type Deps struct {
	Primary   Fetcher // Frankfurter
	Secondary Fetcher // fawazahmed0
	Voter     Fetcher // exchangerate-api
	Catalog   *catalog.Catalog
	Policy    policy.Policy
	Log       *slog.Logger
	Now       func() time.Time
}

// Job runs one fetch-merge-publish cycle.
type Job struct {
	cfg  Config
	deps Deps
}

// New checks the configuration and builds a Job.
func New(cfg Config, deps Deps) (*Job, error) {
	switch {
	case cfg.PublicDir == "" || cfg.StateDir == "" || cfg.ArchiveDir == "":
		return nil, errors.New("job: public, state and archive directories are required")
	case deps.Primary == nil || deps.Secondary == nil || deps.Voter == nil:
		return nil, errors.New("job: all three fetchers are required")
	case deps.Catalog == nil:
		return nil, errors.New("job: catalog is required")
	}
	if deps.Log == nil {
		deps.Log = slog.New(slog.DiscardHandler)
	}
	if deps.Now == nil {
		deps.Now = time.Now
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 60 * time.Second
	}
	return &Job{cfg: cfg, deps: deps}, nil
}

// Report summarizes a finished run.
type Report struct {
	Outcome  Outcome
	Reason   string
	Health   Health
	Duration time.Duration
}

// Run executes one cycle. The error is non-nil only for OutcomeFailed: an
// I/O error or a bug. Aborts are normal outcomes reported in health.json.
func (j *Job) Run(ctx context.Context) (Report, error) {
	start := j.deps.Now()
	r, err := j.open()
	if err != nil {
		return Report{Outcome: OutcomeFailed, Reason: err.Error()}, err
	}
	defer r.close()

	unlock, err := r.store.Lock()
	if errors.Is(err, state.ErrLocked) {
		j.deps.Log.Info("another run holds the lock, skipping")
		return Report{Outcome: OutcomeSkipped}, nil
	}
	if err != nil {
		return Report{Outcome: OutcomeFailed, Reason: err.Error()}, fmt.Errorf("lock state: %w", err)
	}
	defer func() { _ = unlock() }()

	rep, err := r.execute(ctx, start)
	rep.Duration = j.deps.Now().Sub(start)
	if err != nil {
		rep.Outcome, rep.Reason = OutcomeFailed, err.Error()
		if ctx.Err() == nil {
			// Best effort: monitoring should see the failure, not silence.
			rep.Health = r.health(start, rep)
			if herr := r.writeHealth(rep.Health); herr != nil {
				err = errors.Join(err, herr)
			}
		}
	}
	j.logSummary(rep)
	return rep, err
}

func (j *Job) logSummary(rep Report) {
	lvl := slog.LevelInfo
	if rep.Outcome == OutcomeAborted || rep.Outcome == OutcomeFailed {
		lvl = slog.LevelWarn
	}
	j.deps.Log.Log(context.Background(), lvl, "run done",
		slog.String("outcome", string(rep.Outcome)),
		slog.String("reason", rep.Reason),
		slog.String("asOf", formatTime(rep.Health.AsOf)),
		slog.Int("published", rep.Health.Counts.Published),
		slog.Int("held", rep.Health.Counts.Held),
		slog.Int("disputed", rep.Health.Counts.Disputed),
		slog.String("dur", rep.Duration.Round(time.Millisecond).String()),
	)
}

// run holds the resources and the working state of one execution.
type run struct {
	cfg     Config
	deps    Deps
	public  *publish.Dir
	archive *publish.Dir
	store   *state.Store

	st        state.State
	current   previous // what clients see; updated after publishing
	attention []Attention
	sources   map[source.ID]SourceHealth
}

func (j *Job) open() (*run, error) {
	public, err := publish.Open(j.cfg.PublicDir, j.cfg.Precompress)
	if err != nil {
		return nil, fmt.Errorf("open public dir: %w", err)
	}
	archive, err := publish.Open(j.cfg.ArchiveDir, false)
	if err != nil {
		_ = public.Close()
		return nil, fmt.Errorf("open archive dir: %w", err)
	}
	store, err := state.Open(j.cfg.StateDir)
	if err != nil {
		_ = public.Close()
		_ = archive.Close()
		return nil, err
	}
	return &run{
		cfg:     j.cfg,
		deps:    j.deps,
		public:  public,
		archive: archive,
		store:   store,
		sources: make(map[source.ID]SourceHealth, 3),
	}, nil
}

func (r *run) close() {
	_ = r.public.Close()
	_ = r.archive.Close()
	_ = r.store.Close()
}

// previous is what is currently published.
type previous struct {
	snap      rates.Snapshot
	bytes     []byte
	published bool // false: snap is the bundled baseline, nothing is live
}

func (r *run) execute(ctx context.Context, now time.Time) (Report, error) {
	for _, d := range []interface{ Sweep() error }{r.public, r.archive, r.store} {
		if err := d.Sweep(); err != nil {
			return Report{}, fmt.Errorf("sweep temporary files: %w", err)
		}
	}
	r.loadState()
	ov := r.loadOverrides()
	prev, err := r.loadPrevious()
	if err != nil {
		return Report{}, err
	}
	r.current = prev

	if _, err := r.closeDays(prev, now); err != nil {
		return Report{}, fmt.Errorf("close days: %w", err)
	}

	fetched := r.fetchAll(ctx, now)
	cfg := merge.Config{Catalog: r.deps.Catalog, Policy: r.deps.Policy}
	in, warns := merge.Screen(fetched.inputs(), prev.snap, cfg, now)
	r.recordSources(fetched, in, warns, now)
	res, mst := merge.Merge(in, prev.snap, r.st.Merge, cfg, ov, now)
	res.Warnings = append(warns, res.Warnings...)
	r.st.Merge = mst
	r.noteWarnings(res.Warnings)

	rep, latest := r.gate(res, prev, now)

	// Writes start only while the context is alive; once started they run
	// to completion so the tree is never left half-updated.
	if err := ctx.Err(); err != nil {
		return rep, fmt.Errorf("cancelled before writing: %w", err)
	}
	if err := r.writeArchive(fetched, now); err != nil {
		return rep, fmt.Errorf("write archive: %w", err)
	}
	if latest != nil {
		if _, err := r.public.Write(validate.LatestPath, latest); err != nil {
			return rep, fmt.Errorf("write latest: %w", err)
		}
		snap, err := rates.Decode(latest)
		if err != nil {
			return rep, fmt.Errorf("decode written latest: %w", err)
		}
		r.current = previous{snap: snap, bytes: latest, published: true}
	}
	if err := r.writeHistoryIndex(); err != nil {
		return rep, fmt.Errorf("write history index: %w", err)
	}
	r.st.LastRunAt = rates.NewTime(now)
	if err := r.store.Save(r.st); err != nil {
		return rep, fmt.Errorf("save state: %w", err)
	}
	rep.Health = r.health(now, rep)
	if err := r.writeHealth(rep.Health); err != nil {
		return rep, fmt.Errorf("write health: %w", err)
	}
	return rep, nil
}

// gate decides about publication. latest is the bytes to
// write, nil when latest.json must stay as it is.
func (r *run) gate(res merge.Result, prev previous, now time.Time) (rep Report, latest []byte) {
	cat, pol := r.deps.Catalog, r.deps.Policy
	aborted := func(reason string) (Report, []byte) {
		return Report{Outcome: OutcomeAborted, Reason: reason}, nil
	}
	if res.Abort != "" {
		return aborted(string(res.Abort))
	}
	cand := res.Snapshot
	if err := validate.Snapshot(cand, cat, pol); err != nil {
		r.deps.Log.Error("candidate violates the contract", slog.Any("err", err))
		return aborted(ReasonSchemaViolation)
	}
	if err := validate.Fresh(cand, time.Time{}, pol, now); err != nil {
		r.deps.Log.Error("candidate asOf out of range", slog.Any("err", err))
		return aborted(ReasonSchemaViolation)
	}
	if prev.published {
		switch {
		case cand.AsOf.Before(prev.snap.AsOf.Time):
			// Unreachable while merge clamps asOf; kept as the gate's own
			// guarantee that published data never goes back in time.
			return aborted(ReasonStaleCandidate)
		case cand.AsOf.Equal(prev.snap.AsOf.Time) &&
			cand.PrimaryCount() <= prev.snap.PrimaryCount()-pol.DegradedDrop:
			r.deps.Log.Warn("candidate degraded",
				slog.Int("primary", cand.PrimaryCount()),
				slog.Int("prevPrimary", prev.snap.PrimaryCount()))
			return aborted(ReasonDegraded)
		}
		cand.GeneratedAt = prev.snap.GeneratedAt
		if b, err := rates.Encode(cand); err == nil && bytes.Equal(b, prev.bytes) {
			return Report{Outcome: OutcomeUnchanged}, nil
		}
	}
	cand.GeneratedAt = rates.NewTime(now)
	b, err := rates.Encode(cand)
	if err == nil {
		_, err = validate.Encoded(b, cat, pol)
	}
	if err != nil {
		r.deps.Log.Error("encoded candidate rejected", slog.Any("err", err))
		return aborted(ReasonSchemaViolation)
	}
	if len(b) > pol.TargetLatestBytes {
		r.deps.Log.Warn("latest.json over the target size",
			slog.Int("bytes", len(b)), slog.Int("target", pol.TargetLatestBytes))
	}
	return Report{Outcome: OutcomePublished}, b
}

func (r *run) loadState() {
	st, err := r.store.Load()
	if err != nil {
		// The state only speeds up fetching and counts jump confirmations;
		// starting over is safe, silently losing it is not.
		r.deps.Log.Error("state unreadable, starting fresh", slog.Any("err", err))
		r.attend(Attention{Kind: "stateReset", Detail: err.Error()})
		st = state.State{SchemaVersion: 1}
	}
	if st.Sources == nil {
		st.Sources = make(map[source.ID]state.Source, 3)
	}
	r.st = st
}

func (r *run) loadOverrides() merge.Overrides {
	ov, err := state.LoadOverrides(r.cfg.OverridesFile)
	if err != nil {
		r.deps.Log.Error("overrides ignored", slog.Any("err", err))
		r.attend(Attention{Kind: "overridesInvalid", Detail: err.Error()})
		return nil
	}
	return ov
}

func (r *run) loadPrevious() (previous, error) {
	b, err := r.public.ReadFile(validate.LatestPath)
	switch {
	case err == nil:
		s, verr := validate.Encoded(b, r.deps.Catalog, r.deps.Policy)
		if verr == nil {
			return previous{snap: s, bytes: b, published: true}, nil
		}
		// Do not build on a broken file; the next publication replaces it.
		r.deps.Log.Error("published latest.json is invalid", slog.Any("err", verr))
		r.attend(Attention{Kind: "latestInvalid", Detail: verr.Error()})
	case !errors.Is(err, fs.ErrNotExist):
		return previous{}, fmt.Errorf("read latest: %w", err)
	}
	bundled, err := catalog.Bundled()
	if err != nil {
		return previous{}, err
	}
	return previous{snap: bundled}, nil
}

// fetchResult is the raw outcome of fetching the three upstreams.
type fetchResult map[source.ID]fetchOutcome

type fetchOutcome struct {
	quote *source.Quote // nil on error
	err   error
}

func (f fetchResult) inputs() merge.Inputs {
	return merge.Inputs{
		Primary:   f[source.Frankfurter].quote,
		Secondary: f[source.Fawaz].quote,
		Voter:     f[source.ERAPI].quote,
	}
}

// fetchAll queries the three upstreams in parallel under one deadline.
func (r *run) fetchAll(ctx context.Context, now time.Time) fetchResult {
	ctx, cancel := context.WithTimeout(ctx, r.cfg.Timeout)
	defer cancel()

	fetchers := map[source.ID]Fetcher{
		source.Frankfurter: r.deps.Primary,
		source.Fawaz:       r.deps.Secondary,
		source.ERAPI:       r.deps.Voter,
	}
	// Every goroutine owns one slot; results is assembled after Wait, so
	// no map is shared while fetches run.
	type slot struct {
		id  source.ID
		out fetchOutcome
	}
	slots := make([]slot, 0, len(fetchers))
	for id := range fetchers {
		slots = append(slots, slot{id: id})
	}
	var wg sync.WaitGroup
	for i := range slots {
		s := &slots[i]
		if until := r.st.Sources[s.id].CooldownUntil; until.After(now) {
			s.out.err = fmt.Errorf("cooling down until %s", formatTime(until))
			continue
		}
		f, cache := fetchers[s.id], r.cacheFor(s.id)
		wg.Go(func() {
			q, err := f.Fetch(ctx, cache)
			s.out.err = err
			if err == nil {
				s.out.quote = &q
			}
		})
	}
	wg.Wait()

	results := make(fetchResult, len(slots))
	for _, s := range slots {
		results[s.id] = s.out
	}
	return results
}

func formatTime(t rates.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}
