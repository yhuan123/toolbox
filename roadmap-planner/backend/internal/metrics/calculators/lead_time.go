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

// Lead Time for Changes — DORA Phase 2 implementation.
//
// Algorithm reference: docs/plans/2026-05-21-dora-optimization-plan.md
// §Lead Time 3 段计算 · 详细设计 (§1–§11). All design decisions
// (D3, D14, D15, D16, D17) are honoured here.
//
//   T0 = min(p.first_commit_at)  ── earliest commit on linked PRs
//   T1 = min(p.created_at)       ── first PR opened
//   T2 = max(p.merged_at)        ── last PR merged
//   T3 = issue.fix_version.releaseDate
//
//   dev_s     = T1 − T0
//   review_s  = T2 − T1
//   release_s = T3 − T2
//   total_s   = T3 − T0
//
// Per-issue fallback (C1..C6) is documented in plan §4. Bottleneck
// rule (D15): stage_p50 / sum > 40% AND stage_p50 >= team baseline p75.
// worst_issues: each stage's top-1, three rows total. Trend (D17):
// month-bucket p50 + MoM/QoQ chips.

package calculators

import (
	"context"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/AlaudaDevops/toolbox/roadmap-planner/backend/internal/metrics/models"
)

// Stage names — kept lowercase to match API contract (plan §8).
const (
	stageDev     = "dev"
	stageReview  = "review"
	stageRelease = "release"
)

// Bottleneck rule thresholds (D15).
const (
	bottleneckShareThreshold = 0.40
)

// excludedLongDevThreshold ─ if T0 is earlier than (window.start - 180d)
// the issue is dropped (plan §9 E9). Keeps long-tail outliers from
// dragging Dev/Total p50.
const excludedLongDevThreshold = 180 * 24 * time.Hour

// stagedIssue is the per-issue computation result.
type stagedIssue struct {
	Key           string
	Issuetype     string
	Summary       string
	Component     string
	LinkedPRCount int
	IsBotMajority bool
	ReleaseDate   time.Time
	Case          string

	// Stage durations in hours. NaN means the issue is excluded from
	// that stage's distribution (typically C3 — no first_commit_at).
	DevHours     float64
	ReviewHours  float64
	ReleaseHours float64
	TotalHours   float64
}

// stageBreakdown is the aggregated per-stage view returned to the API.
type stageBreakdown struct {
	Name             string  `json:"name"`
	Label            string  `json:"label"`
	Definition       string  `json:"definition"`
	P50Hours         float64 `json:"p50_hours"`
	P75Hours         float64 `json:"p75_hours"`
	SampleCount      int     `json:"sample_count"`
	Bottleneck       bool    `json:"bottleneck"`
	BottleneckReason string  `json:"bottleneck_reason,omitempty"`
}

// worstIssue is a top-1 record for one stage. The API returns up to
// three (one per stage that has a candidate).
type worstIssue struct {
	JiraKey                 string  `json:"jira_key"`
	Issuetype               string  `json:"issuetype"`
	Summary                 string  `json:"summary"`
	BottleneckStage         string  `json:"bottleneck_stage"`
	BottleneckDurationHours float64 `json:"bottleneck_duration_hours"`
	TotalLeadTimeHours      float64 `json:"total_lead_time_hours"`
	PRCount                 int     `json:"pr_count"`
	IsBotMajority           bool    `json:"is_bot_majority"`
}

// coverageStats reports how every issue was classified by the fallback
// matrix (plan §4 C1..C6). Exposed in the API so the UI can show
// "linked-PR coverage X% · N excluded".
type coverageStats struct {
	IssuesTotal       int `json:"issues_total"`
	IssuesFull        int `json:"issues_full"`         // C1
	IssuesPartial     int `json:"issues_partial"`      // C2
	IssuesDevMissing  int `json:"issues_dev_missing"`  // C3
	IssuesNoPRs       int `json:"issues_no_prs"`       // C4 (excluded)
	IssuesUnreleased  int `json:"issues_unreleased"`   // C5 (excluded, count-only)
	IssuesDataAnomaly int `json:"issues_data_anomaly"` // C6 (excluded)
	IssuesLongDev     int `json:"issues_long_dev"`     // E9 (excluded)
	CoveragePct       int `json:"coverage_pct"`
}

// trendPoint is one month's bucket aggregate. P50Hours is *float64 so
// the API can encode "sample too small" as null (plan §11).
type trendPoint struct {
	Period      string   `json:"period"` // "2026-05"
	P50Hours    *float64 `json:"p50_hours"`
	SampleCount int      `json:"sample_count"`
	IsCurrent   bool     `json:"is_current,omitempty"`
}

// trendData is the panel payload (D17).
type trendData struct {
	Granularity  string       `json:"granularity"`
	Points       []trendPoint `json:"points"`
	MoMPct       *float64     `json:"mom_pct"`
	QoQPct       *float64     `json:"qoq_pct"`
	MoMDirection string       `json:"mom_direction"`
	QoQDirection string       `json:"qoq_direction"`
}

// percentileStats packages the common quartile view.
type percentileStats struct {
	P25Hours    float64 `json:"p25_hours"`
	P50Hours    float64 `json:"p50_hours"`
	P75Hours    float64 `json:"p75_hours"`
	P90Hours    float64 `json:"p90_hours"`
	SampleCount int     `json:"sample_count"`
}

// LeadTimeCalculator implements the DORA Lead Time for Changes metric
// with 3-stage attribution (Dev / Review / Release).
type LeadTimeCalculator struct {
	BaseCalculator
}

// NewLeadTimeCalculator wires the calculator with default options.
//
// Recognised options:
//
//	include_bots (bool, default false) — when false, bot PRs are
//	  filtered out of T0/T1/T2 computation and is_bot_majority is
//	  evaluated against the original set.
//	with_trend (bool, default true) — emit the trend block.
func NewLeadTimeCalculator(options map[string]interface{}) *LeadTimeCalculator {
	return &LeadTimeCalculator{
		BaseCalculator: NewBaseCalculator(
			"lead_time_to_release",
			"Time from first commit on a linked PR to release",
			// Unit stays "days" for backward compatibility with the
			// existing MetricCard / MetricBreakdown / Prometheus
			// consumers (DORA day thresholds, lead_time_days). The
			// hour-precision values live in MetricResult.Metadata
			// (total.p50_hours, stages[].p50_hours, trend.points[]).
			"days",
			[]string{"component"},
			options,
		),
	}
}

// Calculate runs the full 3-stage attribution + trend computation.
// Per-request flags from data.Options (include_bots, with_trend) override
// the calculator's startup defaults.
//
// When no PR storage backend is configured (PRStoreAvailable == false),
// Calculate falls back to a Jira-only calendar lead time (issue.created
// → release.releaseDate, in days) and marks the result as degraded.
// This preserves the pre-Phase-2 Lead Time signal for minimal
// deployments where storage.enabled is still false.
func (c *LeadTimeCalculator) Calculate(ctx context.Context, data *models.CalculationContext) ([]models.MetricResult, error) {
	if !data.PRStoreAvailable {
		return c.calculateJiraOnlyFallback(data), nil
	}
	includeBots := readBoolOpt(data.Options, "include_bots", c.GetBoolOption("include_bots", false))
	withTrend := readBoolOpt(data.Options, "with_trend", c.GetBoolOption("with_trend", true))

	// Indexes for the per-issue join.
	versionByName := indexReleases(data.Releases)
	prsByKey := groupPRsByJiraKey(data.PullRequests, includeBots)

	// Two passes:
	//  1) classify every issue, collect staged issues per component
	//  2) aggregate, identify bottlenecks, pick worst_issues, build trend
	componentBuckets := make(map[string][]stagedIssue)
	componentCoverage := make(map[string]*coverageStats)

	for _, iss := range mergeIssueLists(data.Epics, data.Issues) {
		// Window check is by release date, not issue.created (DORA
		// convention; plan §9 E6).
		relDate, release := matchReleasedVersion(iss.Versions, versionByName, data.TimeRange)
		if relDate.IsZero() {
			recordCoverage(componentCoverage, "", "C5")
			continue
		}
		component := release.Component
		if component == "" {
			// Collector drops unparsable releases, but guard here too: a
			// raw version name (e.g. "0.3") is not a component bucket.
			continue
		}
		if len(data.Filters.Components) > 0 && !containsString(data.Filters.Components, component) {
			continue
		}

		linked := prsByKey[iss.Key]
		linked = filterPreReleasePRs(linked, relDate)

		si, kase := classifyIssue(iss, linked, relDate, component, data.TimeRange)
		recordCoverage(componentCoverage, component, kase)
		switch kase {
		case "C1", "C2", "C3":
			componentBuckets[component] = append(componentBuckets[component], si)
		}
	}

	// Team baseline p75 per stage — drives the bottleneck rule (D15).
	teamBaseline := computeTeamBaselineP75(componentBuckets)

	results := make([]models.MetricResult, 0, len(componentBuckets))
	for component, issues := range componentBuckets {
		coverage := componentCoverage[component]
		if coverage == nil {
			coverage = &coverageStats{}
		}
		coverage.CoveragePct = computeCoveragePct(coverage)

		// Total percentiles over qualified issues.
		totals := collectTotals(issues)
		totalStats := percentileSummary(totals)

		stages := computeStageBreakdowns(issues, teamBaseline)
		worst := pickWorstPerStage(issues, stages)
		var consistencyWarning string
		if !consistencyOK(stages, totalStats) {
			consistencyWarning = fmt.Sprintf(
				"sum(stages.p50)=%.0fh vs total.p50=%.0fh — fallback exclusion exceeds 5%% drift",
				sumStageP50(stages), totalStats.P50Hours,
			)
		}

		var trend *trendData
		if withTrend {
			trend = buildTrend(issues, data.TimeRange)
		}

		// Legacy metadata keys (days) consumed by MetricBreakdown.jsx
		// and any other day-based reader. Sourced from `totals` (hours)
		// and converted; structured hour-precision data stays in
		// metadata.total / metadata.stages.
		minDays, maxDays, avgDays := legacyDaySummary(totals)

		results = append(results, models.MetricResult{
			Name: c.Name(),
			// Value is in days for the existing consumers (P1-1); the
			// hour-precision values are in Metadata.
			Value: totalStats.P50Hours / 24,
			Unit:  c.Unit(),
			Labels: map[string]string{
				"component": component,
			},
			Timestamp: time.Now(),
			Metadata: map[string]interface{}{
				// New (hours-precision) structured payload.
				"total":               totalStats,
				"stages":              stages,
				"worst_issues":        worst,
				"coverage":            coverage,
				"trend":               trend,
				"consistency_warning": stringPtrOrNil(consistencyWarning),
				"include_bots":        includeBots,
				// Legacy backward-compat (days) for MetricBreakdown.jsx
				// `min`/`max`/`count` reads. See P2 review fix.
				"min":         minDays,
				"max":         maxDays,
				"average":     avgDays,
				"count":       totalStats.SampleCount,
				"sample_size": totalStats.SampleCount,
				"percentile":  50,
			},
		})
	}

	return results, nil
}

// legacyDaySummary returns min/max/average of the totals slice
// (which is in hours) converted to days. Empty slice yields zeros.
func legacyDaySummary(totals []float64) (minDays, maxDays, avgDays float64) {
	if len(totals) == 0 {
		return 0, 0, 0
	}
	sorted := append([]float64(nil), totals...)
	sort.Float64s(sorted)
	minDays = sorted[0] / 24
	maxDays = sorted[len(sorted)-1] / 24
	var sum float64
	for _, h := range sorted {
		sum += h
	}
	avgDays = (sum / float64(len(sorted))) / 24
	return
}

// calculateJiraOnlyFallback runs the pre-Phase-2 calendar Lead Time
// (issue.created → release.releaseDate, days) when no PR store is
// available. Output is intentionally minimal: Value + the legacy
// metadata keys (min/max/average/count) so MetricBreakdown.jsx renders
// numbers instead of N/A. Stage attribution, worst_issues, trend, and
// coverage are absent and metadata.degraded is set so the UI can warn
// the operator that storage.enabled is off.
//
// This breaks D3 (commit-centric start) — but only on minimal
// deployments where PR data is not even ingested. The trade-off is
// documented in plan §10.
func (c *LeadTimeCalculator) calculateJiraOnlyFallback(data *models.CalculationContext) []models.MetricResult {
	versionByName := indexReleases(data.Releases)
	componentBuckets := make(map[string][]float64)

	for _, iss := range mergeIssueLists(data.Epics, data.Issues) {
		if iss.CreatedDate.IsZero() {
			continue
		}
		relDate, release := matchReleasedVersion(iss.Versions, versionByName, data.TimeRange)
		if relDate.IsZero() {
			continue
		}
		component := release.Component
		if component == "" {
			// Same guard as the PR-backed path: unparsable version names
			// are not component buckets.
			continue
		}
		if len(data.Filters.Components) > 0 && !containsString(data.Filters.Components, component) {
			continue
		}
		leadDays := relDate.Sub(iss.CreatedDate).Hours() / 24
		if leadDays < 0 {
			continue
		}
		componentBuckets[component] = append(componentBuckets[component], leadDays)
	}

	results := make([]models.MetricResult, 0, len(componentBuckets))
	for component, days := range componentBuckets {
		if len(days) == 0 {
			continue
		}
		sorted := append([]float64(nil), days...)
		sort.Float64s(sorted)
		var sum float64
		for _, d := range sorted {
			sum += d
		}
		avg := sum / float64(len(sorted))

		results = append(results, models.MetricResult{
			Name:  c.Name(),
			Value: percentile(sorted, 50),
			Unit:  c.Unit(),
			Labels: map[string]string{
				"component": component,
			},
			Timestamp: time.Now(),
			Metadata: map[string]interface{}{
				"min":             sorted[0],
				"max":             sorted[len(sorted)-1],
				"average":         avg,
				"count":           len(sorted),
				"sample_size":     len(sorted),
				"percentile":      50,
				"degraded":        "no_pr_store",
				"degraded_reason": "PR storage is not enabled; falling back to issue.created → release.releaseDate calendar lead time (days). Stage attribution, worst_issues, trend, and coverage are unavailable until storage.enabled = true and a PR sync has run.",
			},
		})
	}
	return results
}

// PrometheusMetrics returns the Prometheus metric descriptors.
func (c *LeadTimeCalculator) PrometheusMetrics() []models.PrometheusMetricDesc {
	return []models.PrometheusMetricDesc{
		{
			Name:       "lead_time_hours_p50",
			Help:       "Lead Time for Changes (DORA) p50, first commit → released, in hours",
			Type:       "gauge",
			LabelNames: []string{"component"},
		},
	}
}

// ─────────────────────────────────────────────────────────────────────
// Internal helpers — kept package-private; the structured payload is
// the only externally observable surface.
// ─────────────────────────────────────────────────────────────────────

func indexReleases(rs []models.EnrichedRelease) map[string]models.EnrichedRelease {
	out := make(map[string]models.EnrichedRelease, len(rs))
	for _, r := range rs {
		out[r.Name] = r
	}
	return out
}

func groupPRsByJiraKey(prs []models.EnrichedPR, includeBots bool) map[string][]models.EnrichedPR {
	out := make(map[string][]models.EnrichedPR)
	for _, p := range prs {
		if p.JiraKey == "" {
			continue
		}
		if !includeBots && p.IsBot {
			continue
		}
		out[p.JiraKey] = append(out[p.JiraKey], p)
	}
	return out
}

// mergeIssueLists yields Epics + Issues without duplicates by Key.
// Probe A confirms the bulk of fix_version-carrying issues are Story /
// Bug / Technical Debt (under `Issues`), not Epic — but we sweep both
// since the collector populates each slice from different JQL.
func mergeIssueLists(epics, issues []models.EnrichedIssue) []models.EnrichedIssue {
	seen := make(map[string]struct{}, len(epics)+len(issues))
	out := make([]models.EnrichedIssue, 0, len(epics)+len(issues))
	for _, i := range epics {
		if _, ok := seen[i.Key]; ok {
			continue
		}
		seen[i.Key] = struct{}{}
		out = append(out, i)
	}
	for _, i := range issues {
		if _, ok := seen[i.Key]; ok {
			continue
		}
		seen[i.Key] = struct{}{}
		out = append(out, i)
	}
	return out
}

// matchReleasedVersion picks the earliest released version on this
// issue whose releaseDate falls inside `window`. Returns the matching
// EnrichedRelease so callers can reuse its parsed Component field
// (set by collector via ConvertJiraVersionToVersion); zero values map
// to C5.
//
// The returned T3 is normalised to end-of-day (23:59:59.999999999).
// Jira version releaseDate only carries a calendar date, parsed at
// midnight; comparing PR merge timestamps directly would drop any PR
// merged later on the same calendar day as "after release", losing
// the last PR on release day for many shipped issues.
func matchReleasedVersion(versionNames []string, byName map[string]models.EnrichedRelease, window models.TimeRange) (time.Time, models.EnrichedRelease) {
	var bestDate time.Time
	var best models.EnrichedRelease
	for _, name := range versionNames {
		r, ok := byName[name]
		if !ok || !r.Released || r.ReleaseDate.IsZero() {
			continue
		}
		if r.ReleaseDate.Before(window.Start) || r.ReleaseDate.After(window.End) {
			continue
		}
		if bestDate.IsZero() || r.ReleaseDate.Before(bestDate) {
			bestDate = r.ReleaseDate
			best = r
		}
	}
	if !bestDate.IsZero() {
		bestDate = endOfDay(bestDate)
	}
	return bestDate, best
}

// endOfDay snaps a timestamp to 23:59:59.999999999 of the same calendar
// day in its original location. Used for Jira release date comparison;
// see matchReleasedVersion.
func endOfDay(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 23, 59, 59, 999999999, t.Location())
}

// filterPreReleasePRs drops PRs whose merged_at is *after* the release
// date — those are hotfixes against the released version and belong to
// the *next* release window (plan §9 E3).
func filterPreReleasePRs(prs []models.EnrichedPR, releaseDate time.Time) []models.EnrichedPR {
	out := prs[:0:0] // new slice, don't mutate caller
	for _, p := range prs {
		if p.MergedAt == nil || p.MergedAt.After(releaseDate) {
			continue
		}
		out = append(out, p)
	}
	return out
}

// classifyIssue runs the C1–C6 fallback matrix and produces a
// stagedIssue with stage durations populated where possible.
func classifyIssue(iss models.EnrichedIssue, linked []models.EnrichedPR, releaseDate time.Time, component string, window models.TimeRange) (stagedIssue, string) {
	si := stagedIssue{
		Key:           iss.Key,
		Issuetype:     iss.IssueType,
		Summary:       iss.Name,
		Component:     component,
		LinkedPRCount: len(linked),
		ReleaseDate:   releaseDate,
		DevHours:      math.NaN(),
		ReviewHours:   math.NaN(),
		ReleaseHours:  math.NaN(),
		TotalHours:    math.NaN(),
	}

	if len(linked) == 0 {
		return si, "C4"
	}

	// Bot-majority badge is based on the *unfiltered* set as it exists
	// after include_bots/hotfix filtering — i.e. what is being charted.
	botCount := 0
	for _, p := range linked {
		if p.IsBot {
			botCount++
		}
	}
	si.IsBotMajority = botCount*2 > len(linked)

	// T1, T2 are non-null as long as one PR is linked (created_at and
	// merged_at are always present on shipped PRs).
	var t1, t2 time.Time
	for _, p := range linked {
		if t1.IsZero() || p.CreatedAt.Before(t1) {
			t1 = p.CreatedAt
		}
		if p.MergedAt != nil && p.MergedAt.After(t2) {
			t2 = *p.MergedAt
		}
	}

	// T0 — first_commit_at. May be missing on some/all PRs.
	var (
		t0           time.Time
		anyT0        bool
		allT0        = true
		t0Source     int
	)
	for _, p := range linked {
		if p.FirstCommitAt == nil {
			allT0 = false
			continue
		}
		t0Source++
		if !anyT0 || p.FirstCommitAt.Before(t0) {
			t0 = *p.FirstCommitAt
			anyT0 = true
		}
	}

	t3 := releaseDate

	// E9 — long-dev exclusion (plan §9 E9).
	if anyT0 && t0.Before(window.Start.Add(-excludedLongDevThreshold)) {
		return si, "long_dev"
	}

	// Compute stages where data is present.
	si.ReleaseHours = t3.Sub(t2).Hours()
	si.ReviewHours = t2.Sub(t1).Hours()

	if anyT0 {
		si.DevHours = t1.Sub(t0).Hours()
		si.TotalHours = t3.Sub(t0).Hours()
	}

	// C6 — any computed stage is negative (timezone / squash / draft
	// occupancy bug). Drop completely.
	if anyT0 && si.DevHours < 0 {
		return si, "C6"
	}
	if si.ReviewHours < 0 {
		return si, "C6"
	}
	if si.ReleaseHours < 0 {
		return si, "C6"
	}

	switch {
	case !anyT0:
		// C3 — Dev stage excluded; Review's effective start becomes T1.
		si.TotalHours = t3.Sub(t1).Hours()
		return si, "C3"
	case !allT0:
		// C2 — partial first_commit_at coverage; Dev still computed
		// from the earliest available.
		return si, "C2"
	default:
		return si, "C1"
	}
}

func recordCoverage(byComp map[string]*coverageStats, component, kase string) {
	if component == "" {
		component = "_unscoped"
	}
	cov, ok := byComp[component]
	if !ok {
		cov = &coverageStats{}
		byComp[component] = cov
	}
	cov.IssuesTotal++
	switch kase {
	case "C1":
		cov.IssuesFull++
	case "C2":
		cov.IssuesPartial++
	case "C3":
		cov.IssuesDevMissing++
	case "C4":
		cov.IssuesNoPRs++
	case "C5":
		cov.IssuesUnreleased++
	case "C6":
		cov.IssuesDataAnomaly++
	case "long_dev":
		cov.IssuesLongDev++
	}
}

func computeCoveragePct(c *coverageStats) int {
	denom := c.IssuesTotal - c.IssuesUnreleased - c.IssuesDataAnomaly - c.IssuesLongDev
	if denom <= 0 {
		return 0
	}
	num := c.IssuesFull + c.IssuesPartial + c.IssuesDevMissing
	return int(math.Round(float64(num) * 100 / float64(denom)))
}

// computeTeamBaselineP75 averages the per-stage p75 across every
// component bucket. This avoids letting one component's noisy month
// throw the bottleneck rule on every other component. For the v1
// rollout the team has only 9 months of data — calibrate later.
func computeTeamBaselineP75(buckets map[string][]stagedIssue) map[string]float64 {
	out := map[string]float64{stageDev: 0, stageReview: 0, stageRelease: 0}
	if len(buckets) == 0 {
		return out
	}
	merged := map[string][]float64{stageDev: {}, stageReview: {}, stageRelease: {}}
	for _, issues := range buckets {
		for _, si := range issues {
			if !math.IsNaN(si.DevHours) {
				merged[stageDev] = append(merged[stageDev], si.DevHours)
			}
			if !math.IsNaN(si.ReviewHours) {
				merged[stageReview] = append(merged[stageReview], si.ReviewHours)
			}
			if !math.IsNaN(si.ReleaseHours) {
				merged[stageRelease] = append(merged[stageRelease], si.ReleaseHours)
			}
		}
	}
	for stage, samples := range merged {
		out[stage] = percentile(samples, 75)
	}
	return out
}

func computeStageBreakdowns(issues []stagedIssue, baselineP75 map[string]float64) []stageBreakdown {
	devSamples := make([]float64, 0, len(issues))
	revSamples := make([]float64, 0, len(issues))
	rlsSamples := make([]float64, 0, len(issues))
	for _, si := range issues {
		if !math.IsNaN(si.DevHours) {
			devSamples = append(devSamples, si.DevHours)
		}
		if !math.IsNaN(si.ReviewHours) {
			revSamples = append(revSamples, si.ReviewHours)
		}
		if !math.IsNaN(si.ReleaseHours) {
			rlsSamples = append(rlsSamples, si.ReleaseHours)
		}
	}

	dev := buildStage(stageDev, "Dev", "first_commit_at → first_pr_opened", devSamples)
	rev := buildStage(stageReview, "Review", "first_pr_opened → last_pr_merged", revSamples)
	rls := buildStage(stageRelease, "Release", "last_pr_merged → fix_version.release_date", rlsSamples)

	stages := []stageBreakdown{dev, rev, rls}
	sumP50 := dev.P50Hours + rev.P50Hours + rls.P50Hours
	if sumP50 <= 0 {
		return stages
	}
	for i := range stages {
		share := stages[i].P50Hours / sumP50
		if share <= bottleneckShareThreshold {
			continue
		}
		baseline := baselineP75[stages[i].Name]
		if baseline <= 0 || stages[i].P50Hours < baseline {
			continue
		}
		stages[i].Bottleneck = true
		stages[i].BottleneckReason = fmt.Sprintf(
			"%.0f%% of total Lead Time · >= team baseline P75 (%.0fh)",
			share*100, baseline,
		)
	}
	return stages
}

func buildStage(name, label, definition string, samples []float64) stageBreakdown {
	return stageBreakdown{
		Name:        name,
		Label:       label,
		Definition:  definition,
		P50Hours:    percentile(samples, 50),
		P75Hours:    percentile(samples, 75),
		SampleCount: len(samples),
	}
}

// pickWorstPerStage returns up to three worst_issues — one per stage —
// chosen as the issue whose *dominant* stage is this stage AND whose
// duration there is the largest. The dominant-stage filter avoids the
// "release segment always wins by absolute hours" trap (plan §7 P2-4).
func pickWorstPerStage(issues []stagedIssue, stages []stageBreakdown) []worstIssue {
	winners := map[string]stagedIssue{}
	durations := map[string]float64{}
	for _, si := range issues {
		if math.IsNaN(si.TotalHours) || si.TotalHours <= 0 {
			continue
		}
		dom := dominantStage(si)
		var d float64
		switch dom {
		case stageDev:
			d = si.DevHours
		case stageReview:
			d = si.ReviewHours
		case stageRelease:
			d = si.ReleaseHours
		default:
			continue
		}
		if math.IsNaN(d) {
			continue
		}
		if existing, ok := winners[dom]; !ok || d > durations[dom] {
			_ = existing
			winners[dom] = si
			durations[dom] = d
		}
	}
	order := []string{stageDev, stageReview, stageRelease}
	out := make([]worstIssue, 0, 3)
	for _, stage := range order {
		si, ok := winners[stage]
		if !ok {
			continue
		}
		out = append(out, worstIssue{
			JiraKey:                 si.Key,
			Issuetype:               si.Issuetype,
			Summary:                 si.Summary,
			BottleneckStage:         stage,
			BottleneckDurationHours: durations[stage],
			TotalLeadTimeHours:      si.TotalHours,
			PRCount:                 si.LinkedPRCount,
			IsBotMajority:           si.IsBotMajority,
		})
	}
	return out
}

// dominantStage returns the stage with the largest share of the issue's
// total Lead Time. Returns "" when the issue has insufficient data.
func dominantStage(si stagedIssue) string {
	if math.IsNaN(si.TotalHours) || si.TotalHours <= 0 {
		return ""
	}
	type pair struct {
		name string
		dur  float64
	}
	candidates := []pair{
		{stageDev, si.DevHours},
		{stageReview, si.ReviewHours},
		{stageRelease, si.ReleaseHours},
	}
	best := ""
	bestDur := -1.0
	for _, p := range candidates {
		if math.IsNaN(p.dur) {
			continue
		}
		if p.dur > bestDur {
			bestDur = p.dur
			best = p.name
		}
	}
	return best
}

func collectTotals(issues []stagedIssue) []float64 {
	out := make([]float64, 0, len(issues))
	for _, si := range issues {
		if !math.IsNaN(si.TotalHours) {
			out = append(out, si.TotalHours)
		}
	}
	return out
}

func percentileSummary(samples []float64) percentileStats {
	return percentileStats{
		P25Hours:    percentile(samples, 25),
		P50Hours:    percentile(samples, 50),
		P75Hours:    percentile(samples, 75),
		P90Hours:    percentile(samples, 90),
		SampleCount: len(samples),
	}
}

// percentile returns the q-th percentile (0-100) using the
// nearest-rank method. Empty slice yields 0.
func percentile(samples []float64, q int) float64 {
	if len(samples) == 0 {
		return 0
	}
	sorted := append([]float64(nil), samples...)
	sort.Float64s(sorted)
	if q <= 0 {
		return sorted[0]
	}
	if q >= 100 {
		return sorted[len(sorted)-1]
	}
	rank := math.Ceil(float64(q)*float64(len(sorted))/100) - 1
	if rank < 0 {
		rank = 0
	}
	if int(rank) >= len(sorted) {
		rank = float64(len(sorted) - 1)
	}
	return sorted[int(rank)]
}

func sumStageP50(stages []stageBreakdown) float64 {
	var sum float64
	for _, s := range stages {
		sum += s.P50Hours
	}
	return sum
}

func consistencyOK(stages []stageBreakdown, total percentileStats) bool {
	if total.P50Hours <= 0 {
		return true
	}
	sum := sumStageP50(stages)
	ratio := sum / total.P50Hours
	return ratio >= 0.95 && ratio <= 1.05
}

func stringPtrOrNil(s string) interface{} {
	if s == "" {
		return nil
	}
	return s
}

// readBoolOpt fetches a bool flag from an options map, falling back to
// the supplied default when the key is missing or has the wrong type.
func readBoolOpt(opts map[string]interface{}, key string, fallback bool) bool {
	if opts == nil {
		return fallback
	}
	v, ok := opts[key]
	if !ok {
		return fallback
	}
	if b, ok := v.(bool); ok {
		return b
	}
	return fallback
}

func containsString(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

// ─────────────────────────────────────────────────────────────────────
// Trend (D17 / §11) — month-bucket p50 + MoM/QoQ.
// ─────────────────────────────────────────────────────────────────────

const minTrendSamples = 3

func buildTrend(issues []stagedIssue, window models.TimeRange) *trendData {
	if window.Start.IsZero() || window.End.IsZero() {
		return nil
	}
	// Anchor month buckets to (window.End - N months) ... window.End, N=9.
	// We snap each end to the first of its calendar month so partial
	// windows still produce nine evenly-spaced points.
	end := monthStart(window.End)
	start := end.AddDate(0, -8, 0) // 9 buckets inclusive
	if start.Before(monthStart(window.Start)) {
		start = monthStart(window.Start)
	}

	// Bucket issues by release-date month.
	bucket := make(map[string][]float64)
	for _, si := range issues {
		if math.IsNaN(si.TotalHours) {
			continue
		}
		key := si.ReleaseDate.Format("2006-01")
		bucket[key] = append(bucket[key], si.TotalHours)
	}

	points := make([]trendPoint, 0, 9)
	currentKey := end.Format("2006-01")
	for m := start; !m.After(end); m = m.AddDate(0, 1, 0) {
		key := m.Format("2006-01")
		samples := bucket[key]
		p := trendPoint{Period: key, SampleCount: len(samples), IsCurrent: key == currentKey}
		if len(samples) >= minTrendSamples {
			v := percentile(samples, 50)
			p.P50Hours = &v
		}
		points = append(points, p)
	}

	mom, momDir := momPct(points)
	qoq, qoqDir := qoqPct(points)
	return &trendData{
		Granularity:  "month",
		Points:       points,
		MoMPct:       mom,
		QoQPct:       qoq,
		MoMDirection: momDir,
		QoQDirection: qoqDir,
	}
}

func monthStart(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, t.Location())
}

// momPct compares the last month vs the prior month. Returns nil when
// either value is missing.
func momPct(points []trendPoint) (*float64, string) {
	if len(points) < 2 {
		return nil, "n/a"
	}
	last := points[len(points)-1].P50Hours
	prev := points[len(points)-2].P50Hours
	if last == nil || prev == nil || *prev == 0 {
		return nil, "n/a"
	}
	pct := (*last - *prev) / *prev * 100
	dir := "degraded"
	if pct < 0 {
		dir = "improved"
	}
	return &pct, dir
}

// qoqPct compares the last 3 months vs the previous 3 months. Returns
// nil if any month is missing.
func qoqPct(points []trendPoint) (*float64, string) {
	if len(points) < 6 {
		return nil, "n/a"
	}
	curr := mean3(points[len(points)-3:])
	prev := mean3(points[len(points)-6 : len(points)-3])
	if curr == nil || prev == nil || *prev == 0 {
		return nil, "n/a"
	}
	pct := (*curr - *prev) / *prev * 100
	dir := "degraded"
	if pct < 0 {
		dir = "improved"
	}
	return &pct, dir
}

func mean3(window []trendPoint) *float64 {
	var sum float64
	for _, p := range window {
		if p.P50Hours == nil {
			return nil
		}
		sum += *p.P50Hours
	}
	v := sum / float64(len(window))
	return &v
}
