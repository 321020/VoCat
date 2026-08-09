package main

import (
	"context"
	"log/slog"
	"math"
	"strings"
	"time"

	"vocat/internal/store"
)

const cellularTrafficSampleInterval = 30 * time.Second

type interfaceTrafficSample struct {
	interfaceName string
	rxBytes       uint64
	txBytes       uint64
}

func collectCellularTraffic(ctx context.Context, logger *slog.Logger, database *store.Store) {
	previous := make(map[string]interfaceTrafficSample)
	var lastPrune time.Time
	collect := func() {
		now := time.Now()
		if lastPrune.IsZero() || now.Sub(lastPrune) >= 24*time.Hour {
			lastPrune = now
			if _, err := database.DeleteTrafficBefore(ctx, now.Add(-35*24*time.Hour)); err != nil && ctx.Err() == nil {
				logger.Warn("prune old cellular traffic", "error", err)
			}
		}

		configs, err := database.ListDevices(ctx)
		if err != nil {
			if ctx.Err() == nil {
				logger.Warn("list devices for cellular traffic collection", "error", err)
			}
			return
		}

		active := make(map[string]struct{}, len(configs))
		for _, config := range configs {
			interfaceName := strings.TrimSpace(config.Interface)
			if !config.NetworkEnabled || interfaceName == "" {
				delete(previous, config.ID)
				continue
			}
			active[config.ID] = struct{}{}

			rxBytes, txBytes, err := readInterfaceTrafficCounters(interfaceName)
			if err != nil {
				// Interfaces can briefly disappear while QMI reconnects. The next
				// successful read establishes a fresh baseline, so no reconnect
				// traffic is accidentally counted twice.
				delete(previous, config.ID)
				continue
			}
			rxDelta, txDelta, ok := trafficCounterDelta(previous[config.ID], interfaceName, rxBytes, txBytes)
			previous[config.ID] = interfaceTrafficSample{
				interfaceName: interfaceName,
				rxBytes:       rxBytes,
				txBytes:       txBytes,
			}
			if !ok || (rxDelta == 0 && txDelta == 0) {
				continue
			}

			for bucket, periodStart := range trafficBucketPeriods(time.Now()) {
				if err := database.AddTrafficBucket(ctx, store.TrafficBucket{
					DeviceID:    config.ID,
					Bucket:      bucket,
					PeriodStart: periodStart,
					RXBytes:     rxDelta,
					TXBytes:     txDelta,
				}); err != nil && ctx.Err() == nil {
					logger.Warn("record cellular traffic", "device", config.ID, "bucket", bucket, "error", err)
				}
			}
		}

		for deviceID := range previous {
			if _, ok := active[deviceID]; !ok {
				delete(previous, deviceID)
			}
		}
	}

	collect()
	ticker := time.NewTicker(cellularTrafficSampleInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			collect()
		}
	}
}

func trafficCounterDelta(previous interfaceTrafficSample, interfaceName string, rxBytes, txBytes uint64) (int64, int64, bool) {
	if previous.interfaceName == "" || previous.interfaceName != interfaceName || rxBytes < previous.rxBytes || txBytes < previous.txBytes {
		return 0, 0, false
	}
	rxDelta := rxBytes - previous.rxBytes
	txDelta := txBytes - previous.txBytes
	if rxDelta > math.MaxInt64 || txDelta > math.MaxInt64 {
		return 0, 0, false
	}
	return int64(rxDelta), int64(txDelta), true
}

func trafficBucketPeriods(now time.Time) map[string]time.Time {
	local := now.In(time.Local)
	year, month, day := local.Date()
	dayStart := time.Date(year, month, day, 0, 0, 0, 0, time.Local).UTC()
	return map[string]time.Time{
		"hour":  now.UTC().Truncate(time.Minute),
		"day":   now.UTC().Truncate(time.Hour),
		"week":  dayStart,
		"month": dayStart,
	}
}
