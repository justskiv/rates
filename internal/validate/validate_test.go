package validate_test

import (
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/justskiv/rates/internal/catalog"
	"github.com/justskiv/rates/internal/fixture"
	"github.com/justskiv/rates/internal/policy"
	"github.com/justskiv/rates/internal/publish"
	"github.com/justskiv/rates/internal/rates"
	"github.com/justskiv/rates/internal/validate"
)

func bundled(t *testing.T) rates.Snapshot {
	t.Helper()
	s, err := catalog.Bundled()
	if err != nil {
		t.Fatal(err)
	}
	// The bundled snapshot carries 9-digit cross pegs and "2.0"; round
	// them so the canonical-form check applies to our own output style.
	for code, v := range s.Rates {
		s.Rates[code] = rates.Round(v, 7)
	}
	return s
}

func TestSnapshot(t *testing.T) {
	cat, pol := fixture.Catalog(t), policy.Default()
	if err := validate.Snapshot(bundled(t), cat, pol); err != nil {
		t.Fatalf("bundled snapshot rejected: %v", err)
	}
	tests := []struct {
		name   string
		mutate func(s *rates.Snapshot)
	}{
		{"schema version", func(s *rates.Snapshot) { s.SchemaVersion = 2 }},
		{"base", func(s *rates.Snapshot) { s.Base = "EUR" }},
		{"usd not 1", func(s *rates.Snapshot) { s.Rates["USD"] = 1.01 }},
		{"too few rates", func(s *rates.Snapshot) {
			for _, code := range cat.Codes()[:20] {
				delete(s.Rates, code)
			}
		}},
		{"unknown code", func(s *rates.Snapshot) { s.Rates["BTC"] = 0.00001; delete(s.Rates, "EUR") }},
		{"zero", func(s *rates.Snapshot) { s.Rates["EUR"] = 0 }},
		{"huge", func(s *rates.Snapshot) { s.Rates["IRR"] = 2e8 }},
		{"unknown flag", func(s *rates.Snapshot) { s.Flags = map[rates.Code][]rates.Flag{"EUR": {"shiny"}} }},
		{"unsorted flags", func(s *rates.Snapshot) {
			s.Flags = map[rates.Code][]rates.Flag{"CUP": {rates.FlagPegged, rates.FlagMultiRate}}
		}},
		{"flag without rate", func(s *rates.Snapshot) {
			delete(s.Rates, "CUP")
			s.Flags = map[rates.Code][]rates.Flag{"CUP": {rates.FlagPegged}}
		}},
		{"http endpoint", func(s *rates.Snapshot) { s.Endpoints = []string{"http://rates.example/v1/latest.json"} }},
		{"no asOf", func(s *rates.Snapshot) { s.AsOf = rates.Time{} }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := bundled(t)
			tt.mutate(&s)
			if err := validate.Snapshot(s, cat, pol); err == nil {
				t.Error("accepted")
			}
		})
	}
}

func TestFresh(t *testing.T) {
	pol := policy.Default()
	now := fixture.Now
	day := func(d int) rates.Snapshot {
		return rates.Snapshot{AsOf: rates.NewTime(time.Date(2026, 11, d, 0, 0, 0, 0, time.UTC))}
	}
	if err := validate.Fresh(day(1), time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC), pol, now); err != nil {
		t.Errorf("valid asOf rejected: %v", err)
	}
	if err := validate.Fresh(day(1), time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC), pol, now); err == nil {
		t.Error("asOf behind the previous accepted")
	}
	if err := validate.Fresh(day(3), time.Time{}, pol, now); err == nil {
		t.Error("future asOf accepted")
	}
	old := rates.Snapshot{AsOf: rates.NewTime(now.AddDate(0, -2, 0))}
	if err := validate.Fresh(old, time.Time{}, pol, now); err == nil {
		t.Error("two-month-old asOf accepted")
	}
}

func TestEncodedRequiresCanonicalBytes(t *testing.T) {
	cat, pol := fixture.Catalog(t), policy.Default()
	b, err := rates.Encode(bundled(t))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := validate.Encoded(b, cat, pol); err != nil {
		t.Fatalf("canonical bytes rejected: %v", err)
	}
	pretty := strings.Replace(string(b), `"base":"USD"`, `"base": "USD"`, 1)
	if _, err := validate.Encoded([]byte(pretty), cat, pol); err == nil {
		t.Error("non-canonical bytes accepted")
	}
}

func TestTree(t *testing.T) {
	cat, pol := fixture.Catalog(t), policy.Default()
	root := t.TempDir()
	d, err := publish.Open(root, true)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	s := bundled(t)
	s.AsOf = rates.NewTime(fixture.Now.Truncate(24 * time.Hour))
	latest, err := rates.Encode(s)
	if err != nil {
		t.Fatal(err)
	}
	health := `{"schemaVersion":1,"checkedAt":"2026-11-01T19:07:00Z","outcome":"published"}`
	for name, data := range map[string][]byte{
		validate.LatestPath: latest,
		validate.HealthPath: []byte(health),
		validate.HistoryPath(fixture.Now.AddDate(0, 0, -1)): latest,
		validate.HistoryIndexPath:                           []byte(`{"first":"2026-10-31","last":"2026-10-31","missing":[]}`),
	} {
		if _, err := d.Write(name, data); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "CNAME"), []byte("rates.tuzov.dev\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := validate.Tree(d.FS(), cat, pol, fixture.Now); err != nil {
		t.Fatalf("valid tree rejected: %v", err)
	}

	stray := filepath.Join(root, "v1", publish.TempPrefix+"x")
	if err := os.WriteFile(stray, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, validate.LatestPath+".gz"), []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}
	err = validate.Tree(d.FS(), cat, pol, fixture.Now)
	if err == nil || !strings.Contains(err.Error(), "temporary") || !strings.Contains(err.Error(), "gzip") {
		t.Errorf("broken tree: err = %v", err)
	}
}

func TestHistoryPath(t *testing.T) {
	day := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	p := validate.HistoryPath(day)
	if p != "v1/history/2026/2026-11-01.json" {
		t.Errorf("path %s", p)
	}
	if got, ok := validate.HistoryDay(p); !ok || !got.Equal(day) {
		t.Errorf("HistoryDay(%s) = %v %v", p, got, ok)
	}
	if _, ok := validate.HistoryDay("v1/history/2025/2026-11-01.json"); ok {
		t.Error("year mismatch accepted")
	}
}

// A history file written under an older catalog must not block a run.
func TestTreeHistoryIgnoresCatalogChanges(t *testing.T) {
	root := t.TempDir()
	d, err := publish.Open(root, false)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	s := bundled(t)
	s.AsOf = rates.NewTime(fixture.Now.Truncate(24 * time.Hour))
	latest, err := rates.Encode(s)
	if err != nil {
		t.Fatal(err)
	}
	old := s
	old.Rates = maps.Clone(s.Rates)
	old.Rates["HRK"] = 6.74 // withdrawn since
	old.AsOf = rates.NewTime(time.Date(2022, 12, 30, 0, 0, 0, 0, time.UTC))
	oldBytes, err := rates.Encode(old)
	if err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string][]byte{
		validate.LatestPath: latest,
		validate.HealthPath: []byte(`{"schemaVersion":1,"checkedAt":"2026-11-01T19:07:00Z","outcome":"unchanged"}`),
		validate.HistoryPath(time.Date(2022, 12, 31, 0, 0, 0, 0, time.UTC)): oldBytes,
	} {
		if _, err := d.Write(name, data); err != nil {
			t.Fatal(err)
		}
	}
	if err := validate.Tree(d.FS(), fixture.Catalog(t), policy.Default(), fixture.Now); err != nil {
		t.Errorf("old history file rejected: %v", err)
	}
}
