//go:build windows

package config

import (
	"errors"
	"os"
)

// Windows Rename does not replace an existing destination. Remove it first so
// writes succeed for the normal update path; the temporary file is cleaned by
// the caller if the final rename fails.
func replaceFile(source, destination string) error {
	if err := os.Remove(destination); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return os.Rename(source, destination)
}
