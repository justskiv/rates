// Package source fetches and normalizes the upstream rate feeds:
// Frankfurter v2 (published), fawazahmed0 (second vote and fallback) and
// open.er-api (vote only, never stored or published).
package source

import (
	"context"
	"errors"
	"maps"
	"net/http"
	"slices"
	"time"

	"github.com/justskiv/rates/internal/rates"
)

// ID names an upstream.
type ID string

// Upstream identifiers, also used as keys in state.json and health.json.
const (
	Frankfurter ID = "frankfurter"
	Fawaz       ID = "fawazahmed0"
	ERAPI       ID = "exchangerate-api"
)

// Production endpoints.
const (
	DefaultFrankfurterURL = "https://api.frankfurter.dev/v2/rates?base=USD"
	DefaultFawazBaseURL   = "https://cdn.jsdelivr.net/npm/@fawazahmed0/currency-api"
	DefaultERAPIURL       = "https://open.er-api.com/v6/latest/USD"
)

// Filter selects the codes a parser keeps; everything else is ignored
// without inspecting its value.
type Filter func(rates.Code) bool

// Quote is one upstream's normalized answer: units of currency per 1 USD.
type Quote struct {
	Source ID
	// Date is the observation date; for Frankfurter the mode across codes.
	Date time.Time
	// Dates holds per-code observation dates; nil when the source has none.
	Dates map[rates.Code]time.Time
	Rates map[rates.Code]float64
	// Body is the raw body for raw/; always nil for ERAPI (never persisted).
	Body []byte
	Meta Meta
}

// Clone returns a deep copy of q.
func (q Quote) Clone() Quote {
	q.Dates = maps.Clone(q.Dates)
	q.Rates = maps.Clone(q.Rates)
	q.Body = slices.Clone(q.Body)
	return q
}

// Meta carries protocol details of the exchange that produced a Quote.
type Meta struct {
	URL          string
	ETag         string
	LastModified string
	// NotModified is set when the upstream answered 304 and the cached body
	// was parsed instead.
	NotModified bool
	// Deprecation, Sunset and Successor echo API-change canary headers.
	Deprecation string
	Sunset      string
	Successor   string
	// EOL is set when open.er-api announces the end of its endpoint.
	EOL bool
}

// ParseError reports a body that failed strict parsing. Its message may
// quote upstream content, so callers must not log it for the voter.
type ParseError struct {
	Source ID
	Err    error
}

func (e *ParseError) Error() string {
	return "parse " + string(e.Source) + ": " + e.Err.Error()
}

func (e *ParseError) Unwrap() error {
	return e.Err
}

// Cache is what a previous run knows about an upstream. A conditional
// request is sent only when both a validator and the body are present.
type Cache struct {
	ETag string
	Body []byte
}

// Fetcher downloads and parses one upstream.
type Fetcher struct {
	id     ID
	client *Client
	// urls lists candidate URLs; a 404 moves on to the next one.
	urls        func() []string
	parse       func([]byte) (Quote, error)
	conditional bool
	keepBody    bool
}

// NewFrankfurter fetches Frankfurter v2 with a conditional GET.
func NewFrankfurter(c *Client, url string, keep Filter) *Fetcher {
	return &Fetcher{
		id:          Frankfurter,
		client:      c,
		urls:        func() []string { return []string{url} },
		parse:       func(b []byte) (Quote, error) { return ParseFrankfurter(b, keep) },
		conditional: true,
		keepBody:    true,
	}
}

// NewFawaz fetches fawazahmed0 by dated version first (today, then
// yesterday in UTC) and @latest last: dated versions are immutable, which
// sidesteps the 12-hour jsDelivr cache of @latest.
func NewFawaz(c *Client, baseURL string, keep Filter, now func() time.Time) *Fetcher {
	const path = "/v1/currencies/usd.min.json"
	return &Fetcher{
		id:     Fawaz,
		client: c,
		urls: func() []string {
			today := now().UTC()
			return []string{
				baseURL + "@" + today.Format(time.DateOnly) + path,
				baseURL + "@" + today.AddDate(0, 0, -1).Format(time.DateOnly) + path,
				baseURL + "@latest" + path,
			}
		},
		parse:    func(b []byte) (Quote, error) { return ParseFawaz(b, keep) },
		keepBody: true,
	}
}

// NewERAPI fetches open.er-api unconditionally. Its body is dropped right
// after parsing: the terms forbid redistribution and the archive is public.
func NewERAPI(c *Client, url string, keep Filter) *Fetcher {
	return &Fetcher{
		id:     ERAPI,
		client: c,
		urls:   func() []string { return []string{url} },
		parse:  func(b []byte) (Quote, error) { return ParseERAPI(b, keep) },
	}
}

// ID returns the upstream identifier.
func (f *Fetcher) ID() ID {
	return f.id
}

// Fetch downloads and parses the upstream. On 304 the cached body is parsed
// and the quote is marked NotModified.
func (f *Fetcher) Fetch(ctx context.Context, cache Cache) (Quote, error) {
	urls := f.urls()
	for i, url := range urls {
		hdr := http.Header{}
		if f.conditional && cache.ETag != "" && len(cache.Body) > 0 {
			hdr.Set("If-None-Match", cache.ETag)
		}
		resp, err := f.client.get(ctx, url, hdr)
		if err != nil {
			var se *StatusError
			if errors.As(err, &se) && se.Code == http.StatusNotFound && i < len(urls)-1 {
				continue
			}
			return Quote{}, err
		}
		body := resp.body
		if resp.notModified {
			body = cache.Body
		}
		q, err := f.parse(body)
		if err != nil {
			return Quote{}, &ParseError{Source: f.id, Err: err}
		}
		q.Source = f.id
		m := meta(url, resp, cache)
		m.EOL = q.Meta.EOL
		q.Meta = m
		if f.keepBody {
			q.Body = body
		}
		return q, nil
	}
	return Quote{}, errors.New("no candidate urls")
}

func meta(url string, resp response, cache Cache) Meta {
	m := Meta{
		URL:          url,
		ETag:         resp.header.Get("ETag"),
		LastModified: resp.header.Get("Last-Modified"),
		NotModified:  resp.notModified,
		Deprecation:  resp.header.Get("Deprecation"),
		Sunset:       resp.header.Get("Sunset"),
		Successor:    successorLink(resp.header.Values("Link")),
	}
	if m.NotModified && m.ETag == "" {
		m.ETag = cache.ETag
	}
	return m
}
