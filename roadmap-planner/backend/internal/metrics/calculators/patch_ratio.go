/*
Copyright 2024 The AlaudaDevops Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package calculators

import (
	"context"
	"time"

	"github.com/AlaudaDevops/toolbox/roadmap-planner/backend/internal/metrics/models"
)

// PatchRatioCalculator calculates the ratio of patch releases to total releases
type PatchRatioCalculator struct {
	BaseCalculator
}

// NewPatchRatioCalculator creates a new patch ratio calculator
func NewPatchRatioCalculator(options map[string]interface{}) *PatchRatioCalculator {
	return &PatchRatioCalculator{
		BaseCalculator: NewBaseCalculator(
			"patch_ratio",
			"Ratio of patch releases to total releases",
			"ratio",
			[]string{"component", "pillar"},
			options,
		),
	}
}

// Calculate computes the patch ratio metric
func (c *PatchRatioCalculator) Calculate(ctx context.Context, data *models.CalculationContext) ([]models.MetricResult, error) {
	windowMonths := c.GetIntOption("window_months", 6)

	// Group releases by component
	componentStats := make(map[string]struct {
		total   int
		patches int
		majors  int
		minors  int
	})

	windowStart := data.TimeRange.End.AddDate(0, -windowMonths, 0)

	for _, release := range data.Releases {
		// Only count released versions
		if !release.Released {
			continue
		}

		// Apply time range filter
		if !release.ReleaseDate.IsZero() {
			if release.ReleaseDate.Before(windowStart) || release.ReleaseDate.After(data.TimeRange.End) {
				continue
			}
		}

		component := release.Component
		if component == "" {
			// Collector drops these, but guard here too: an unparsable
			// version name is not a component bucket.
			continue
		}

		// Apply component filter if specified
		if len(data.Filters.Components) > 0 && !Contains(data.Filters.Components, component) {
			continue
		}

		stats := componentStats[component]
		stats.total++

		switch release.Type {
		case "patch":
			stats.patches++
		case "major":
			stats.majors++
		case "minor":
			stats.minors++
		}

		componentStats[component] = stats
	}

	results := make([]models.MetricResult, 0, len(componentStats))

	for component, stats := range componentStats {
		if stats.total == 0 {
			continue
		}

		ratio := float64(stats.patches) / float64(stats.total)

		results = append(results, models.MetricResult{
			Name:  c.Name(),
			Value: ratio,
			Unit:  c.Unit(),
			Labels: map[string]string{
				"component": component,
			},
			Timestamp: time.Now(),
			Metadata: map[string]interface{}{
				"total_releases": stats.total,
				"patch_releases": stats.patches,
				"major_releases": stats.majors,
				"minor_releases": stats.minors,
				"window_months":  windowMonths,
			},
		})
	}

	return results, nil
}

// PrometheusMetrics returns the Prometheus metric descriptors
func (c *PatchRatioCalculator) PrometheusMetrics() []models.PrometheusMetricDesc {
	return []models.PrometheusMetricDesc{
		{
			Name:       "patch_ratio",
			Help:       "Ratio of patch releases to total releases",
			Type:       "gauge",
			LabelNames: []string{"component", "pillar"},
		},
	}
}
