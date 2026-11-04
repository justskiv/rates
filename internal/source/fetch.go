package source

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Client defaults.
const (
	DefaultAttemptTimeout = 10 * time.Second
	DefaultRetryDelay     = 5 * time.Second
	DefaultMaxBody        = 1 << 20
	_maxRedirects         = 3
)

var (
	// ErrBodyTooLarge is returned when a response exceeds the body limit
	// after decompression.
	ErrBodyTooLarge = errors.New("response body exceeds limit")

	// errRedirect marks a refused redirect: deterministic, never retried.
	errRedirect = errors.New("redirect refused")
)

// StatusError reports an HTTP status other than 200.
type StatusError struct {
	Code       int
	RetryAfter time.Duration // zero when the header is absent or invalid
}

func (e *StatusError) Error() string {
	return "unexpected status " + strconv.Itoa(e.Code)
}

// ClientConfig configures a Client. Zero fields take the defaults.
type ClientConfig struct {
	// UserAgent identifies us to upstreams; required.
	UserAgent string
	// Transport replaces the default hardened transport (tests only).
	Transport http.RoundTripper
	// AttemptTimeout bounds one request including the body.
	AttemptTimeout time.Duration
	// RetryDelay is the pause before the single retry, jittered by ±50%.
	RetryDelay time.Duration
	// MaxBody is the largest accepted decompressed body.
	MaxBody int64
	// Jitter returns a value in [0, 1); defaults to math/rand/v2.
	Jitter func() float64
}

// Client performs GET requests against upstreams with size limits, an
// https-only redirect policy and one retry for transient failures. One
// Client is shared by all sources of a run.
type Client struct {
	http       *http.Client
	userAgent  string
	retryDelay time.Duration
	maxBody    int64
	jitter     func() float64
}

// NewClient builds a Client from cfg.
func NewClient(cfg ClientConfig) *Client {
	transport := cfg.Transport
	if transport == nil {
		transport = &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			DialContext:           (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
			TLSHandshakeTimeout:   5 * time.Second,
			ResponseHeaderTimeout: 10 * time.Second,
			MaxIdleConnsPerHost:   2,
			IdleConnTimeout:       30 * time.Second,
			ForceAttemptHTTP2:     true,
			TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
		}
	}
	c := &Client{
		http: &http.Client{
			Transport:     transport,
			Timeout:       cmpOr(cfg.AttemptTimeout, DefaultAttemptTimeout),
			CheckRedirect: httpsOnlyMaxRedirects(_maxRedirects),
		},
		userAgent:  cfg.UserAgent,
		retryDelay: cmpOr(cfg.RetryDelay, DefaultRetryDelay),
		maxBody:    cmpOr(cfg.MaxBody, DefaultMaxBody),
		jitter:     cfg.Jitter,
	}
	if c.jitter == nil {
		c.jitter = rand.Float64
	}
	return c
}

// CloseIdleConnections releases pooled connections at the end of a run.
func (c *Client) CloseIdleConnections() {
	c.http.CloseIdleConnections()
}

func cmpOr[T comparable](v, fallback T) T {
	var zero T
	if v == zero {
		return fallback
	}
	return v
}

func httpsOnlyMaxRedirects(limit int) func(*http.Request, []*http.Request) error {
	return func(req *http.Request, via []*http.Request) error {
		if len(via) >= limit {
			return fmt.Errorf("%w: stopped after %d redirects", errRedirect, limit)
		}
		if req.URL.Scheme != "https" {
			return fmt.Errorf("%w: non-https url %s", errRedirect, req.URL.Redacted())
		}
		return nil
	}
}

// response is a successful exchange.
type response struct {
	body   []byte
	header http.Header
}

// get performs a GET with at most one retry.
func (c *Client) get(ctx context.Context, url string) (response, error) {
	resp, retry, err := c.attempt(ctx, url)
	if err == nil || !retry {
		return resp, err
	}
	delay := time.Duration(float64(c.retryDelay) * (0.5 + c.jitter()))
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return response{}, fmt.Errorf("%w (retry abandoned: %w)", err, context.Cause(ctx))
	case <-timer.C:
	}
	resp, _, err = c.attempt(ctx, url)
	return resp, err
}

// attempt performs one request. retry reports whether the failure is
// transient: a network error, 408 or 5xx. 429 is deliberately not retried.
func (c *Client) attempt(ctx context.Context, url string) (_ response, retry bool, _ error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return response{}, false, err
	}
	req.Header.Set("User-Agent", c.userAgent)
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return response{}, ctx.Err() == nil && !errors.Is(err, errRedirect), err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		// Drain the body so the connection is reused.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, c.maxBody))
		se := &StatusError{
			Code:       resp.StatusCode,
			RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After"), time.Now()),
		}
		transient := resp.StatusCode == http.StatusRequestTimeout || resp.StatusCode >= 500
		return response{}, transient, fmt.Errorf("get %s: %w", req.URL.Redacted(), se)
	}

	// The transport decompresses gzip transparently, so the limit applies
	// to decompressed bytes and a gzip bomb stops here.
	body, err := io.ReadAll(io.LimitReader(resp.Body, c.maxBody+1))
	if err != nil {
		return response{}, ctx.Err() == nil, fmt.Errorf("read %s: %w", req.URL.Redacted(), err)
	}
	if int64(len(body)) > c.maxBody {
		return response{}, false, fmt.Errorf("read %s: %w", req.URL.Redacted(), ErrBodyTooLarge)
	}
	return response{body: body, header: resp.Header}, false, nil
}

// parseRetryAfter accepts delay-seconds or an HTTP date.
func parseRetryAfter(v string, now time.Time) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil {
		if secs < 0 {
			return 0
		}
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil && t.After(now) {
		return t.Sub(now)
	}
	return 0
}

// successorLink extracts the target of a Link header entry with
// rel="successor-version", if any.
func successorLink(values []string) string {
	for _, v := range values {
		for part := range strings.SplitSeq(v, ",") {
			target, params, ok := strings.Cut(part, ";")
			if !ok {
				continue
			}
			for p := range strings.SplitSeq(params, ";") {
				name, value, _ := strings.Cut(strings.TrimSpace(p), "=")
				if strings.EqualFold(name, "rel") &&
					strings.Contains(strings.Trim(value, `"`), "successor-version") {
					return strings.Trim(strings.TrimSpace(target), "<>")
				}
			}
		}
	}
	return ""
}
