// Package memory provides a small RSS-based soft admission guard.
package memory

import (
	"errors"
)

var (
	ErrLimitExceeded = errors.New("memory limit exceeded")
	ErrUnsupported   = errors.New("resident set size is unsupported")
)

type Reader func() (uint64, error)

type Guard struct {
	limitBytes uint64
	read       Reader
}

func NewGuard(limitMB int, read Reader) Guard {
	if limitMB < 1 {
		limitMB = 1
	}
	return Guard{limitBytes: uint64(limitMB) * 1024 * 1024, read: read}
}

func (g Guard) PublicAdmission() error { return g.check() }

func (g Guard) StreamContinue() error { return g.check() }

func (g Guard) check() error {
	if g.read == nil {
		return nil
	}
	rss, err := g.read()
	if err != nil {
		if errors.Is(err, ErrUnsupported) {
			return nil
		}
		// RSS is a soft guard. A transient platform read failure should not
		// turn into an outage; the next request/stream sample retries it.
		return nil
	}
	if rss > g.limitBytes {
		return ErrLimitExceeded
	}
	return nil
}
