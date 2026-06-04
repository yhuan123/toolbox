/*
Copyright 2024 The AlaudaDevops Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package calculators

import (
	"context"
	"testing"
	"time"

	"github.com/AlaudaDevops/toolbox/roadmap-planner/backend/internal/metrics/models"
)

// TestReleaseFrequency_SkipsEmptyComponentReleases locks in the
// invalid-component guard: a release whose name did not parse
// (Component == "") used to be bucketed under "unknown"; it must now be
// skipped entirely so legacy version names like "0.3" never surface as
// component buckets.
func TestReleaseFrequency_SkipsEmptyComponentReleases(t *testing.T) {
	c := NewReleaseFrequencyCalculator(nil)

	relDate := time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC)
	ctx := &models.CalculationContext{
		Releases: []models.EnrichedRelease{
			{ID: "v1", Name: "argo-cd-2.9.0", Component: "argo-cd", Released: true, ReleaseDate: relDate, Type: "minor"},
			{ID: "v2", Name: "0.3", Component: "", Released: true, ReleaseDate: relDate, Type: "unknown"},
		},
		TimeRange: models.TimeRange{
			Start: relDate.AddDate(0, -3, 0),
			End:   relDate.AddDate(0, 1, 0),
		},
	}

	results, err := c.Calculate(context.Background(), ctx)
	if err != nil {
		t.Fatalf("Calculate: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("len(results) = %d, want 1 (only the valid component)", len(results))
	}
	if got := results[0].Labels["component"]; got != "argo-cd" {
		t.Errorf("component = %q, want argo-cd (empty-component release must not bucket as %q)", got, got)
	}
}
