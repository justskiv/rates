package merge_test

import (
	"bytes"
	"flag"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/justskiv/rates/internal/fixture"
	"github.com/justskiv/rates/internal/merge"
	"github.com/justskiv/rates/internal/rates"
)

var _update = flag.Bool("update", false, "rewrite golden files in testdata/merge")

// TestGolden runs named scenarios on the recorded inputs and compares the
// snapshot bytes (want.json) and a readable report of every decision and
// warning (want.report) with testdata/merge/<case>/.
func TestGolden(t *testing.T) {
	tests := []struct {
		name  string
		setup func(e *env) merge.Inputs
	}{
		{"real-2026-11-01", func(e *env) merge.Inputs { return e.inputs() }},
		{"primary-down", func(e *env) merge.Inputs {
			in := e.inputs()
			in.Primary = nil
			return in
		}},
		{"secondary-down", func(e *env) merge.Inputs {
			in := e.inputs()
			in.Secondary = nil
			return in
		}},
		{"voter-down", func(e *env) merge.Inputs {
			in := e.inputs()
			in.Voter = nil
			return in
		}},
		{"kzt-jump-held", func(e *env) merge.Inputs {
			e.f.Rates["KZT"] *= 1.2
			e.w.Rates["KZT"] *= 1.2
			return e.inputs()
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			res := e.merge(t, tt.setup(e))
			res.Snapshot.GeneratedAt = rates.NewTime(e.now)
			gotJSON, err := rates.Encode(res.Snapshot)
			if err != nil {
				t.Fatal(err)
			}
			gotReport := report(res, e.st)

			dir := filepath.Join(fixture.TestdataDir(), "merge", tt.name)
			compareGolden(t, filepath.Join(dir, "want.json"), gotJSON)
			compareGolden(t, filepath.Join(dir, "want.report"), []byte(gotReport))
		})
	}
}

func compareGolden(t *testing.T, path string, got []byte) {
	t.Helper()
	if *_update {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run with -update to create)", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("%s differs from the golden file (run with -update after review)\ngot:\n%s",
			filepath.Base(path), got)
	}
}

func report(res merge.Result, st merge.State) string {
	var b strings.Builder
	fmt.Fprintf(&b, "abort: %s\n", cmpOr(string(res.Abort), "-"))
	fmt.Fprintf(&b, "asOf: %s\n", res.Snapshot.AsOf.Format(time.RFC3339))
	fmt.Fprintf(&b, "published: %d\n\nwarnings:\n", len(res.Snapshot.Rates))
	for _, w := range res.Warnings {
		fmt.Fprintf(&b, "  %s %s%s %s\n", w.Kind, w.Source, w.Code, w.Detail)
	}
	b.WriteString("\npending:\n")
	for _, code := range slices.Sorted(maps.Keys(st.Pending)) {
		p := st.Pending[code]
		fmt.Fprintf(&b, "  %s value=%v runs=%d\n", code, p.Value, p.Runs)
	}
	b.WriteString("\ndecisions:\n")
	for _, d := range res.Decisions {
		fmt.Fprintf(&b, "  %s\n", strings.TrimSpace(d.String()))
	}
	return b.String()
}

func cmpOr(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}
