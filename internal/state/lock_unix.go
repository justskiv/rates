//go:build unix

package state

import (
	"errors"
	"os"
	"syscall"
)

// Lock takes a non-blocking exclusive flock(2) on the state directory
// itself, so no lock file ends up in the archive. It returns ErrLocked when
// another run holds it; the returned function releases the lock.
func (s *Store) Lock() (unlock func() error, err error) {
	d, err := os.Open(s.path)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(d.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = d.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrLocked
		}
		return nil, err
	}
	return d.Close, nil // closing the descriptor releases the lock
}
