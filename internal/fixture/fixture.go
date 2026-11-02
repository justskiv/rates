// Package fixture loads the recorded upstream responses of 2026-11-01 for
// tests. It is imported by tests only.
//
// testdata/sources/2026-11-01 holds:
//   - frankfurter.json: real Frankfurter v2 response;
//   - fawazahmed0.json: real fawazahmed0 response for @2026-11-01 (CC0);
//   - exchangerate-api.synthetic.json: SYNTHETIC. open.er-api forbids
//     redistribution, so its data is never stored, not even in tests. The
//     file has the open.er-api shape; values are Frankfurter's times
//     1.00317 (canaries: no published value may ever equal one of them)
//     with the known 2026-11-01 disagreements staged on purpose: SDG
//     and IRR off, BWP 14.19, KPW missing, plus non-catalog codes.
package fixture

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/justskiv/rates/internal/catalog"
	"github.com/justskiv/rates/internal/source"
)

// Now is the moment the fixtures were recorded.
var Now = time.Date(2026, 11, 1, 19, 7, 0, 0, time.UTC)

// File names inside SourcesDir.
const (
	FrankfurterFile = "frankfurter.json"
	FawazFile       = "fawazahmed0.json"
	ERAPIFile       = "exchangerate-api.synthetic.json"
)

// RootDir returns the absolute path of the repository root.
func RootDir() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..")
}

// TestdataDir returns the absolute path of testdata/.
func TestdataDir() string {
	return filepath.Join(RootDir(), "testdata")
}

// ContractsDir returns the absolute path of the embedded contract export.
func ContractsDir() string {
	return filepath.Join(RootDir(), "contracts")
}

// SourcesDir returns the directory with the recorded responses.
func SourcesDir() string {
	return filepath.Join(TestdataDir(), "sources", "2026-11-01")
}

// Read returns a file from SourcesDir.
func Read(t testing.TB, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(SourcesDir(), name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// Catalog returns the embedded catalog.
func Catalog(t testing.TB) *catalog.Catalog {
	t.Helper()
	cat, err := catalog.Default()
	if err != nil {
		t.Fatal(err)
	}
	return cat
}

// Quotes parses the three recorded responses with the catalog filter.
func Quotes(t testing.TB) (f, w, e source.Quote) {
	t.Helper()
	keep := source.Filter(Catalog(t).Contains)
	var err error
	if f, err = source.ParseFrankfurter(Read(t, FrankfurterFile), keep); err != nil {
		t.Fatal(err)
	}
	f.Source = source.Frankfurter
	if w, err = source.ParseFawaz(Read(t, FawazFile), keep); err != nil {
		t.Fatal(err)
	}
	w.Source = source.Fawaz
	if e, err = source.ParseERAPI(Read(t, ERAPIFile), keep); err != nil {
		t.Fatal(err)
	}
	e.Source = source.ERAPI
	return f, w, e
}
