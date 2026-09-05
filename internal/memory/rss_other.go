//go:build !linux && !windows

package memory

func RSS() (uint64, error) { return 0, ErrUnsupported }
