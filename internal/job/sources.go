package job

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"maps"
	"net/http"
	"path"
	"slices"
	"time"

	"github.com/justskiv/rates/internal/merge"
	"github.com/justskiv/rates/internal/publish"
	"github.com/justskiv/rates/internal/rates"
	"github.com/justskiv/rates/internal/source"
)

// _maxCooldown caps a Retry-After so a hostile header cannot silence a
// source for long.
const _maxCooldown = 6 * time.Hour

// _archived are the sources whose bodies may be kept: the voter's terms
// forbid redistribution and the archive is public in option B.
var _archived = []source.ID{source.Frankfurter, source.Fawaz}

// recordSources updates state and health for every upstream after
// screening: a source dropped by Screen counts as failed for this run.
func (r *run) recordSources(fetched fetchResult, screened merge.Inputs, warns []merge.Warning, now time.Time) {
	kept := map[source.ID]bool{
		source.Frankfurter: screened.Primary != nil,
		source.Fawaz:       screened.Secondary != nil,
		source.ERAPI:       screened.Voter != nil,
	}
	dropped := make(map[source.ID]merge.Warning, len(warns))
	for _, w := range warns {
		if w.Source != "" {
			dropped[w.Source] = w
		}
	}
	for _, id := range slices.Sorted(maps.Keys(fetched)) {
		res := fetched[id]
		switch {
		case res.err != nil:
			r.sourceFailed(id, errorSummary(id, res.err), now)
		case !kept[id]:
			r.sourceFailed(id, string(dropped[id].Kind), now)
		default:
			r.sourceOK(id, *res.quote, now)
		}
		var se *source.StatusError
		if errors.As(res.err, &se) && se.Code == http.StatusTooManyRequests && se.RetryAfter > 0 {
			ss := r.st.Sources[id]
			ss.CooldownUntil = rates.NewTime(now.Add(min(se.RetryAfter, _maxCooldown)))
			r.st.Sources[id] = ss
		}
	}
}

func (r *run) sourceFailed(id source.ID, summary string, now time.Time) {
	ss := r.st.Sources[id]
	if ss.DownSince.IsZero() {
		ss.DownSince = rates.NewTime(now)
	}
	ss.LastError = summary
	r.st.Sources[id] = ss
	r.sources[id] = SourceHealth{OK: false, LastOkAt: ss.LastOkAt, Error: summary}
	r.deps.Log.Warn("source failed", slog.String("source", string(id)), slog.String("err", summary))

	if id == source.Frankfurter && now.Sub(ss.DownSince.Time) > r.deps.Policy.PrimaryDownAfter {
		r.attend(Attention{Kind: "primaryDown", Source: id, Since: ss.DownSince})
	}
}

func (r *run) sourceOK(id source.ID, q source.Quote, now time.Time) {
	ss := r.st.Sources[id]
	ss.LastOkAt = rates.NewTime(now)
	ss.DownSince, ss.CooldownUntil, ss.LastError = rates.Time{}, rates.Time{}, ""
	r.st.Sources[id] = ss

	h := SourceHealth{OK: true, LastOkAt: ss.LastOkAt}
	if id == source.ERAPI {
		// Nothing derived from the voter's values is reported.
		eol := q.Meta.EOL
		h.EOL = &eol
		r.deps.Log.Info("source ok", slog.String("source", string(id)))
	} else {
		h.Date = q.Date.Format(time.DateOnly)
		h.Codes = len(q.Rates)
		r.deps.Log.Info("source ok", slog.String("source", string(id)),
			slog.String("date", h.Date), slog.Int("codes", h.Codes))
	}
	r.sources[id] = h

	if m := q.Meta; m.Deprecation != "" || m.Sunset != "" || m.Successor != "" {
		r.attend(Attention{
			Kind:   "sourceDeprecation",
			Source: id,
			Detail: "deprecation=" + m.Deprecation + " sunset=" + m.Sunset + " successor=" + m.Successor,
		})
	}
	if q.Meta.EOL {
		r.attend(Attention{Kind: "sourceEol", Source: id})
	}
}

// errorSummary renders a fetch error for logs and health.json. Parse
// errors of the voter may quote its values, so only the class is kept.
func errorSummary(id source.ID, err error) string {
	var (
		se *source.StatusError
		pe *source.ParseError
	)
	switch {
	case errors.As(err, &se):
		return se.Error()
	case errors.Is(err, source.ErrBodyTooLarge):
		return "body too large"
	case errors.As(err, &pe) && id == source.ERAPI:
		return "invalid response"
	}
	msg := err.Error()
	if len(msg) > 200 {
		msg = msg[:200]
	}
	return msg
}

// writeArchive stores the bodies of publishable sources whose content
// changed since the last kept body.
func (r *run) writeArchive(fetched fetchResult, now time.Time) error {
	for _, id := range _archived {
		q := fetched[id].quote
		if q == nil || len(q.Body) == 0 {
			continue
		}
		ss := r.st.Sources[id]
		sum := sha256Hex(q.Body)
		if sum == ss.BodySHA256 {
			continue
		}
		gz, err := publish.Gzip(q.Body)
		if err != nil {
			return err
		}
		if _, err := r.archive.Write(rawPath(id, now), gz); err != nil {
			return err
		}
		if _, err := r.archive.Write(lastPath(id), gz); err != nil {
			return err
		}
		ss.BodySHA256 = sum
		r.st.Sources[id] = ss
	}
	return nil
}

// rawPath is raw/YYYY/MM/DD/<src>-<HHMM>Z.json.gz relative to the archive.
func rawPath(id source.ID, now time.Time) string {
	t := now.UTC()
	return path.Join(t.Format("2006/01/02"), string(id)+"-"+t.Format("1504")+"Z.json.gz")
}

func lastPath(id source.ID) string {
	return path.Join("last", string(id)+".json.gz")
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
