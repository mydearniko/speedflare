package location

import (
	"reflect"
	"testing"
)

func TestSetProbeRanges198Only(t *testing.T) {
	t.Cleanup(func() {
		SetProbeRanges198Only(false)
	})

	SetProbeRanges198Only(false)
	if probeCountPerRange != defaultProbeCountPerRange {
		t.Fatalf("default probe count mismatch: got %d want %d", probeCountPerRange, defaultProbeCountPerRange)
	}
	if !reflect.DeepEqual(probeRanges, defaultProbeRanges) {
		t.Fatalf("default probe ranges mismatch: got %v want %v", probeRanges, defaultProbeRanges)
	}

	SetProbeRanges198Only(true)
	if probeCountPerRange != probe198CountPerRange {
		t.Fatalf("198-only probe count mismatch: got %d want %d", probeCountPerRange, probe198CountPerRange)
	}
	if !reflect.DeepEqual(probeRanges, probe198Ranges) {
		t.Fatalf("198-only probe ranges mismatch: got %v want %v", probeRanges, probe198Ranges)
	}
}
