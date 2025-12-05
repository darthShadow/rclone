//go:build linux

package mountlib

import (
	"os"
	"testing"
)

func TestGetKernelReadAhead(t *testing.T) {
	// Test on a real mounted filesystem (root fs always exists)
	val, err := GetKernelReadAhead("/")
	if err != nil {
		t.Skipf("Cannot read sysfs (may need root or sysfs not mounted): %v", err)
	}
	if val <= 0 {
		t.Errorf("Expected positive readahead value, got %d", val)
	}
	t.Logf("Root filesystem readahead: %d KiB", val)
}

func TestTuneKernelReadAhead_PermissionDenied(t *testing.T) {
	// Non-root users should get a permission error, not a crash
	if os.Getuid() == 0 {
		t.Skip("Test requires non-root user")
	}

	err := TuneKernelReadAhead("/", 1024)
	if err == nil {
		t.Error("Expected permission error for non-root user")
	}
	// Verify it's a sensible error, not a panic
	t.Logf("Expected error: %v", err)
}

func TestEnforceMinReadAheadKiB(t *testing.T) {
	tests := []struct {
		name     string
		input    int
		expected int
	}{
		{"zero passes through", 0, 0},
		{"below minimum raised to 128", 64, 128},
		{"well below minimum raised to 128", 1, 128},
		{"exactly minimum unchanged", 128, 128},
		{"above minimum unchanged", 256, 256},
		{"large value unchanged", 1024, 1024},
		{"negative normalized to zero", -1, 0},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := EnforceMinReadAheadKiB(tc.input)
			if got != tc.expected {
				t.Errorf("EnforceMinReadAheadKiB(%d) = %d, want %d", tc.input, got, tc.expected)
			}
		})
	}
}

func TestGetEffectiveMaxWrite(t *testing.T) {
	maxWrite := GetEffectiveMaxWrite()

	// Should return a positive, reasonable value (at least 64 KiB, typical minimum)
	minReasonable := 64 * 1024
	if maxWrite < minReasonable {
		t.Errorf("GetEffectiveMaxWrite() = %d, want >= %d", maxWrite, minReasonable)
	}

	// Should be page-aligned
	pageSize := os.Getpagesize()
	if maxWrite%pageSize != 0 {
		t.Errorf("GetEffectiveMaxWrite() = %d, not page-aligned (page size %d)", maxWrite, pageSize)
	}

	// Log the result for visibility
	if _, err := os.Stat(procMaxPagesLimit); err == nil {
		t.Logf("Kernel 6.13+: max_pages_limit present, effective MaxWrite = %d bytes", maxWrite)
	} else {
		t.Logf("Kernel <6.13: using DefaultMaxWrite = %d bytes", maxWrite)
	}
}
