package main

import (
	"testing"
	"time"
)

func TestTrafficCounterDelta(t *testing.T) {
	previous := interfaceTrafficSample{interfaceName: "wwan0", rxBytes: 100, txBytes: 50}
	rx, tx, ok := trafficCounterDelta(previous, "wwan0", 175, 90)
	if !ok || rx != 75 || tx != 40 {
		t.Fatalf("delta = (%d, %d, %v), want (75, 40, true)", rx, tx, ok)
	}
	if _, _, ok := trafficCounterDelta(previous, "wwan1", 175, 90); ok {
		t.Fatal("interface change must establish a new baseline")
	}
	if _, _, ok := trafficCounterDelta(previous, "wwan0", 90, 40); ok {
		t.Fatal("counter reset must establish a new baseline")
	}
}

func TestTrafficBucketPeriods(t *testing.T) {
	now := time.Date(2026, 8, 10, 12, 34, 56, 0, time.Local)
	periods := trafficBucketPeriods(now)
	if got := periods["hour"]; !got.Equal(now.UTC().Truncate(time.Minute)) {
		t.Fatalf("hour period = %s", got)
	}
	if got := periods["day"]; !got.Equal(now.UTC().Truncate(time.Hour)) {
		t.Fatalf("day period = %s", got)
	}
	localDay := periods["week"].In(time.Local)
	if localDay.Hour() != 0 || localDay.Minute() != 0 || localDay.Day() != 10 {
		t.Fatalf("week period = %s, want local day start", periods["week"])
	}
	if !periods["month"].Equal(periods["week"]) {
		t.Fatal("week and month should share daily periods")
	}
}
