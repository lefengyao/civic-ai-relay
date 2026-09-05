package memory

import (
	"errors"
	"testing"
)

func TestGuardRejectsOnlyPublicWorkAboveLimit(t *testing.T) {
	guard := NewGuard(200, func() (uint64, error) { return 201 << 20, nil })
	if err := guard.PublicAdmission(); !errors.Is(err, ErrLimitExceeded) {
		t.Fatalf("err = %v", err)
	}
}

func TestGuardAllowsUnsupportedRSSReader(t *testing.T) {
	guard := NewGuard(1, func() (uint64, error) { return 0, ErrUnsupported })
	if err := guard.PublicAdmission(); err != nil {
		t.Fatalf("err = %v", err)
	}
}

func TestStreamGuardStopsAfterLimitCrossing(t *testing.T) {
	readings := []uint64{100 << 20, 201 << 20}
	guard := NewGuard(200, func() (uint64, error) {
		value := readings[0]
		readings = readings[1:]
		return value, nil
	})
	if err := guard.PublicAdmission(); err != nil {
		t.Fatalf("first sample: %v", err)
	}
	if err := guard.StreamContinue(); !errors.Is(err, ErrLimitExceeded) {
		t.Fatalf("second sample: %v", err)
	}
}
