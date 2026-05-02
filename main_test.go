package main

import "testing"

func TestCalculateVolumeAndPercent(t *testing.T) {
	tests := []struct {
		name       string
		distanceCm *float64
		wantVolume *float64
		wantLevel  *float64
	}{
		{
			name:       "nil distance",
			distanceCm: nil,
			wantVolume: nil,
			wantLevel:  nil,
		},
		{
			name:       "empty tank",
			distanceCm: ptr(32.5),
			wantVolume: ptr(0),
			wantLevel:  ptr(0),
		},
		{
			name:       "full tank",
			distanceCm: ptr(20.5),
			wantVolume: ptr(12),
			wantLevel:  ptr(100),
		},
		{
			name:       "half tank",
			distanceCm: ptr(26.5),
			wantVolume: ptr(6),
			wantLevel:  ptr(50),
		},
		{
			name:       "above full clamps",
			distanceCm: ptr(19),
			wantVolume: ptr(12),
			wantLevel:  ptr(100),
		},
		{
			name:       "below empty clamps",
			distanceCm: ptr(40),
			wantVolume: ptr(0),
			wantLevel:  ptr(0),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotVolume, gotLevel := calculateVolumeAndPercent(tt.distanceCm)

			assertFloatPtrEqual(t, "volume", gotVolume, tt.wantVolume)
			assertFloatPtrEqual(t, "level", gotLevel, tt.wantLevel)
		})
	}
}

func ptr(v float64) *float64 {
	return &v
}

func assertFloatPtrEqual(t *testing.T, name string, got, want *float64) {
	t.Helper()

	if got == nil || want == nil {
		if got != want {
			t.Fatalf("%s = %v, want %v", name, got, want)
		}
		return
	}

	if *got != *want {
		t.Fatalf("%s = %v, want %v", name, *got, *want)
	}
}
