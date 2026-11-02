package state

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"time"

	"github.com/justskiv/rates/internal/merge"
	"github.com/justskiv/rates/internal/rates"
)

// LoadOverrides reads overrides.json:
//
//	{ "SYP": { "rate": 121.8, "until": "2026-12-31", "note": "..." } }
//
// Keys starting with "_" are comments. A missing file means no overrides.
// The file is edited by hand, so unknown fields are rejected to catch
// typos; time-dependent checks (expiry, horizon) belong to merge.
func LoadOverrides(path string) (merge.Overrides, error) {
	if path == "" {
		return nil, nil
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil, fmt.Errorf("decode overrides: %w", err)
	}
	ov := make(merge.Overrides, len(raw))
	for key, msg := range raw {
		if strings.HasPrefix(key, "_") {
			continue
		}
		o, err := parseOverride(msg)
		if err != nil {
			return nil, fmt.Errorf("override %s: %w", key, err)
		}
		if !rates.ValidCode(key) {
			return nil, fmt.Errorf("override %q: not a currency code", key)
		}
		ov[rates.Code(key)] = o
	}
	return ov, nil
}

func parseOverride(msg json.RawMessage) (merge.Override, error) {
	var e struct {
		Rate  *float64 `json:"rate"`
		Until *string  `json:"until"`
		Note  string   `json:"note"`
	}
	dec := json.NewDecoder(bytes.NewReader(msg))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&e); err != nil {
		return merge.Override{}, err
	}
	if e.Rate == nil || e.Until == nil {
		return merge.Override{}, errors.New("rate and until are required")
	}
	until, err := time.Parse(time.DateOnly, *e.Until)
	if err != nil {
		return merge.Override{}, err
	}
	return merge.Override{Rate: *e.Rate, Until: until, Note: e.Note}, nil
}
