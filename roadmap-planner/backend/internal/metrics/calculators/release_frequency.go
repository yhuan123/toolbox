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

// ReleaseFrequencyCalculator calculates how often releases are published
type ReleaseFrequencyCalculator struct {
	BaseCalculator
}

// NewReleaseFrequencyCalculator creates a new release frequency calculator
func NewReleaseFrequencyCalculator(options map[string]interface{}) *ReleaseFrequencyCalculator {
	return &ReleaseFrequencyCalculator{
		BaseCalculator: NewBaseCalculator(
			"release_frequency",
			"How often releases are published per component",
			"releases/month",
			[]string{"component", "pillar", "quarter"},
			options,
		),
	}
}

// Calculate computes the release frequency metric
func (c *ReleaseFrequencyCalculator) Calculate(ctx context.Context, data *models.CalculationContext) ([]models.MetricResult, error) {
	windowMonths := c.GetIntOption("window_months", 3)

	// Group releases by component
	componentReleases := make(map[string][]models.EnrichedRelease)
	for _, release := range data.Releases {
		// Only count released versions
		if !release.Released {
			continue
		}

		// Apply time range filter
		if !release.ReleaseDate.IsZero() {
			if release.ReleaseDate.Before(data.TimeRange.Start) || release.ReleaseDate.After(data.TimeRange.End) {
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

		componentReleases[component] = append(componentReleases[component], release)
	}

	results := make([]models.MetricResult, 0, len(componentReleases))

	// Calculate frequency per component
	for component, releases := range componentReleases {
		// Count releases in time window
		windowStart := data.TimeRange.End.AddDate(0, -windowMonths, 0)
		count := 0
		majorCount, minorCount, patchCount := 0, 0, 0

		for _, r := range releases {
			if r.ReleaseDate.IsZero() {
				continue
			}
			if r.ReleaseDate.After(windowStart) && !r.ReleaseDate.After(data.TimeRange.End) {
				count++
				switch r.Type {
				case "major":
					majorCount++
				case "minor":
					minorCount++
				case "patch":
					patchCount++
				}
			}
		}

		// Calculate frequency (releases per month)
		frequency := float64(count) / float64(windowMonths)

		results = append(results, models.MetricResult{
			Name:  c.Name(),
			Value: frequency,
			Unit:  c.Unit(),
			Labels: map[string]string{
				"component": component,
			},
			Timestamp: time.Now(),
			Metadata: map[string]interface{}{
				"total_releases": count,
				"major_releases": majorCount,
				"minor_releases": minorCount,
				"patch_releases": patchCount,
				"window_months":  windowMonths,
			},
		})
	}

	return results, nil
}

// PrometheusMetrics returns the Prometheus metric descriptors
func (c *ReleaseFrequencyCalculator) PrometheusMetrics() []models.PrometheusMetricDesc {
	return []models.PrometheusMetricDesc{
		{
			Name:       "release_frequency",
			Help:       "Number of releases per month",
			Type:       "gauge",
			LabelNames: []string{"component", "pillar", "release_type"},
		},
	}
}
