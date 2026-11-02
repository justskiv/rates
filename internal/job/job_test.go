package job_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/justskiv/rates/internal/fixture"
	"github.com/justskiv/rates/internal/job"
	"github.com/justskiv/rates/internal/policy"
	"github.com/justskiv/rates/internal/rates"
	"github.com/justskiv/rates/internal/source"
	"github.com/justskiv/rates/internal/state"
	"github.com/justskiv/rates/internal/validate"
)

// fakeSource serves a scripted quote and emulates 304 for a matching ETag.
type fakeSource struct {
	mu    sync.Mutex
	quote source.Quote
	err   error
	calls []source.Cache
}

func (f *fakeSource) Fetch(_ context.Context, c source.Cache) (source.Quote, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, c)
	if f.err != nil {
		return source.Quote{}, f.err
	}
	q := f.quote.Clone()
	if c.ETag != "" && c.ETag == q.Meta.ETag && bytes.Equal(c.Body, q.Body) {
		q.Meta.NotModified = true
	}
	return q, nil
}

func (f *fakeSource) set(q source.Quote, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.quote, f.err = q, err
}

// lockedBuffer collects log output written from several goroutines.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) Bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return bytes.Clone(b.buf.Bytes())
}

// world drives the job through simulated days on a fake clock.
type world struct {
	t         *testing.T
	base      [3]source.Quote // recorded F, W, E of 2026-11-01
	eBody     []byte
	f, w, e   *fakeSource
	now       time.Time
	dir       string
	job       *job.Job
	logs      *lockedBuffer
	overrides string
	lastDay   int
	lastEUR   float64
}

func newWorld(t *testing.T) *world {
	t.Helper()
	f, w, e := fixture.Quotes(t)
	f.Body = fixture.Read(t, fixture.FrankfurterFile)
	w.Body = fixture.Read(t, fixture.FawazFile)
	wd := &world{
		t:     t,
		base:  [3]source.Quote{f, w, e},
		eBody: fixture.Read(t, fixture.ERAPIFile),
		f:     &fakeSource{},
		w:     &fakeSource{},
		e:     &fakeSource{},
		now:   fixture.Now,
		dir:   t.TempDir(),
	}
	wd.overrides = filepath.Join(wd.dir, "overrides.json")
	wd.logs = &lockedBuffer{}
	wd.setDay(0, 1)
	j, err := job.New(job.Config{
		PublicDir:     filepath.Join(wd.dir, "public"),
		StateDir:      filepath.Join(wd.dir, "state"),
		ArchiveDir:    filepath.Join(wd.dir, "archive"),
		OverridesFile: wd.overrides,
		Precompress:   true,
		Timeout:       5 * time.Second,
		Version:       "test",
	}, job.Deps{
		Primary:   wd.f,
		Secondary: wd.w,
		Voter:     wd.e,
		Catalog:   fixture.Catalog(t),
		Policy:    policy.Default(),
		Log:       slog.New(slog.NewJSONHandler(wd.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Now:       func() time.Time { return wd.now },
	})
	if err != nil {
		t.Fatal(err)
	}
	wd.job = j
	return wd
}

// setDay makes every source observe day k after 2026-11-01 with EUR moved
// by eur. The bodies change with the content so raw/ sees new versions.
func (wd *world) setDay(k int, eur float64) {
	wd.lastDay, wd.lastEUR = k, eur
	shift := func(q source.Quote) source.Quote {
		q = q.Clone()
		q.Date = q.Date.AddDate(0, 0, k)
		for code, d := range q.Dates {
			q.Dates[code] = d.AddDate(0, 0, k)
		}
		if v, ok := q.Rates["EUR"]; ok {
			q.Rates["EUR"] = v * eur
		}
		return q
	}
	// Leading spaces keep the bodies valid JSON yet distinct per day.
	pad := bytes.Repeat([]byte(" "), k+1)
	f := shift(wd.base[0])
	f.Body = append(pad, f.Body...)
	f.Meta.ETag = `"f-` + strconv.Itoa(k) + `"`
	w := shift(wd.base[1])
	w.Body = append(pad, w.Body...)
	e := shift(wd.base[2])
	e.Body = wd.eBody // a buggy adapter would hand it over; the job must drop it
	wd.f.set(f, nil)
	wd.w.set(w, nil)
	wd.e.set(e, nil)
}

// restore puts the sources back to the last day set with setDay.
func (wd *world) restore() {
	wd.setDay(wd.lastDay, wd.lastEUR)
}

func (wd *world) run(at time.Time) job.Report {
	wd.t.Helper()
	wd.now = at
	rep, err := wd.job.Run(context.Background())
	if err != nil {
		wd.t.Fatalf("run at %s: %v", at.Format(time.RFC3339), err)
	}
	return rep
}

func (wd *world) public(name string) string {
	return filepath.Join(wd.dir, "public", name)
}

func (wd *world) health() job.Health {
	wd.t.Helper()
	var h job.Health
	if err := json.Unmarshal(wd.read(wd.public(validate.HealthPath)), &h); err != nil {
		wd.t.Fatal(err)
	}
	return h
}

func (wd *world) read(path string) []byte {
	wd.t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		wd.t.Fatal(err)
	}
	return b
}

func (wd *world) latestStat() os.FileInfo {
	wd.t.Helper()
	fi, err := os.Stat(wd.public(validate.LatestPath))
	if err != nil {
		wd.t.Fatal(err)
	}
	return fi
}

func at(day, hour, minute int) time.Time {
	return time.Date(2026, 11, day, hour, minute, 0, 0, time.UTC)
}

func expectOutcome(t *testing.T, rep job.Report, outcome job.Outcome, reason string) {
	t.Helper()
	if rep.Outcome != outcome || rep.Reason != reason {
		t.Fatalf("outcome %s(%s), want %s(%s)", rep.Outcome, rep.Reason, outcome, reason)
	}
	if rep.Health.Outcome != outcome {
		t.Fatalf("health outcome %s, want %s", rep.Health.Outcome, outcome)
	}
}

// TestTenDays runs the job through ten simulated days: first publication,
// unchanged runs, day closing, conditional fetches, every kind of abort,
// and checks the published tree after each step.
func TestTenDays(t *testing.T) {
	wd := newWorld(t)

	// Day 1 (2026-11-01): first publication.
	rep := wd.run(at(1, 19, 7))
	expectOutcome(t, rep, job.OutcomePublished, "")
	if _, err := os.Stat(wd.public(validate.HistoryIndexPath)); !errors.Is(err, fs.ErrNotExist) {
		t.Error("history written before any day closed")
	}
	first := wd.latestStat()
	h := wd.health()
	if h.Counts.Published != 153 || h.AsOf.Format(time.DateOnly) != "2026-11-01" ||
		!h.Sources[source.Frankfurter].OK || h.Sources[source.ERAPI].EOL == nil {
		t.Fatalf("health after first run: %+v", h)
	}

	// Same data an hour later: unchanged, latest.json untouched, the
	// primary is fetched conditionally with the archived body.
	time.Sleep(5 * time.Millisecond)
	rep = wd.run(at(1, 20, 7))
	expectOutcome(t, rep, job.OutcomeUnchanged, "")
	if !wd.latestStat().ModTime().Equal(first.ModTime()) {
		t.Error("unchanged run touched latest.json")
	}
	if c := wd.f.calls[len(wd.f.calls)-1]; c.ETag == "" || len(c.Body) == 0 {
		t.Error("second run did not send a conditional request")
	}
	if !wd.health().Sources[source.Frankfurter].NotModified {
		t.Error("304 not reported in health.json")
	}

	// Days 2-10: one new observation per day around noon, a check run
	// after midnight, and an incident on some days.
	for day := 2; day <= 10; day++ {
		rep = wd.run(at(day, 0, 17))
		expectOutcome(t, rep, job.OutcomeUnchanged, "")
		closed := validate.HistoryPath(at(day-1, 0, 0))
		if _, err := os.Stat(wd.public(closed)); err != nil {
			t.Fatalf("day %d: %s not closed: %v", day, closed, err)
		}

		switch day {
		case 3: // both publishable sources down
			wd.f.set(source.Quote{}, &source.StatusError{Code: 503})
			wd.w.set(source.Quote{}, errors.New("connection reset"))
			before := wd.read(wd.public(validate.LatestPath))
			rep = wd.run(at(day, 12, 17))
			expectOutcome(t, rep, job.OutcomeAborted, "all_sources_down")
			if !bytes.Equal(before, wd.read(wd.public(validate.LatestPath))) {
				t.Fatal("aborted run changed latest.json")
			}
			if wd.health().Sources[source.Frankfurter].Error != "unexpected status 503" {
				t.Errorf("source error not reported: %+v", wd.health().Sources)
			}
			wd.restore()
		case 5: // mass jump: eleven currencies move 30% in both sources
			f, w := wd.base[0].Clone(), wd.base[1].Clone()
			for _, c := range []rates.Code{"AUD", "BRL", "CAD", "CHF", "CNY", "CZK", "DKK", "GBP", "HKD", "HUF", "ILS"} {
				f.Rates[c] *= 1.3
				w.Rates[c] *= 1.3
			}
			f.Date, w.Date = f.Date.AddDate(0, 0, 4), w.Date.AddDate(0, 0, 4)
			for c, d := range f.Dates {
				f.Dates[c] = d.AddDate(0, 0, 4)
			}
			wd.f.set(f, nil)
			wd.w.set(w, nil)
			rep = wd.run(at(day, 12, 17))
			expectOutcome(t, rep, job.OutcomeAborted, "mass_jump")
			if got := wd.health().Attention; len(got) != 11 || got[0].Kind != "held" {
				t.Errorf("attention %+v, want 11 held", got)
			}
			wd.restore()
		case 7: // the primary loses five currencies on the same day: degraded
			wd.setDay(day-1, 1+float64(day)/1000)
			rep = wd.run(at(day, 12, 17))
			expectOutcome(t, rep, job.OutcomePublished, "")
			f := wd.f.quote.Clone()
			for _, c := range []rates.Code{"AUD", "BRL", "CAD", "CHF", "CNY"} {
				delete(f.Rates, c)
				delete(f.Dates, c)
			}
			f.Body = append([]byte("  "), f.Body...)
			f.Meta.ETag = `"degraded"`
			wd.f.set(f, nil)
			rep = wd.run(at(day, 13, 17))
			expectOutcome(t, rep, job.OutcomeAborted, "degraded")
			wd.restore()
		default:
			wd.setDay(day-1, 1+float64(day)/1000)
			rep = wd.run(at(day, 12, 17))
			expectOutcome(t, rep, job.OutcomePublished, "")
			if got, want := rep.Health.AsOf.Format(time.DateOnly), at(day, 0, 0).Format(time.DateOnly); got != want {
				t.Errorf("day %d: asOf %s, want %s", day, got, want)
			}
		}
		assertTree(t, wd)
	}

	// History: one immutable file per closed day, an index without gaps.
	var idx struct {
		First, Last string
		Missing     []string
	}
	if err := json.Unmarshal(wd.read(wd.public(validate.HistoryIndexPath)), &idx); err != nil {
		t.Fatal(err)
	}
	if idx.First != "2026-11-01" || idx.Last != "2026-11-09" || len(idx.Missing) != 0 {
		t.Errorf("history index %+v", idx)
	}
	// The day of the double outage closes with the previous day's data.
	d2 := wd.read(wd.public(validate.HistoryPath(at(2, 0, 0))))
	d3 := wd.read(wd.public(validate.HistoryPath(at(3, 0, 0))))
	if !bytes.Equal(d2, d3) {
		t.Error("history of the outage day differs from the day before")
	}

	assertSchema(t, wd)
	assertNoVoterData(t, wd)
	assertArchive(t, wd)
}

func assertTree(t *testing.T, wd *world) {
	t.Helper()
	root, err := os.OpenRoot(wd.public(""))
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := validate.Tree(root.FS(), fixture.Catalog(t), policy.Default(), wd.now); err != nil {
		t.Fatalf("published tree invalid at %s: %v", wd.now.Format(time.RFC3339), err)
	}
	pol := policy.Default()
	if n := len(wd.read(wd.public(validate.LatestPath))); n > pol.TargetLatestBytes {
		t.Errorf("latest.json is %d bytes, target %d", n, pol.TargetLatestBytes)
	}
	if n := len(wd.read(wd.public(validate.HealthPath))); n > pol.MaxHealthBytes {
		t.Errorf("health.json is %d bytes, budget %d", n, pol.MaxHealthBytes)
	}
}

// T24 end to end: no value of the voter reaches any file the job writes,
// including the public archive and the state, nor the log: Actions logs of
// a public repository are public.
func assertNoVoterData(t *testing.T, wd *world) {
	t.Helper()
	var doc struct {
		Rates map[string]json.Number `json:"rates"`
	}
	dec := json.NewDecoder(bytes.NewReader(wd.eBody))
	dec.UseNumber()
	if err := dec.Decode(&doc); err != nil {
		t.Fatal(err)
	}
	var canaries []string
	for _, v := range doc.Rates {
		if s := v.String(); len(strings.Trim(strings.ReplaceAll(s, ".", ""), "0")) >= 7 {
			canaries = append(canaries, s)
		}
	}
	if len(canaries) < 100 {
		t.Fatalf("only %d canaries", len(canaries))
	}
	logs := wd.logs.Bytes()
	if len(logs) == 0 {
		t.Fatal("no log output captured")
	}
	for _, c := range canaries {
		if bytes.Contains(logs, []byte(c)) {
			t.Errorf("the log contains the voter value %s", c)
		}
	}
	err := filepath.WalkDir(wd.dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.HasSuffix(path, ".gz") {
			return nil // archive bodies are checked below via their sources
		}
		for _, c := range canaries {
			if bytes.Contains(b, []byte(c)) {
				t.Errorf("%s contains the voter value %s", path, c)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func assertArchive(t *testing.T, wd *world) {
	t.Helper()
	var names []string
	err := filepath.WalkDir(filepath.Join(wd.dir, "archive"), func(path string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			names = append(names, filepath.Base(path))
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	var f, w int
	for _, n := range names {
		switch {
		case strings.HasPrefix(n, string(source.Frankfurter)):
			f++
		case strings.HasPrefix(n, string(source.Fawaz)):
			w++
		default:
			t.Errorf("unexpected archive file %s", n)
		}
	}
	if f < 5 || w < 5 {
		t.Errorf("archive has %d frankfurter and %d fawazahmed0 bodies", f, w)
	}
}

func TestSkippedWhenLocked(t *testing.T) {
	wd := newWorld(t)
	st, err := state.Open(filepath.Join(wd.dir, "state"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	unlock, err := st.Lock()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = unlock() }()
	rep := wd.run(at(1, 19, 7))
	if rep.Outcome != job.OutcomeSkipped {
		t.Fatalf("outcome %s, want skipped", rep.Outcome)
	}
	if _, err := os.Stat(wd.public(validate.HealthPath)); !errors.Is(err, fs.ErrNotExist) {
		t.Error("a skipped run wrote health.json")
	}
}

func TestFailedRunStillReportsHealth(t *testing.T) {
	wd := newWorld(t)
	wd.run(at(1, 19, 7))
	stateDir := filepath.Join(wd.dir, "state")
	if err := os.Chmod(stateDir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(stateDir, 0o755) })

	wd.now = at(1, 20, 7)
	rep, err := wd.job.Run(context.Background())
	if err == nil || rep.Outcome != job.OutcomeFailed {
		t.Fatalf("outcome %s, err %v; want failed", rep.Outcome, err)
	}
	if h := wd.health(); h.Outcome != job.OutcomeFailed || h.AsOf.IsZero() {
		t.Errorf("health %+v, want failed with the published asOf", h)
	}
}

func TestOverridesInvalidRaisesAttention(t *testing.T) {
	wd := newWorld(t)
	if err := os.WriteFile(wd.overrides, []byte(`{"SYP":{"rate":1}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	rep := wd.run(at(1, 19, 7))
	expectOutcome(t, rep, job.OutcomePublished, "")
	if a := wd.health().Attention; len(a) != 1 || a[0].Kind != "overridesInvalid" {
		t.Errorf("attention %+v", a)
	}

	if err := os.WriteFile(wd.overrides, []byte(`{"SYP":{"rate":121.8,"until":"2026-11-20"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	rep = wd.run(at(1, 20, 7))
	expectOutcome(t, rep, job.OutcomePublished, "")
	h := wd.health()
	if h.Counts.Override != 1 || len(h.Attention) != 1 || h.Attention[0].Kind != "overrideActive" {
		t.Errorf("health %+v", h)
	}
}

func TestPrimaryDownAttention(t *testing.T) {
	wd := newWorld(t)
	wd.run(at(1, 10, 7))
	wd.f.set(source.Quote{}, &source.StatusError{Code: 502})
	for hour := 11; hour <= 18; hour++ {
		rep := wd.run(at(1, hour, 7))
		expectOutcome(t, rep, job.OutcomeAborted, "degraded")
	}
	var found bool
	for _, a := range wd.health().Attention {
		found = found || a.Kind == "primaryDown"
	}
	if !found {
		t.Errorf("no primaryDown after 6 hours: %+v", wd.health().Attention)
	}
}

func TestRateLimitedSourceCoolsDown(t *testing.T) {
	wd := newWorld(t)
	wd.e.set(source.Quote{}, &source.StatusError{Code: 429, RetryAfter: 20 * time.Minute})
	wd.run(at(1, 19, 7))
	calls := len(wd.e.calls)
	wd.run(at(1, 19, 17))
	if len(wd.e.calls) != calls {
		t.Error("source fetched during its cooldown")
	}
	wd.run(at(1, 19, 37))
	if len(wd.e.calls) != calls+1 {
		t.Error("source not fetched after the cooldown")
	}
}
