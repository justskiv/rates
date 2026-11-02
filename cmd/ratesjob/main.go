// Command ratesjob fetches currency rates from three upstreams, reconciles
// them and publishes a tree of static files (latest.json, health.json,
// daily history). It is a run-to-completion job without an HTTP server.
//
// Usage:
//
//	ratesjob run [flags]      one fetch-merge-publish cycle
//	ratesjob verify --dir DIR check a published tree
//	ratesjob verify URL       check a live host, e.g. https://rates.tuzov.dev
//	ratesjob version
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"
)

// _version is set at build time:
// -ldflags "-X main._version=rates-v0.3.1+a1b2c3d".
var _version string

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := run(ctx, os.Args[1:], os.Stdout, os.Stderr, os.Getenv)
	stop()
	if err != nil {
		fmt.Fprintln(os.Stderr, "ratesjob:", err)
		os.Exit(1)
	}
}

const _usage = `usage:
  ratesjob run [flags]        fetch, merge and publish once
  ratesjob verify --dir DIR   check a published tree
  ratesjob verify URL         check a live host (https://...)
  ratesjob version            print the version
`

var errUsage = errors.New("invalid usage")

func run(ctx context.Context, args []string, stdout, stderr io.Writer, getenv func(string) string) error {
	if len(args) == 0 {
		fmt.Fprint(stderr, _usage)
		return errUsage
	}
	switch args[0] {
	case "run":
		return runJob(ctx, args[1:], stdout, stderr, getenv)
	case "verify":
		return verify(ctx, args[1:], stdout, stderr)
	case "version":
		_, err := fmt.Fprintln(stdout, version())
		return err
	case "help", "-h", "--help":
		_, err := fmt.Fprint(stdout, _usage)
		return err
	default:
		fmt.Fprint(stderr, _usage)
		return fmt.Errorf("%w: unknown command %q", errUsage, args[0])
	}
}

// version returns the build-time version or, for development builds, the
// VCS revision recorded by the Go toolchain.
func version() string {
	if _version != "" {
		return _version
	}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "dev"
	}
	var rev, dirty string
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			if s.Value == "true" {
				dirty = "-dirty"
			}
		}
	}
	if len(rev) > 7 {
		rev = rev[:7]
	}
	if rev == "" {
		return "dev"
	}
	return "dev+" + rev + dirty
}
