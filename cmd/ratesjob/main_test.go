package main

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
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

// todayBodies returns the recorded upstream answers keyed by request
// path. The recorded data is from 2026-11-01; the dates are shifted to
// today so a run accepts them as fresh.
func todayBodies(t *testing.T) map[string][]byte {
	t.Helper()
	today := time.Now().UTC().Format(time.DateOnly)
	bodies := map[string][]byte{
		"/v2/rates": fixture.Read(t, fixture.FrankfurterFile),
		"/npm/@fawazahmed0/currency-api@" + today + "/v1/currencies/usd.min.json": fixture.Read(t, fixture.FawazFile),
		"/v6/latest/USD": fixture.Read(t, fixture.ERAPIFile),
	}
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
	return bodies
}

// fakeUpstreams serves bodies by request path. A non-nil frankfurter
// answers /v2/rates instead.
func fakeUpstreams(t *testing.T, bodies map[string][]byte, frankfurter http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.UserAgent(), "rates/") {
			http.Error(w, "anonymous", http.StatusForbidden)
			return
		}
		if frankfurter != nil && r.URL.Path == "/v2/rates" {
			frankfurter(w, r)
			return
		}
		b, ok := bodies[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(b)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// runArgs points "ratesjob run" at the directories under dir and at the
// fake upstreams behind baseURL.
func runArgs(dir, baseURL string) []string {
	return []string{
		"run",
		"--public", filepath.Join(dir, "public"),
		"--state", filepath.Join(dir, "state"),
		"--archive", filepath.Join(dir, "archive"),
		"--overrides", filepath.Join(dir, "overrides.json"),
		"--frankfurter-url", baseURL + "/v2/rates?base=USD",
		"--fawaz-url", baseURL + "/npm/@fawazahmed0/currency-api",
		"--erapi-url", baseURL + "/v6/latest/USD",
	}
}

// TestRunAgainstFakeUpstreams drives the binary's entry point over real
// HTTP: three fake upstreams serving the recorded responses, a full run,
// then verify --dir on the result.
func TestRunAgainstFakeUpstreams(t *testing.T) {
	srv := fakeUpstreams(t, todayBodies(t), nil)
	dir := t.TempDir()
	var stdout, stderr bytes.Buffer
	if err := run(context.Background(), runArgs(dir, srv.URL), &stdout, &stderr, envFrom(nil)); err != nil {
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

// Frankfurter's ETag is the MD5 of the newest date in the answer, not of
// the body: the first providers of a day change it, and the rest of the
// day's quotes arrive under the same ETag. A job that revalidated with
// If-None-Match got 304, kept the early body, whose mode was still the
// previous day, and published yesterday's asOf all day (2026-11-04).
func TestRunPicksUpQuotesAddedUnderTheSameETag(t *testing.T) {
	now := time.Now().UTC()
	today := now.Format(time.DateOnly)
	yesterday := now.AddDate(0, 0, -1).Format(time.DateOnly)
	bodies := todayBodies(t)
	// Shortly after midnight only a few providers have today's rates.
	early := redate(t, bodies["/v2/rates"], func(i int) string {
		if i < 10 {
			return today
		}
		return yesterday
	})
	full := redate(t, bodies["/v2/rates"], func(int) string { return today })

	sum := md5.Sum([]byte(today + "|"))
	etag := `"` + hex.EncodeToString(sum[:]) + `"`
	var current atomic.Pointer[[]byte]
	srv := fakeUpstreams(t, bodies, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", etag)
		if inm := r.Header.Get("If-None-Match"); inm == etag || inm == "W/"+etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		_, _ = w.Write(*current.Load())
	})

	dir := t.TempDir()
	for _, step := range []struct {
		body []byte
		want string
	}{
		{early, yesterday},
		{full, today},
	} {
		current.Store(&step.body)
		var stdout, stderr bytes.Buffer
		if err := run(context.Background(), runArgs(dir, srv.URL), &stdout, &stderr, envFrom(nil)); err != nil {
			t.Fatalf("run: %v\n%s%s", err, stdout.String(), stderr.String())
		}
		if got := latestAsOf(t, dir); got != step.want+"T00:00:00Z" {
			t.Fatalf("asOf %s, want %s\n%s", got, step.want, stdout.String())
		}
	}
}

// redate sets the date of every quote in a Frankfurter v2 body; date(i)
// gives the date of the i-th quote.
func redate(t *testing.T, body []byte, date func(i int) string) []byte {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var quotes []map[string]any
	if err := dec.Decode(&quotes); err != nil {
		t.Fatal(err)
	}
	for i, q := range quotes {
		q["date"] = date(i)
	}
	b, err := json.Marshal(quotes)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func latestAsOf(t *testing.T, dir string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "public", "v1", "latest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var latest struct {
		AsOf string `json:"asOf"`
	}
	if err := json.Unmarshal(b, &latest); err != nil {
		t.Fatal(err)
	}
	return latest.AsOf
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
