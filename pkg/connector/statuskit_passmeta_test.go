package connector

import (
	"testing"
	"time"
)

// TestStatusKitPassMetaParseLegacyTwoFieldRow pins the backward-compat path:
// rows written before the 6-field format existed ("<ms>:<errs>") must still
// parse, with every flag defaulting false (which keeps a not-yet-established
// account on the fresh floor).
func TestStatusKitPassMetaParseLegacyTwoFieldRow(t *testing.T) {
	raw := "1700000000000:3"
	m := parseStatusKitPassMeta(&raw)

	if m.ConsecutiveErrs != 3 {
		t.Fatalf("ConsecutiveErrs = %d, want 3", m.ConsecutiveErrs)
	}
	if m.LastAttempt.UnixMilli() != 1700000000000 {
		t.Fatalf("LastAttempt = %v, want UnixMilli 1700000000000", m.LastAttempt)
	}
	if m.Established {
		t.Error("Established should default false for a 2-field row")
	}
	if m.LastPulledKeys {
		t.Error("LastPulledKeys should default false for a 2-field row")
	}
	if m.DrainComplete {
		t.Error("DrainComplete should default false for a 2-field row")
	}
	if m.RecoveryRepullDone {
		t.Error("RecoveryRepullDone should default false for a 2-field row")
	}
}

// TestStatusKitPassMetaEncodeRoundTripAllFlags checks that a fully-set meta
// survives encode() → parseStatusKitPassMeta() (the 6th field,
// RecoveryRepullDone, is the one added by the stranded-keys self-heal).
func TestStatusKitPassMetaEncodeRoundTripAllFlags(t *testing.T) {
	original := statusKitPassMeta{
		LastAttempt:        time.UnixMilli(1700000000000),
		ConsecutiveErrs:    2,
		Established:        true,
		LastPulledKeys:     true,
		DrainComplete:      true,
		RecoveryRepullDone: true,
	}

	encoded := original.encode()
	if want := "1700000000000:2:1:1:1:1"; encoded != want {
		t.Fatalf("encode() = %q, want %q", encoded, want)
	}

	parsed := parseStatusKitPassMeta(&encoded)
	if parsed.LastAttempt.UnixMilli() != 1700000000000 {
		t.Fatalf("LastAttempt round trip = %v, want UnixMilli 1700000000000", parsed.LastAttempt)
	}
	if parsed.ConsecutiveErrs != 2 {
		t.Fatalf("ConsecutiveErrs round trip = %d, want 2", parsed.ConsecutiveErrs)
	}
	if !parsed.Established {
		t.Error("Established lost in round trip")
	}
	if !parsed.LastPulledKeys {
		t.Error("LastPulledKeys lost in round trip")
	}
	if !parsed.DrainComplete {
		t.Error("DrainComplete lost in round trip")
	}
	if !parsed.RecoveryRepullDone {
		t.Error("RecoveryRepullDone lost in round trip")
	}
}

// TestStatusKitPassMetaNextAllowedAtFloors covers the four success-path
// floors: an incomplete drain resumes on the short drain floor; a completed
// drain pulls on the fresh floor while keys haven't been established or are
// still arriving, and backs off to the steady-state floor only once
// established and quiet.
func TestStatusKitPassMetaNextAllowedAtFloors(t *testing.T) {
	last := time.UnixMilli(1700000000000)

	cases := []struct {
		name string
		meta statusKitPassMeta
		want time.Duration
	}{
		{
			name: "incomplete drain resumes on drain floor",
			meta: statusKitPassMeta{
				LastAttempt:   last,
				DrainComplete: false,
			},
			want: statusKitDrainResumeFloor,
		},
		{
			name: "complete drain, not established, stays on fresh floor",
			meta: statusKitPassMeta{
				LastAttempt:   last,
				DrainComplete: true,
				Established:   false,
			},
			want: statusKitFreshFloor,
		},
		{
			name: "complete drain, established, still pulling keys, stays on fresh floor",
			meta: statusKitPassMeta{
				LastAttempt:    last,
				DrainComplete:  true,
				Established:    true,
				LastPulledKeys: true,
			},
			want: statusKitFreshFloor,
		},
		{
			name: "complete drain, established and quiet, backs off to success floor",
			meta: statusKitPassMeta{
				LastAttempt:    last,
				DrainComplete:  true,
				Established:    true,
				LastPulledKeys: false,
			},
			want: statusKitSuccessFloor,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.meta.nextAllowedAt(0)
			want := last.Add(tc.want)
			if !got.Equal(want) {
				t.Fatalf("nextAllowedAt(0) = %v, want %v", got, want)
			}
		})
	}
}
