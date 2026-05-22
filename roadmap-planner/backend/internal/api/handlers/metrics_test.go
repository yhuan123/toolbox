/*
Copyright 2024 The AlaudaDevops Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package handlers

import (
	"net/http/httptest"
	"testing"
	"time"

	"github.com/AlaudaDevops/toolbox/roadmap-planner/backend/internal/config"
	"github.com/gin-gonic/gin"
)

// TestParseTimeRange_DefaultMatchesHistoricalDays guards the P2 fix:
// when `from` is omitted, the API default window must mirror
// metrics.historical_days, not a hard-coded 1 year. The collector
// loads PRs over a window sized to HistoricalDays + lookback, so a
// 1-year API default against HistoricalDays=90 silently under-reports
// Lead Time for releases 270d–365d ago whose PRs were never loaded.
func TestParseTimeRange_DefaultMatchesHistoricalDays(t *testing.T) {
	gin.SetMode(gin.TestMode)

	cases := []struct {
		name           string
		historicalDays int
		wantDays       int
	}{
		{"90-day deployment", 90, 90},
		{"default 365-day deployment", 365, 365},
		{"180-day deployment", 180, 180},
		{"zero falls back to 365", 0, 365},
		{"negative falls back to 365", -1, 365},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := &MetricsHandler{
				config: &config.Config{
					Metrics: config.Metrics{HistoricalDays: tc.historicalDays},
				},
			}
			ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
			ctx.Request = httptest.NewRequest("GET", "/api/metrics/lead_time_to_release", nil)

			before := time.Now()
			got := h.parseTimeRange(ctx)
			after := time.Now()

			// End defaults to now — between `before` and `after`.
			if got.End.Before(before) || got.End.After(after) {
				t.Errorf("End = %v, want between %v and %v", got.End, before, after)
			}
			// Start = End - HistoricalDays. Use a small tolerance for
			// the few microseconds between End and our `before` capture.
			wantStart := got.End.AddDate(0, 0, -tc.wantDays)
			delta := got.Start.Sub(wantStart)
			if delta < -time.Second || delta > time.Second {
				t.Errorf("Start = %v, want %v (HistoricalDays=%d → %d days back)",
					got.Start, wantStart, tc.historicalDays, tc.wantDays)
			}
		})
	}
}

// TestParseTimeRange_ExplicitFromOverridesHistoricalDays makes sure
// callers can still query outside HistoricalDays by passing `from`
// explicitly — the default only fires when the param is absent.
func TestParseTimeRange_ExplicitFromOverridesHistoricalDays(t *testing.T) {
	gin.SetMode(gin.TestMode)

	h := &MetricsHandler{
		config: &config.Config{Metrics: config.Metrics{HistoricalDays: 30}},
	}
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest("GET", "/x?from=2025-01-15&to=2025-06-15", nil)

	got := h.parseTimeRange(ctx)
	wantStart := time.Date(2025, 1, 15, 0, 0, 0, 0, time.UTC)
	wantEnd := time.Date(2025, 6, 15, 0, 0, 0, 0, time.UTC)
	if !got.Start.Equal(wantStart) || !got.End.Equal(wantEnd) {
		t.Errorf("explicit range = (%v, %v), want (%v, %v) — caller-supplied from/to must win over the HistoricalDays default",
			got.Start, got.End, wantStart, wantEnd)
	}
}
