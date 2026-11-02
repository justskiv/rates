package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/justskiv/rates/internal/fixture"
)

func envFrom(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestParseRunConfigPrecedence(t *testing.T) {
	env := envFrom(map[string]string{
		"RATES_PUBLIC_DIR":  "/env/public",
		"RATES_STATE_DIR":   "/env/state",
		"RATES_PRECOMPRESS": "false",
		"RATES_TIMEOUT":     "30s",
		"RATES_LOG_LEVEL":   "debug",
	})
	cfg, err := parseRunConfig([]string{"--public", "/flag/public", "--timeout", "45s"}, env, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	switch {
	case cfg.job.PublicDir != "/flag/public":
		t.Errorf("flag did not override env: %s", cfg.job.PublicDir)
	case cfg.job.StateDir != "/env/state":
		t.Errorf("env did not override default: %s", cfg.job.StateDir)
	case cfg.job.ArchiveDir != "/var/lib/rates/archive":
		t.Errorf("default lost: %s", cfg.job.ArchiveDir)
	case cfg.job.Precompress:
		t.Error("RATES_PRECOMPRESS=false ignored")
	case cfg.job.Timeout != 45*time.Second:
		t.Errorf("timeout %v", cfg.job.Timeout)
	case cfg.logLevel.String() != "DEBUG":
		t.Errorf("log level %v", cfg.logLevel)
	}
}

func TestParseRunConfigErrors(t *testing.T) {
	tests := []struct {
		name string
		args []string
		env  map[string]string
	}{
		{"bad env bool", nil, map[string]string{"RATES_PRECOMPRESS": "maybe"}},
		{"bad env duration", nil, map[string]string{"RATES_TIMEOUT": "soon"}},
		{"bad level", []string{"--log-level", "loud"}, nil},
		{"plain http source", []string{"--frankfurter-url", "http://api.frankfurter.dev/v2/rates"}, nil},
		{"stray argument", []string{"extra"}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := parseRunConfig(tt.args, envFrom(tt.env), &bytes.Buffer{}); err == nil {
				t.Error("accepted")
			}
		})
	}
}

func TestCheckSourceURL(t *testing.T) {
	for _, ok := range []string{"https://api.frankfurter.dev/v2/rates", "http://127.0.0.1:8080/x", "http://localhost/x", "http://[::1]/x"} {
		if err := checkSourceURL(ok); err != nil {
			t.Errorf("%s rejected: %v", ok, err)
		}
	}
	for _, bad := range []string{"http://api.frankfurter.dev/v2/rates", "ftp://x", "file:///etc/passwd"} {
		if err := checkSourceURL(bad); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
}

// TestRunAgainstFakeUpstreams drives the binary's entry point over real
// HTTP: three fake upstreams serving the recorded responses, a full run,
// then verify --dir on the result.
func TestRunAgainstFakeUpstreams(t *testing.T) {
	bodies := map[string][]byte{
		"/v2/rates": fixture.Read(t, fixture.FrankfurterFile),
		"/npm/@fawazahmed0/currency-api@" + time.Now().UTC().Format(time.DateOnly) + "/v1/currencies/usd.min.json": fixture.Read(t, fixture.FawazFile),
		"/v6/latest/USD": fixture.Read(t, fixture.ERAPIFile),
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.UserAgent(), "rates/") {
			http.Error(w, "anonymous", http.StatusForbidden)
			return
		}
		b, ok := bodies[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(b)
	}))
	defer srv.Close()

	// The recorded data is from 2026-11-01; shift the fixture dates to
	// today so the run accepts them as fresh.
	today := time.Now().UTC().Format(time.DateOnly)
	for path, b := range bodies {
		b = bytes.ReplaceAll(b, []byte("2026-10-30"), []byte(today))
		b = bytes.ReplaceAll(b, []byte("2026-11-02"), []byte(today))
		b = bytes.ReplaceAll(b, []byte("2026-11-01"), []byte(today))
		bodies[path] = b
	}
	var e map[string]any
	if err := json.Unmarshal(bodies["/v6/latest/USD"], &e); err != nil {
		t.Fatal(err)
	}
	e["time_last_update_unix"] = time.Now().Unix()
	bodies["/v6/latest/USD"], _ = json.Marshal(e)

	dir := t.TempDir()
	args := []string{
		"run",
		"--public", filepath.Join(dir, "public"),
		"--state", filepath.Join(dir, "state"),
		"--archive", filepath.Join(dir, "archive"),
		"--overrides", filepath.Join(dir, "overrides.json"),
		"--frankfurter-url", srv.URL + "/v2/rates?base=USD",
		"--fawaz-url", srv.URL + "/npm/@fawazahmed0/currency-api",
		"--erapi-url", srv.URL + "/v6/latest/USD",
	}
	var stdout, stderr bytes.Buffer
	if err := run(context.Background(), args, &stdout, &stderr, envFrom(nil)); err != nil {
		t.Fatalf("run: %v\n%s%s", err, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), `"msg":"run done","outcome":"published"`) {
		t.Errorf("no summary line in the log:\n%s", stdout.String())
	}
	stdout.Reset()
	if err := run(context.Background(), []string{"verify", "--dir", filepath.Join(dir, "public")}, &stdout, &stderr, envFrom(nil)); err != nil {
		t.Fatalf("verify: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "public", "v1", "latest.json.gz")); err != nil {
		t.Errorf("no precompressed sidecar: %v", err)
	}
}

func TestUsage(t *testing.T) {
	var out, errOut bytes.Buffer
	if err := run(context.Background(), nil, &out, &errOut, envFrom(nil)); err == nil {
		t.Error("no command accepted")
	}
	if err := run(context.Background(), []string{"version"}, &out, &errOut, envFrom(nil)); err != nil || out.Len() == 0 {
		t.Errorf("version: %v %q", err, out.String())
	}
	if err := run(context.Background(), []string{"verify", "http://insecure.example"}, &out, &errOut, envFrom(nil)); err == nil {
		t.Error("plain http host accepted by verify")
	}
}
