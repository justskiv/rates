package job

import (
	"cmp"
	"encoding/json"
	"log/slog"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/justskiv/rates/internal/merge"
	"github.com/justskiv/rates/internal/rates"
	"github.com/justskiv/rates/internal/source"
	"github.com/justskiv/rates/internal/validate"
)

// _maxAttention keeps health.json inside its budget during a storm.
const _maxAttention = 20

// Health is v1/health.json. It holds no values of the voter.
type Health struct {
	SchemaVersion   int                        `json:"schemaVersion"`
	CheckedAt       rates.Time                 `json:"checkedAt"`
	Outcome         Outcome                    `json:"outcome"`
	Reason          string                     `json:"reason"`
	LastPublishedAt rates.Time                 `json:"lastPublishedAt,omitzero"`
	AsOf            rates.Time                 `json:"asOf,omitzero"`
	Version         string                     `json:"version"`
	Sources         map[source.ID]SourceHealth `json:"sources"`
	Counts          Counts                     `json:"counts"`
	Disputed        []rates.Code               `json:"disputed"`
	Held            []rates.Code               `json:"held"`
	Attention       []Attention                `json:"attention"`
}

// SourceHealth is the per-upstream part of health.json.
type SourceHealth struct {
	OK          bool       `json:"ok"`
	LastOkAt    rates.Time `json:"lastOkAt,omitzero"`
	Date        string     `json:"date,omitempty"`
	Codes       int        `json:"codes,omitempty"`
	NotModified bool       `json:"notModified,omitempty"`
	EOL         *bool      `json:"eol,omitempty"`
	Error       string     `json:"error,omitempty"`
}

// Counts describes the snapshot clients currently receive.
type Counts struct {
	Published int `json:"published"`
	Primary   int `json:"primary"`
	Secondary int `json:"secondary"`
	Pegged    int `json:"pegged"`
	Disputed  int `json:"disputed"`
	Held      int `json:"held"`
	Stale     int `json:"stale"`
	Override  int `json:"override"`
}

// Attention is something that needs a human.
type Attention struct {
	Kind   string     `json:"kind"`
	Code   rates.Code `json:"code,omitempty"`
	Source source.ID  `json:"source,omitempty"`
	Since  rates.Time `json:"since,omitzero"`
	Detail string     `json:"detail,omitempty"`
}

func (r *run) attend(a Attention) {
	r.attention = append(r.attention, a)
}

// _attentionKinds maps merge warnings that need a human to attention kinds;
// the rest is logged only.
var _attentionKinds = map[merge.WarningKind]string{
	merge.WarnJumpHeld:            "held",
	merge.WarnJumpConfirmed:       "jumpConfirmed",
	merge.WarnPegMismatch:         "pegMismatch",
	merge.WarnOverrideActive:      "overrideActive",
	merge.WarnOverrideExpiresSoon: "overrideExpiresSoon",
	merge.WarnOverrideRejected:    "overrideRejected",
	merge.WarnSourceDrift:         "sourceDrift",
	merge.WarnAnchorMissing:       "anchorMissing",
}

// noteWarnings logs every warning (one line per currency) and turns the
// actionable ones into attention items.
func (r *run) noteWarnings(ws []merge.Warning) {
	for _, w := range ws {
		attrs := []any{slog.String("detail", w.Detail)}
		if w.Code != "" {
			attrs = append(attrs, slog.String("code", string(w.Code)))
		}
		if w.Source != "" {
			attrs = append(attrs, slog.String("source", string(w.Source)))
		}
		r.deps.Log.Warn(strings.ReplaceAll(string(w.Kind), "_", " "), attrs...)

		kind, ok := _attentionKinds[w.Kind]
		if !ok {
			continue
		}
		a := Attention{Kind: kind, Code: w.Code, Source: w.Source, Detail: w.Detail}
		if !w.Since.IsZero() {
			a.Since = rates.NewTime(w.Since)
			a.Detail = ""
		}
		r.attend(a)
	}
}

func (r *run) health(now time.Time, rep Report) Health {
	h := Health{
		SchemaVersion: 1,
		CheckedAt:     rates.NewTime(now),
		Outcome:       rep.Outcome,
		Reason:        rep.Reason,
		Version:       r.cfg.Version,
		Sources:       make(map[source.ID]SourceHealth, 3),
		Disputed:      []rates.Code{},
		Held:          []rates.Code{},
		Attention:     r.sortedAttention(),
	}
	for _, id := range []source.ID{source.Frankfurter, source.Fawaz, source.ERAPI} {
		h.Sources[id] = r.sources[id] // zero value (ok=false) when not fetched
	}
	if !r.current.published {
		return h
	}
	s := r.current.snap
	h.LastPublishedAt, h.AsOf = s.GeneratedAt, s.AsOf
	for _, code := range slices.Sorted(maps.Keys(s.Rates)) {
		h.Counts.Published++
		if s.Primary(code) {
			h.Counts.Primary++
		}
		for _, f := range s.Flags[code] {
			switch f {
			case rates.FlagSecondary:
				h.Counts.Secondary++
			case rates.FlagPegged:
				h.Counts.Pegged++
			case rates.FlagDisputed:
				h.Counts.Disputed++
				h.Disputed = append(h.Disputed, code)
			case rates.FlagHeld:
				h.Counts.Held++
				h.Held = append(h.Held, code)
			case rates.FlagStale:
				h.Counts.Stale++
			case rates.FlagOverride:
				h.Counts.Override++
			}
		}
	}
	return h
}

func (r *run) sortedAttention() []Attention {
	as := slices.Clone(r.attention)
	slices.SortStableFunc(as, func(a, b Attention) int {
		return cmp.Or(
			cmp.Compare(a.Kind, b.Kind),
			cmp.Compare(a.Source, b.Source),
			cmp.Compare(a.Code, b.Code),
		)
	})
	as = slices.CompactFunc(as, func(a, b Attention) bool { return a == b })
	if len(as) > _maxAttention {
		more := len(as) - _maxAttention + 1
		as = append(as[:_maxAttention-1], Attention{
			Kind:   "truncated",
			Detail: strconv.Itoa(more) + " more items, see the run log",
		})
	}
	if as == nil {
		as = []Attention{}
	}
	return as
}

func (r *run) writeHealth(h Health) error {
	b, err := json.Marshal(h)
	if err != nil {
		return err
	}
	if len(b) > r.deps.Policy.MaxHealthBytes {
		r.deps.Log.Warn("health.json over budget", slog.Int("bytes", len(b)))
	}
	_, err = r.public.Write(validate.HealthPath, b)
	return err
}
