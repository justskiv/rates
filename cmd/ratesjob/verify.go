package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/justskiv/rates/internal/catalog"
	"github.com/justskiv/rates/internal/policy"
	"github.com/justskiv/rates/internal/validate"
)

func verify(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dir := fs.String("dir", "", "published tree to check (the directory holding v1/)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cat, err := catalog.Default()
	if err != nil {
		return err
	}
	pol := policy.Default()
	switch {
	case *dir != "" && fs.NArg() == 0:
		root, err := os.OpenRoot(*dir)
		if err != nil {
			return err
		}
		defer root.Close()
		if err := validate.Tree(root.FS(), cat, pol, time.Now()); err != nil {
			return fmt.Errorf("verify %s:\n%w", *dir, err)
		}
		_, err = fmt.Fprintln(stdout, "ok", *dir)
		return err
	case *dir == "" && fs.NArg() == 1:
		return verifyHost(ctx, fs.Arg(0), cat, pol, stdout)
	default:
		return fmt.Errorf("%w: want --dir DIR or one URL", errUsage)
	}
}

// verifyHost checks latest.json and health.json served by a live host.
func verifyHost(ctx context.Context, base string, cat *catalog.Catalog, pol policy.Policy, stdout io.Writer) error {
	if !strings.HasPrefix(base, "https://") {
		return errors.New("verify: only https hosts are checked")
	}
	base = strings.TrimSuffix(base, "/")
	client := &http.Client{Timeout: 10 * time.Second}

	latest, err := download(ctx, client, base+"/"+validate.LatestPath)
	if err != nil {
		return err
	}
	s, err := validate.Encoded(latest, cat, pol)
	if err == nil {
		err = validate.Fresh(s, time.Time{}, pol, time.Now())
	}
	if err != nil {
		return fmt.Errorf("latest.json: %w", err)
	}
	health, err := download(ctx, client, base+"/"+validate.HealthPath)
	if err != nil {
		return err
	}
	var h validate.Health
	if err := json.Unmarshal(health, &h); err != nil {
		return fmt.Errorf("health.json: %w", err)
	}
	_, err = fmt.Fprintf(stdout, "ok %s asOf=%s rates=%d bytes=%d checkedAt=%s outcome=%s\n",
		base, formatAsOf(s.AsOf), len(s.Rates), len(latest), formatAsOf(h.CheckedAt), h.Outcome)
	return err
}

func download(ctx context.Context, client *http.Client, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "rates-verify/"+version())
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("get %s: status %d", url, resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, validate.MaxWireBytes+1))
	if err != nil {
		return nil, err
	}
	if len(b) > validate.MaxWireBytes {
		return nil, fmt.Errorf("get %s: over %d bytes", url, validate.MaxWireBytes)
	}
	return b, nil
}
