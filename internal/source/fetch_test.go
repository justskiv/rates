package source_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/justskiv/rates/internal/fixture"
	"github.com/justskiv/rates/internal/source"
)

const _ua = "rates/test (+https://rates.tuzov.dev)"

// upstream is a fake server that answers with a scripted handler and
// counts requests.
type upstream struct {
	*httptest.Server
	hits atomic.Int32
}

func newUpstream(t *testing.T, h http.HandlerFunc) *upstream {
	t.Helper()
	u := &upstream{}
	u.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.hits.Add(1)
		h(w, r)
	}))
	t.Cleanup(u.Close)
	return u
}

func fastClient() *source.Client {
	return source.NewClient(source.ClientConfig{
		UserAgent:      _ua,
		RetryDelay:     time.Millisecond,
		AttemptTimeout: 2 * time.Second,
	})
}

func keep(t *testing.T) source.Filter {
	return fixture.Catalog(t).Contains
}

func frankfurterBody(t *testing.T) []byte {
	return fixture.Read(t, fixture.FrankfurterFile)
}

func TestFetchOK(t *testing.T) {
	body := frankfurterBody(t)
	up := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("User-Agent") != _ua {
			http.Error(w, "anonymous user agent", http.StatusForbidden)
			return
		}
		if r.Header.Get("If-None-Match") != "" || r.Header.Get("If-Modified-Since") != "" {
			t.Error("conditional request")
		}
		w.Header().Set("ETag", `"v1"`)
		w.Header().Set("Deprecation", "@1779103800")
		w.Header().Set("Link", `<https://api.frankfurter.dev/v3/rates>; rel="successor-version"`)
		_, _ = w.Write(body)
	})
	f := source.NewFrankfurter(fastClient(), up.URL, keep(t))
	q, err := f.Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if q.Source != source.Frankfurter || len(q.Rates) < 150 || !bytes.Equal(q.Body, body) {
		t.Errorf("unexpected quote: source %s, %d rates", q.Source, len(q.Rates))
	}
	m := q.Meta
	if m.Deprecation != "@1779103800" || m.Successor != "https://api.frankfurter.dev/v3/rates" {
		t.Errorf("meta %+v", m)
	}
}

func TestFetchFailures(t *testing.T) {
	big := bytes.Repeat([]byte(" "), source.DefaultMaxBody+1)
	var bomb bytes.Buffer
	zw := gzip.NewWriter(&bomb)
	_, _ = zw.Write(big)
	_ = zw.Close()

	tests := []struct {
		name     string
		handler  http.HandlerFunc
		wantHits int32
		check    func(t *testing.T, err error)
	}{
		{
			name:     "body over 1 MiB",
			handler:  func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(big) },
			wantHits: 1,
			check:    wantIs(source.ErrBodyTooLarge),
		},
		{
			name: "gzip bomb",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Encoding", "gzip")
				_, _ = w.Write(bomb.Bytes())
			},
			wantHits: 1,
			check:    wantIs(source.ErrBodyTooLarge),
		},
		{
			name: "429 with Retry-After is not retried",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Retry-After", "1200")
				w.WriteHeader(http.StatusTooManyRequests)
			},
			wantHits: 1,
			check: func(t *testing.T, err error) {
				var se *source.StatusError
				if !errors.As(err, &se) || se.Code != 429 || se.RetryAfter != 20*time.Minute {
					t.Errorf("err = %v", err)
				}
			},
		},
		{
			name:     "404 is not retried",
			handler:  func(w http.ResponseWriter, _ *http.Request) { http.NotFound(w, nil) },
			wantHits: 1,
			check:    wantStatus(404),
		},
		{
			name:     "503 is retried once",
			handler:  func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) },
			wantHits: 2,
			check:    wantStatus(503),
		},
		{
			name: "dropped connection is retried once",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				hj, ok := w.(http.Hijacker)
				if !ok {
					panic("response writer cannot hijack")
				}
				if conn, _, err := hj.Hijack(); err == nil {
					_ = conn.Close()
				}
			},
			wantHits: 2,
			check: func(t *testing.T, err error) {
				if err == nil {
					t.Error("no error")
				}
			},
		},
		{
			name: "304 is an error: no validator is ever sent",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusNotModified)
			},
			wantHits: 1,
			check:    wantStatus(304),
		},
		{
			name: "broken json",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, `[{"date":`)
			},
			wantHits: 1,
			check: func(t *testing.T, err error) {
				var pe *source.ParseError
				if !errors.As(err, &pe) {
					t.Errorf("err = %v, want ParseError", err)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			up := newUpstream(t, tt.handler)
			_, err := source.NewFrankfurter(fastClient(), up.URL, keep(t)).Fetch(context.Background())
			tt.check(t, err)
			if got := up.hits.Load(); got != tt.wantHits {
				t.Errorf("%d requests, want %d", got, tt.wantHits)
			}
		})
	}
}

func TestFetchRetrySucceeds(t *testing.T) {
	body := frankfurterBody(t)
	var n atomic.Int32
	up := newUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		if n.Add(1) == 1 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		_, _ = w.Write(body)
	})
	if _, err := source.NewFrankfurter(fastClient(), up.URL, keep(t)).Fetch(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestFetchRejectsRedirectToHTTP(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://downgrade.invalid/rates", http.StatusFound)
	}))
	defer srv.Close()
	c := source.NewClient(source.ClientConfig{UserAgent: _ua, Transport: srv.Client().Transport})
	_, err := source.NewFrankfurter(c, srv.URL, keep(t)).Fetch(context.Background())
	if err == nil || !strings.Contains(err.Error(), "non-https") {
		t.Errorf("err = %v, want a refused downgrade", err)
	}
}

func TestFetchFawazFallsBackByDate(t *testing.T) {
	body := fixture.Read(t, fixture.FawazFile)
	var paths []string
	up := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if strings.Contains(r.URL.Path, "@2026-11-01/") {
			_, _ = w.Write(body)
			return
		}
		http.NotFound(w, r)
	})
	now := func() time.Time { return time.Date(2026, 11, 2, 0, 30, 0, 0, time.UTC) }
	f := source.NewFawaz(fastClient(), up.URL+"/npm/@fawazahmed0/currency-api", keep(t), now)
	q, err := f.Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"/npm/@fawazahmed0/currency-api@2026-11-02/v1/currencies/usd.min.json",
		"/npm/@fawazahmed0/currency-api@2026-11-01/v1/currencies/usd.min.json",
	}
	if strings.Join(paths, ",") != strings.Join(want, ",") {
		t.Errorf("requested %v, want %v", paths, want)
	}
	if !strings.HasSuffix(q.Meta.URL, want[1]) {
		t.Errorf("meta url %s", q.Meta.URL)
	}

	paths = nil
	up.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		http.NotFound(w, r)
	})
	if _, err := f.Fetch(context.Background()); err == nil || len(paths) != 3 {
		t.Errorf("err = %v after %d requests, want an error after today, yesterday, latest", err, len(paths))
	}
}

// roundTripFunc lets a test answer requests without a network.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// The retry waits RetryDelay ±50%: checked on a fake clock.
func TestFetchRetryDelay(t *testing.T) {
	body := frankfurterBody(t)
	for _, tt := range []struct {
		jitter float64
		want   time.Duration
	}{
		{0, 2500 * time.Millisecond},
		{0.5, 5 * time.Second},
		{0.999, 7495 * time.Millisecond},
	} {
		synctest.Test(t, func(t *testing.T) {
			calls := 0
			var at []time.Time
			rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				at = append(at, time.Now())
				status := http.StatusServiceUnavailable
				if calls > 1 {
					status = http.StatusOK
				}
				return &http.Response{
					StatusCode: status,
					Header:     http.Header{},
					Body:       io.NopCloser(bytes.NewReader(body)),
					Request:    r,
				}, nil
			})
			c := source.NewClient(source.ClientConfig{
				UserAgent: _ua,
				Transport: rt,
				Jitter:    func() float64 { return tt.jitter },
			})
			if _, err := source.NewFrankfurter(c, "https://api.invalid/v2/rates", keep(t)).Fetch(t.Context()); err != nil {
				t.Fatal(err)
			}
			if got := at[1].Sub(at[0]).Round(time.Millisecond); got != tt.want {
				t.Errorf("jitter %v: waited %v, want %v", tt.jitter, got, tt.want)
			}
		})
	}
}

// A cancelled run does not wait for the retry.
func TestFetchRetryHonoursContext(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 503, Header: http.Header{}, Body: http.NoBody, Request: r}, nil
		})
		c := source.NewClient(source.ClientConfig{UserAgent: _ua, Transport: rt})
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		start := time.Now()
		_, err := source.NewFrankfurter(c, "https://api.invalid/v2/rates", keep(t)).Fetch(ctx)
		if err == nil || time.Since(start) != time.Second {
			t.Errorf("err = %v after %v, want deadline after 1s", err, time.Since(start))
		}
	})
}

func wantIs(target error) func(*testing.T, error) {
	return func(t *testing.T, err error) {
		t.Helper()
		if !errors.Is(err, target) {
			t.Errorf("err = %v, want %v", err, target)
		}
	}
}

func wantStatus(code int) func(*testing.T, error) {
	return func(t *testing.T, err error) {
		t.Helper()
		var se *source.StatusError
		if !errors.As(err, &se) || se.Code != code {
			t.Errorf("err = %v, want status %d", err, code)
		}
	}
}
