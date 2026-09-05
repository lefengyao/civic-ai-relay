//go:build linux

package memory

import (
	"bufio"
	"os"
	"strconv"
	"strings"
)

// RSS returns the current process resident set size in bytes.
func RSS() (uint64, error) {
	file, err := os.Open("/proc/self/statm")
	if err != nil {
		return 0, err
	}
	defer file.Close()
	fields, err := bufio.NewReader(file).ReadString('\n')
	if err != nil && len(fields) == 0 {
		return 0, err
	}
	parts := strings.Fields(fields)
	if len(parts) < 2 {
		return 0, strconv.ErrSyntax
	}
	pages, err := strconv.ParseUint(parts[1], 10, 64)
	if err != nil {
		return 0, err
	}
	return pages * uint64(os.Getpagesize()), nil
}
