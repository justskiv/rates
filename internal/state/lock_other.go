//go:build !unix

package state

import "errors"

// Lock is not implemented outside Unix: the job runs on Linux (Actions,
// systemd) and macOS for development.
func (s *Store) Lock() (unlock func() error, err error) {
	return nil, errors.ErrUnsupported
}
