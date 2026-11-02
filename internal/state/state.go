// Package state persists what one run hands to the next (state.json),
// reads manual overrides and serializes runs with a lock.
package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"

	"github.com/justskiv/rates/internal/merge"
	"github.com/justskiv/rates/internal/publish"
	"github.com/justskiv/rates/internal/rates"
	"github.com/justskiv/rates/internal/source"
)

// FileName is the state file inside the state directory.
const FileName = "state.json"

// ErrLocked is returned by Lock when another run holds the lock.
var ErrLocked = errors.New("state is locked by another run")

// State is everything a run remembers between runs. In option B it lives
// in a public repository: it must never hold values of the voter.
type State struct {
	SchemaVersion int                  `json:"schemaVersion"`
	LastRunAt     rates.Time           `json:"lastRunAt,omitzero"`
	Sources       map[source.ID]Source `json:"sources,omitempty"`
	Merge         merge.State          `json:"merge"`
}

// Source is the per-upstream memory.
type Source struct {
	// ETag and BodySHA256 describe the last body kept in the archive; the
	// validator is sent only while that body is available.
	ETag       string     `json:"etag,omitempty"`
	BodySHA256 string     `json:"bodySha256,omitempty"`
	LastOkAt   rates.Time `json:"lastOkAt,omitzero"`
	// DownSince is the first failure of the current outage.
	DownSince rates.Time `json:"downSince,omitzero"`
	// CooldownUntil honours Retry-After of a 429.
	CooldownUntil rates.Time `json:"cooldownUntil,omitzero"`
	LastError     string     `json:"lastError,omitempty"`
}

// Store reads and writes state.json atomically in one directory.
type Store struct {
	path string
	dir  *publish.Dir
}

// Open opens (and creates) the state directory.
func Open(path string) (*Store, error) {
	dir, err := publish.Open(path, false)
	if err != nil {
		return nil, fmt.Errorf("open state dir: %w", err)
	}
	return &Store{path: path, dir: dir}, nil
}

// Close releases the directory.
func (s *Store) Close() error {
	return s.dir.Close()
}

// Sweep removes temporary files of an interrupted run.
func (s *Store) Sweep() error {
	return s.dir.Sweep()
}

// Load returns the saved state; a missing file yields the zero state.
func (s *Store) Load() (State, error) {
	b, err := s.dir.ReadFile(FileName)
	if errors.Is(err, fs.ErrNotExist) {
		return State{SchemaVersion: 1}, nil
	}
	if err != nil {
		return State{}, err
	}
	var st State
	if err := json.Unmarshal(b, &st); err != nil {
		return State{}, fmt.Errorf("decode %s: %w", FileName, err)
	}
	if st.SchemaVersion != 1 {
		return State{}, fmt.Errorf("%s: unsupported schemaVersion %d", FileName, st.SchemaVersion)
	}
	return st, nil
}

// Save writes st atomically.
func (s *Store) Save(st State) error {
	st.SchemaVersion = 1
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return fmt.Errorf("encode state: %w", err)
	}
	_, err = s.dir.Write(FileName, append(b, '\n'))
	return err
}
