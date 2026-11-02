package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/url"
	"strconv"
	"time"

	"github.com/justskiv/rates/internal/job"
	"github.com/justskiv/rates/internal/source"
)

// runConfig is the configuration of "ratesjob run". Flags override
// environment variables, which override the defaults.
type runConfig struct {
	job            job.Config
	logLevel       slog.Level
	heartbeatURL   string
	frankfurterURL string
	fawazURL       string
	erapiURL       string
}

func parseRunConfig(args []string, getenv func(string) string, stderr io.Writer) (runConfig, error) {
	env := envReader{getenv: getenv}
	var cfg runConfig
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&cfg.job.PublicDir, "public",
		env.str("RATES_PUBLIC_DIR", "/var/lib/rates/public"), "served tree [RATES_PUBLIC_DIR]")
	fs.StringVar(&cfg.job.StateDir, "state",
		env.str("RATES_STATE_DIR", "/var/lib/rates/state"), "state.json directory [RATES_STATE_DIR]")
	fs.StringVar(&cfg.job.ArchiveDir, "archive",
		env.str("RATES_ARCHIVE_DIR", "/var/lib/rates/archive"), "raw source bodies [RATES_ARCHIVE_DIR]")
	fs.StringVar(&cfg.job.OverridesFile, "overrides",
		env.str("RATES_OVERRIDES_FILE", "/etc/rates/overrides.json"), "manual overrides, optional [RATES_OVERRIDES_FILE]")
	fs.BoolVar(&cfg.job.Precompress, "precompress",
		env.boolean("RATES_PRECOMPRESS", true), "write .gz sidecars for Caddy [RATES_PRECOMPRESS]")
	fs.DurationVar(&cfg.job.Timeout, "timeout",
		env.duration("RATES_TIMEOUT", 60*time.Second), "deadline for fetching [RATES_TIMEOUT]")
	fs.StringVar(&cfg.heartbeatURL, "heartbeat-url",
		env.str("RATES_HEARTBEAT_URL", ""), "optional ping after each run [RATES_HEARTBEAT_URL]")
	level := fs.String("log-level", env.str("RATES_LOG_LEVEL", "info"), "debug, info or warn [RATES_LOG_LEVEL]")
	fs.StringVar(&cfg.frankfurterURL, "frankfurter-url",
		env.str("RATES_SRC_FRANKFURTER", source.DefaultFrankfurterURL), "[RATES_SRC_FRANKFURTER]")
	fs.StringVar(&cfg.fawazURL, "fawaz-url",
		env.str("RATES_SRC_FAWAZ", source.DefaultFawazBaseURL), "package base, @version is appended [RATES_SRC_FAWAZ]")
	fs.StringVar(&cfg.erapiURL, "erapi-url",
		env.str("RATES_SRC_ERAPI", source.DefaultERAPIURL), "[RATES_SRC_ERAPI]")
	if err := fs.Parse(args); err != nil {
		return runConfig{}, err
	}
	if env.err != nil {
		return runConfig{}, env.err
	}
	if fs.NArg() > 0 {
		return runConfig{}, fmt.Errorf("%w: unexpected arguments %q", errUsage, fs.Args())
	}
	if err := cfg.logLevel.UnmarshalText([]byte(*level)); err != nil {
		return runConfig{}, fmt.Errorf("log level: %w", err)
	}
	for _, u := range []string{cfg.frankfurterURL, cfg.fawazURL, cfg.erapiURL} {
		if err := checkSourceURL(u); err != nil {
			return runConfig{}, err
		}
	}
	if cfg.heartbeatURL != "" {
		if err := checkSourceURL(cfg.heartbeatURL); err != nil {
			return runConfig{}, fmt.Errorf("heartbeat: %w", err)
		}
	}
	return cfg, nil
}

// checkSourceURL accepts https, and plain http only to a loopback address
// (fake upstreams in tests).
func checkSourceURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return err
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		if ip := net.ParseIP(u.Hostname()); (ip != nil && ip.IsLoopback()) || u.Hostname() == "localhost" {
			return nil
		}
	}
	return fmt.Errorf("url %s: only https is allowed", u.Redacted())
}

// envReader reads typed environment variables and keeps the first error.
type envReader struct {
	getenv func(string) string
	err    error
}

func (e *envReader) str(key, fallback string) string {
	if v := e.getenv(key); v != "" {
		return v
	}
	return fallback
}

func (e *envReader) boolean(key string, fallback bool) bool {
	v := e.getenv(key)
	if v == "" {
		return fallback
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		e.err = errors.Join(e.err, fmt.Errorf("%s: %w", key, err))
		return fallback
	}
	return b
}

func (e *envReader) duration(key string, fallback time.Duration) time.Duration {
	v := e.getenv(key)
	if v == "" {
		return fallback
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		e.err = errors.Join(e.err, fmt.Errorf("%s: %w", key, err))
		return fallback
	}
	return d
}
