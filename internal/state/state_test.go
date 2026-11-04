package state_test

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/justskiv/rates/internal/merge"
	"github.com/justskiv/rates/internal/rates"
	"github.com/justskiv/rates/internal/source"
	"github.com/justskiv/rates/internal/state"
)

func open(t *testing.T, dir string) *state.Store {
	t.Helper()
	s, err := state.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestLoadSave(t *testing.T) {
	s := open(t, t.TempDir())
	st, err := s.Load()
	if err != nil {
		t.Fatal(err)
	}
	if st.SchemaVersion != 1 || len(st.Sources) != 0 {
		t.Fatalf("fresh state %+v", st)
	}
	now := rates.NewTime(time.Date(2026, 11, 1, 19, 7, 0, 0, time.UTC))
	st.LastRunAt = now
	st.Sources = map[source.ID]state.Source{
		source.Frankfurter: {BodySHA256: "00ff", LastOkAt: now},
	}
	st.Merge.Pending = map[rates.Code]merge.Pending{"KZT": {Value: 544.3, Runs: 2, Since: now}}
	if err := s.Save(st); err != nil {
		t.Fatal(err)
	}
	got, err := s.Load()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, st) {
		t.Errorf("round trip:\n%+v\n%+v", got, st)
	}
}

func TestLoadRejectsCorruptState(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, state.FileName), []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := open(t, dir).Load(); err == nil {
		t.Error("corrupt state accepted")
	}
}

func TestLock(t *testing.T) {
	dir := t.TempDir()
	first := open(t, dir)
	unlock, err := first.Lock()
	if err != nil {
		t.Fatal(err)
	}
	second := open(t, dir)
	if _, err := second.Lock(); !errors.Is(err, state.ErrLocked) {
		t.Errorf("second lock: %v, want ErrLocked", err)
	}
	if err := unlock(); err != nil {
		t.Fatal(err)
	}
	unlock2, err := second.Lock()
	if err != nil {
		t.Fatalf("lock after release: %v", err)
	}
	_ = unlock2()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("lock left files in the state dir: %v", entries)
	}
}

func TestLoadOverrides(t *testing.T) {
	tests := []struct {
		name    string
		give    string
		want    merge.Overrides
		wantErr bool
	}{
		{
			name: "valid with comment",
			give: `{"_readme":"manual values","SYP":{"rate":121.8,"until":"2026-12-31","note":"W bug"}}`,
			want: merge.Overrides{"SYP": {
				Rate: 121.8, Until: time.Date(2026, 12, 31, 0, 0, 0, 0, time.UTC), Note: "W bug",
			}},
		},
		{name: "empty object", give: `{}`, want: merge.Overrides{}},
		{name: "typo in field", give: `{"SYP":{"rate":121.8,"unitl":"2026-12-31"}}`, wantErr: true},
		{name: "until missing", give: `{"SYP":{"rate":121.8}}`, wantErr: true},
		{name: "bad date", give: `{"SYP":{"rate":121.8,"until":"31.12.2026"}}`, wantErr: true},
		{name: "lower-case code", give: `{"syp":{"rate":121.8,"until":"2026-12-31"}}`, wantErr: true},
		{name: "not json", give: `SYP=121.8`, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "overrides.json")
			if err := os.WriteFile(path, []byte(tt.give), 0o644); err != nil {
				t.Fatal(err)
			}
			got, err := state.LoadOverrides(path)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr && !reflect.DeepEqual(got, tt.want) {
				t.Errorf("got %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestLoadOverridesMissingFile(t *testing.T) {
	ov, err := state.LoadOverrides(filepath.Join(t.TempDir(), "absent.json"))
	if err != nil || ov != nil {
		t.Errorf("got %v, %v; want no overrides", ov, err)
	}
}
