package job_test

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/justskiv/rates/internal/fixture"
	"github.com/justskiv/rates/internal/validate"
)

// compileSchema loads contracts/rates.schema.json, the wire format shared
// with the iOS app.
func compileSchema(t *testing.T) *jsonschema.Schema {
	t.Helper()
	c := jsonschema.NewCompiler()
	c.AssertFormat()
	sch, err := c.Compile(filepath.Join(fixture.ContractsDir(), "rates.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	return sch
}

func validateFile(t *testing.T, sch *jsonschema.Schema, path string) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	if err := sch.Validate(doc); err != nil {
		t.Errorf("%s violates rates.schema.json: %v", path, err)
	}
}

// assertSchema checks latest.json and every history file of the world.
func assertSchema(t *testing.T, wd *world) {
	t.Helper()
	sch := compileSchema(t)
	n := 0
	err := filepath.WalkDir(wd.public("v1"), func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".json") {
			return err
		}
		rel, _ := filepath.Rel(wd.public(""), path)
		if _, ok := validate.HistoryDay(filepath.ToSlash(rel)); ok || rel == filepath.FromSlash(validate.LatestPath) {
			validateFile(t, sch, path)
			n++
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if n < 10 {
		t.Errorf("only %d documents validated", n)
	}
}

// The bundled snapshot and every golden merge output satisfy the schema.
func TestFixturesMatchSchema(t *testing.T) {
	sch := compileSchema(t)
	validateFile(t, sch, filepath.Join(fixture.ContractsDir(), "rates-snapshot.json"))
	golden, err := filepath.Glob(filepath.Join(fixture.TestdataDir(), "merge", "*", "want.json"))
	if err != nil || len(golden) == 0 {
		t.Fatalf("no golden files: %v", err)
	}
	for _, path := range golden {
		validateFile(t, sch, path)
	}
}

// VerifyDir validates a published tree given in RATES_VERIFY_DIR against
// the schema; task verify:schema uses it on the dry-run output.
func TestVerifyDirAgainstSchema(t *testing.T) {
	dir := os.Getenv("RATES_VERIFY_DIR")
	if dir == "" {
		t.Skip("RATES_VERIFY_DIR not set")
	}
	sch := compileSchema(t)
	err := filepath.WalkDir(filepath.Join(dir, "v1"), func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".json") ||
			strings.HasSuffix(path, "health.json") || strings.HasSuffix(path, "index.json") {
			return err
		}
		validateFile(t, sch, path)
		t.Logf("%s: valid", path)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
