package job

import (
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"slices"
	"time"

	"github.com/justskiv/rates/internal/validate"
)

const _day = 24 * time.Hour

// closeDays writes the history file of every finished UTC day that lacks
// one. The file of day D is the snapshot in force at
// 23:59:59Z of D. It runs before anything new is published today, so the
// previous snapshot is exactly that one for every day from its own
// publication up to yesterday. Days before its publication are left for
// the backfill: the snapshot in force then is no longer known.
func (r *run) closeDays(prev previous, now time.Time) ([]time.Time, error) {
	if !prev.published || prev.snap.GeneratedAt.IsZero() {
		return nil, nil
	}
	today := now.UTC().Truncate(_day)
	published := prev.snap.GeneratedAt.UTC()
	if !published.Before(today) {
		return nil, nil
	}
	days, err := r.historyDays()
	if err != nil {
		return nil, err
	}
	start := published.Truncate(_day)
	if n := len(days); n > 0 {
		if next := days[n-1].Add(_day); next.After(start) {
			start = next
		}
	}
	var created []time.Time
	for d := start; d.Before(today); d = d.Add(_day) {
		ok, err := r.public.WriteOnce(validate.HistoryPath(d), prev.bytes)
		if err != nil {
			return created, err
		}
		if ok {
			created = append(created, d)
			r.deps.Log.Info("history day closed", slog.String("day", d.Format(time.DateOnly)))
		}
	}
	return created, nil
}

// historyDays lists the days that have a history file, ascending.
func (r *run) historyDays() ([]time.Time, error) {
	var days []time.Time
	err := fs.WalkDir(r.public.FS(), validate.HistoryDir, func(name string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if day, ok := validate.HistoryDay(name); ok && !d.IsDir() {
			days = append(days, day)
		}
		return nil
	})
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	slices.SortFunc(days, time.Time.Compare)
	return days, nil
}

// historyIndex is v1/history/index.json.
type historyIndex struct {
	First   string   `json:"first"`
	Last    string   `json:"last"`
	Missing []string `json:"missing"`
}

// writeHistoryIndex refreshes index.json; Write leaves it untouched when
// the content is the same.
func (r *run) writeHistoryIndex() error {
	days, err := r.historyDays()
	if err != nil || len(days) == 0 {
		return err
	}
	idx := historyIndex{
		First:   days[0].Format(time.DateOnly),
		Last:    days[len(days)-1].Format(time.DateOnly),
		Missing: []string{},
	}
	have := make(map[time.Time]bool, len(days))
	for _, d := range days {
		have[d] = true
	}
	for d := days[0]; d.Before(days[len(days)-1]); d = d.Add(_day) {
		if !have[d] {
			idx.Missing = append(idx.Missing, d.Format(time.DateOnly))
		}
	}
	b, err := json.Marshal(idx)
	if err != nil {
		return err
	}
	_, err = r.public.Write(validate.HistoryIndexPath, b)
	return err
}
