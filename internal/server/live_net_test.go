package server

import (
	"testing"
	"time"
)

// record is the pure rate/window core of the tracker; these tests drive it
// directly with synthetic cumulative counters, no interface I/O involved.
func TestLiveNetRecordFirstSampleWaits(t *testing.T) {
	tracker := newLiveNetTracker()
	now := time.Unix(1_700_000_000, 0)

	_, _, _, _, status := tracker.record("dev1", 1000, 500, now)
	if status != "waiting_sample" {
		t.Fatalf("first sample status = %q, want waiting_sample", status)
	}
}

func TestLiveNetRecordComputesRateAndMinute(t *testing.T) {
	tracker := newLiveNetTracker()
	base := time.Unix(1_700_000_000, 0)

	tracker.record("dev1", 1000, 500, base)
	rxRate, txRate, minuteRx, minuteTx, status := tracker.record("dev1", 2000, 700, base.Add(2*time.Second))

	if status != "" {
		t.Fatalf("second sample status = %q, want empty", status)
	}
	// 1000 rx bytes and 200 tx bytes over 2s.
	if rxRate != 500 {
		t.Errorf("rxRate = %v, want 500", rxRate)
	}
	if txRate != 100 {
		t.Errorf("txRate = %v, want 100", txRate)
	}
	if minuteRx != 1000 {
		t.Errorf("minuteRx = %v, want 1000", minuteRx)
	}
	if minuteTx != 200 {
		t.Errorf("minuteTx = %v, want 200", minuteTx)
	}
}

func TestLiveNetRecordUsesActualElapsed(t *testing.T) {
	tracker := newLiveNetTracker()
	base := time.Unix(1_700_000_000, 0)

	tracker.record("dev1", 0, 0, base)
	// A 4s gap (not the usual 2s tick) must divide by 4, not 2.
	rxRate, _, _, _, status := tracker.record("dev1", 400, 0, base.Add(4*time.Second))
	if status != "" {
		t.Fatalf("status = %q, want empty", status)
	}
	if rxRate != 100 {
		t.Errorf("rxRate = %v, want 100", rxRate)
	}
}

func TestLiveNetRecordCounterResetRebaselines(t *testing.T) {
	tracker := newLiveNetTracker()
	base := time.Unix(1_700_000_000, 0)

	tracker.record("dev1", 5000, 5000, base)
	tracker.record("dev1", 6000, 6000, base.Add(2*time.Second))
	// Counter drops (interface reconnected): must re-baseline, not go negative.
	_, _, _, _, status := tracker.record("dev1", 100, 100, base.Add(4*time.Second))
	if status != "waiting_sample" {
		t.Fatalf("after reset status = %q, want waiting_sample", status)
	}
}

func TestLiveNetRecordLongGapRebaselines(t *testing.T) {
	tracker := newLiveNetTracker()
	base := time.Unix(1_700_000_000, 0)

	tracker.record("dev1", 1000, 1000, base)
	// Gap beyond liveNetMaxGap (tab closed / idle): treat as fresh baseline.
	_, _, _, _, status := tracker.record("dev1", 2000, 2000, base.Add(liveNetMaxGap+time.Second))
	if status != "waiting_sample" {
		t.Fatalf("after long gap status = %q, want waiting_sample", status)
	}
}

func TestLiveNetRecordSlidesWindow(t *testing.T) {
	tracker := newLiveNetTracker()
	base := time.Unix(1_700_000_000, 0)

	// One sample every 2s, rx climbing 100 bytes each tick (50 B/s).
	tracker.record("dev1", 0, 0, base)
	var minuteRx int64
	var status string
	for i := 1; i <= 31; i++ {
		now := base.Add(time.Duration(2*i) * time.Second) // t=2s .. t=62s
		_, _, minuteRx, _, status = tracker.record("dev1", uint64(100*i), 0, now)
	}
	if status != "" {
		t.Fatalf("status = %q, want empty", status)
	}
	// At t=62s the cutoff is t=2s, so the t=0 baseline has slid out. The window
	// now spans t=2s..t=62s = 60s and 30 ticks of 100 bytes.
	if minuteRx != 3000 {
		t.Errorf("minuteRx = %v, want 3000 (only trailing window)", minuteRx)
	}
}

func TestLiveNetRecordTracksDevicesIndependently(t *testing.T) {
	tracker := newLiveNetTracker()
	base := time.Unix(1_700_000_000, 0)

	tracker.record("a", 1000, 0, base)
	tracker.record("b", 9000, 0, base)
	rxRateA, _, _, _, _ := tracker.record("a", 2000, 0, base.Add(2*time.Second))
	rxRateB, _, _, _, _ := tracker.record("b", 9100, 0, base.Add(2*time.Second))
	if rxRateA != 500 {
		t.Errorf("device a rxRate = %v, want 500", rxRateA)
	}
	if rxRateB != 50 {
		t.Errorf("device b rxRate = %v, want 50", rxRateB)
	}
}

func TestFormatLiveBytes(t *testing.T) {
	cases := []struct {
		in   float64
		want string
	}{
		{0, "0.0 B"},
		{512, "512 B"},
		{1023, "1023 B"},
		{1024, "1.0 KB"},
		{1536, "1.5 KB"},
		{100 * 1024, "100 KB"},
		{5 * 1024 * 1024, "5.0 MB"},
		{3 * 1024 * 1024 * 1024, "3.0 GB"},
		{-5, "0.0 B"},
	}
	for _, c := range cases {
		if got := formatLiveBytes(c.in); got != c.want {
			t.Errorf("formatLiveBytes(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}
