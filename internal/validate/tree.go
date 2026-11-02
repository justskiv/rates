package validate

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/justskiv/rates/internal/catalog"
	"github.com/justskiv/rates/internal/policy"
	"github.com/justskiv/rates/internal/rates"
)

// Paths of the published tree.
const (
	LatestPath       = "v1/latest.json"
	HealthPath       = "v1/health.json"
	HistoryDir       = "v1/history"
	HistoryIndexPath = "v1/history/index.json"
)

// _minWireRates is minProperties of rates in rates.schema.json.
const _minWireRates = 100

var _historyName = regexp.MustCompile(`^v1/history/(\d{4})/(\d{4})-\d{2}-\d{2}\.json$`)

// HistoryPath returns the path of the history file for day (UTC).
func HistoryPath(day time.Time) string {
	d := day.UTC()
	return path.Join(HistoryDir, d.Format("2006"), d.Format(time.DateOnly)+".json")
}

// HistoryDay parses a history file path; ok is false for other paths.
func HistoryDay(name string) (time.Time, bool) {
	m := _historyName.FindStringSubmatch(name)
	if m == nil || m[1] != m[2] {
		return time.Time{}, false
	}
	d, err := time.Parse(time.DateOnly, strings.TrimSuffix(path.Base(name), ".json"))
	return d, err == nil
}

// Health is the subset of health.json that verification inspects.
type Health struct {
	SchemaVersion int        `json:"schemaVersion"`
	CheckedAt     rates.Time `json:"checkedAt"`
	Outcome       string     `json:"outcome"`
}

// Tree checks a published tree rooted at fsys (the directory that holds
// v1/): latest.json, health.json, every history file, gzip sidecars, size
// budgets and leftovers of interrupted writes.
func Tree(fsys fs.FS, cat *catalog.Catalog, p policy.Policy, now time.Time) error {
	var errs []error
	fail := func(name string, err error) {
		errs = append(errs, fmt.Errorf("%s: %w", name, err))
	}

	if b, err := fs.ReadFile(fsys, LatestPath); err != nil {
		fail(LatestPath, err)
	} else if s, err := Encoded(b, cat, p); err != nil {
		fail(LatestPath, err)
	} else if err := Fresh(s, time.Time{}, p, now); err != nil {
		fail(LatestPath, err)
	}

	if b, err := fs.ReadFile(fsys, HealthPath); err != nil {
		fail(HealthPath, err)
	} else if err := checkHealth(b, p); err != nil {
		fail(HealthPath, err)
	}

	err := fs.WalkDir(fsys, ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && d.Name() == ".git" {
			return fs.SkipDir
		}
		if strings.HasPrefix(d.Name(), ".tmp-") {
			fail(name, errors.New("leftover temporary file"))
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if err := checkFile(fsys, name, p); err != nil {
			fail(name, err)
		}
		return nil
	})
	if err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

func checkFile(fsys fs.FS, name string, p policy.Policy) error {
	if !strings.HasPrefix(name, "v1/") {
		return nil // .nojekyll, CNAME, README.md of the data branch
	}
	b, err := fs.ReadFile(fsys, name)
	if err != nil {
		return err
	}
	if len(b) > MaxWireBytes {
		return fmt.Errorf("%d bytes over the %d byte wire budget", len(b), MaxWireBytes)
	}
	if plain, ok := strings.CutSuffix(name, ".gz"); ok {
		if err := checkSidecar(fsys, plain, b); err != nil {
			return fmt.Errorf("gzip sidecar: %w", err)
		}
		return nil
	}
	if day, ok := HistoryDay(name); ok {
		return checkHistory(b, day, p)
	}
	if name == HistoryIndexPath {
		var idx struct {
			First   string   `json:"first"`
			Last    string   `json:"last"`
			Missing []string `json:"missing"`
		}
		return json.Unmarshal(b, &idx)
	}
	if name == LatestPath || name == HealthPath {
		return nil // checked above
	}
	return errors.New("unexpected file in the published tree")
}

// checkHistory validates an immutable history file against the wire
// format only: the catalog of the day it was written may differ from
// today's, and an old file must never block a run.
func checkHistory(b []byte, day time.Time, p policy.Policy) error {
	if len(b) > p.MaxLatestBytes {
		return fmt.Errorf("%d bytes, budget %d", len(b), p.MaxLatestBytes)
	}
	s, err := rates.Decode(b)
	if err != nil {
		return err
	}
	switch {
	case s.SchemaVersion != rates.SchemaVersion || s.Base != rates.Base || s.AsOf.IsZero():
		return errors.New("schemaVersion, base or asOf invalid")
	case len(s.Rates) < _minWireRates:
		return fmt.Errorf("%d rates, the schema wants at least %d", len(s.Rates), _minWireRates)
	}
	for code, v := range s.Rates {
		if !rates.ValidCode(string(code)) || !rates.ValidRate(v) {
			return fmt.Errorf("rates[%s] = %v invalid", code, v)
		}
	}
	if s.AsOf.After(day.Add(24 * time.Hour)) {
		return fmt.Errorf("asOf %s is after the day it closes", s.AsOf.Format(time.RFC3339))
	}
	return nil
}

func checkSidecar(fsys fs.FS, plain string, gz []byte) error {
	want, err := fs.ReadFile(fsys, plain)
	if err != nil {
		return fmt.Errorf("sidecar without %s: %w", plain, err)
	}
	zr, err := gzip.NewReader(bytes.NewReader(gz))
	if err != nil {
		return err
	}
	got, err := io.ReadAll(io.LimitReader(zr, MaxWireBytes+1))
	if err != nil {
		return err
	}
	if !bytes.Equal(got, want) {
		return errors.New("sidecar does not match the plain file")
	}
	return nil
}

func checkHealth(b []byte, p policy.Policy) error {
	if len(b) > p.MaxHealthBytes {
		return fmt.Errorf("%d bytes, budget %d", len(b), p.MaxHealthBytes)
	}
	var h Health
	if err := json.Unmarshal(b, &h); err != nil {
		return err
	}
	if h.SchemaVersion != 1 || h.CheckedAt.IsZero() {
		return errors.New("schemaVersion or checkedAt missing")
	}
	switch h.Outcome {
	case "published", "unchanged", "aborted", "failed":
		return nil
	default:
		return fmt.Errorf("unknown outcome %q", h.Outcome)
	}
}
