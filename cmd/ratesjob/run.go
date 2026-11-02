package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/justskiv/rates/internal/catalog"
	"github.com/justskiv/rates/internal/job"
	"github.com/justskiv/rates/internal/policy"
	"github.com/justskiv/rates/internal/rates"
	"github.com/justskiv/rates/internal/source"
)

func runJob(ctx context.Context, args []string, stdout, stderr io.Writer, getenv func(string) string) error {
	cfg, err := parseRunConfig(args, getenv, stderr)
	if err != nil {
		return err
	}
	log := slog.New(slog.NewJSONHandler(stdout, &slog.HandlerOptions{Level: cfg.logLevel}))
	cat, err := catalog.Default()
	if err != nil {
		return err
	}
	cfg.job.Version = version()
	userAgent := "rates/" + cfg.job.Version + " (+https://rates.tuzov.dev)"
	client := source.NewClient(source.ClientConfig{UserAgent: userAgent})
	defer client.CloseIdleConnections()

	keep := source.Filter(cat.Contains)
	j, err := job.New(cfg.job, job.Deps{
		Primary:   source.NewFrankfurter(client, cfg.frankfurterURL, keep),
		Secondary: source.NewFawaz(client, cfg.fawazURL, keep, time.Now),
		Voter:     source.NewERAPI(client, cfg.erapiURL, keep),
		Catalog:   cat,
		Policy:    policy.Default(),
		Log:       log,
		Now:       time.Now,
	})
	if err != nil {
		return err
	}
	rep, runErr := j.Run(ctx)
	if rep.Outcome != job.OutcomeSkipped {
		if err := ping(ctx, cfg.heartbeatURL, userAgent, rep.Outcome); err != nil {
			log.Warn("heartbeat failed", slog.Any("err", err))
		}
	}
	return runErr
}

// ping reports the outcome to an optional heartbeat service in the
// healthchecks.io convention: URL on success, URL/fail otherwise.
func ping(ctx context.Context, url, userAgent string, outcome job.Outcome) error {
	if url == "" {
		return nil
	}
	if outcome != job.OutcomePublished && outcome != job.OutcomeUnchanged {
		url += "/fail"
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("heartbeat status %d", resp.StatusCode)
	}
	return nil
}

// formatAsOf renders an optional timestamp for human output.
func formatAsOf(t rates.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.UTC().Format(time.RFC3339)
}
