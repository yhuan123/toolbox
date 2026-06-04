/*
Copyright 2024 The AlaudaDevops Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
*/

package calculators

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/AlaudaDevops/toolbox/roadmap-planner/backend/internal/metrics/models"
)

// --- helpers ----------------------------------------------------------

func ptrTime(t time.Time) *time.Time { return &t }

func mustClose(t *testing.T, got, want, tol float64) {
	t.Helper()
	if math.Abs(got-want) > tol {
		t.Fatalf("got %.4f, want %.4f (±%.4f)", got, want, tol)
	}
}

// --- percentile -------------------------------------------------------

// TestPercentile_NearestRank verifies the nearest-rank percentile
// implementation across the boundary cases the calculator hits in
// practice — empty input, single sample, exact boundaries.
func TestPercentile_NearestRank(t *testing.T) {
	if got := percentile(nil, 50); got != 0 {
		t.Errorf("empty p50 = %v, want 0", got)
	}
	if got := percentile([]float64{42}, 50); got != 42 {
		t.Errorf("single p50 = %v, want 42", got)
	}
	// 10 samples 1..10 → p50 rounds up to rank 5 → value 5.
	xs := []float64{10, 9, 8, 7, 6, 5, 4, 3, 2, 1}
	if got := percentile(xs, 50); got != 5 {
		t.Errorf("p50 of 1..10 = %v, want 5", got)
	}
	if got := percentile(xs, 75); got != 8 {
		t.Errorf("p75 of 1..10 = %v, want 8", got)
	}
	if got := percentile(xs, 100); got != 10 {
		t.Errorf("p100 = %v, want 10", got)
	}
}

// --- classifyIssue fallback matrix -----------------------------------

// TestClassifyIssue_C1AllFieldsPresent exercises the happy path: a
// shipped issue with a linked PR carrying first_commit_at, created_at,
// merged_at — all three stage durations are populated and the case is
// C1.
func TestClassifyIssue_C1AllFieldsPresent(t *testing.T) {
	t0 := time.Date(2026, 4, 1, 9, 0, 0, 0, time.UTC)
	t1 := t0.Add(18 * time.Hour) // dev = 18h
	t2 := t1.Add(96 * time.Hour) // review = 96h
	t3 := t2.Add(168 * time.Hour) // release = 168h

	iss := models.EnrichedIssue{Key: "DEVOPS-1", IssueType: "Story", Versions: []string{"tektoncd-operator-v4.6.3"}}
	linked := []models.EnrichedPR{
		{ID: "AlaudaDevops/tektoncd-operator#1", JiraKey: "DEVOPS-1",
			FirstCommitAt: ptrTime(t0), CreatedAt: t1, MergedAt: ptrTime(t2)},
	}
	window := models.TimeRange{Start: t0.AddDate(0, -1, 0), End: t3.AddDate(0, 1, 0)}

	si, kase := classifyIssue(iss, linked, t3, "tektoncd-operator", window)
	if kase != "C1" {
		t.Fatalf("case = %q, want C1", kase)
	}
	mustClose(t, si.DevHours, 18, 0.01)
	mustClose(t, si.ReviewHours, 96, 0.01)
	mustClose(t, si.ReleaseHours, 168, 0.01)
	mustClose(t, si.TotalHours, 18+96+168, 0.01)
}

// TestClassifyIssue_C3DevStageMissing — every linked PR lacks
// first_commit_at. Dev stage is dropped (NaN); Review's effective
// start becomes T1; total = T3 − T1.
func TestClassifyIssue_C3DevStageMissing(t *testing.T) {
	t1 := time.Date(2026, 4, 1, 9, 0, 0, 0, time.UTC)
	t2 := t1.Add(96 * time.Hour)
	t3 := t2.Add(168 * time.Hour)

	iss := models.EnrichedIssue{Key: "DEVOPS-2", IssueType: "Bug", Versions: []string{"tektoncd-operator-v4.6.3"}}
	linked := []models.EnrichedPR{
		{ID: "x#1", JiraKey: "DEVOPS-2", FirstCommitAt: nil, CreatedAt: t1, MergedAt: ptrTime(t2)},
	}
	window := models.TimeRange{Start: t1.AddDate(0, -1, 0), End: t3.AddDate(0, 1, 0)}

	si, kase := classifyIssue(iss, linked, t3, "c", window)
	if kase != "C3" {
		t.Fatalf("case = %q, want C3", kase)
	}
	if !math.IsNaN(si.DevHours) {
		t.Errorf("DevHours = %v, want NaN", si.DevHours)
	}
	mustClose(t, si.ReviewHours, 96, 0.01)
	mustClose(t, si.ReleaseHours, 168, 0.01)
	mustClose(t, si.TotalHours, 96+168, 0.01)
}

// TestClassifyIssue_C4NoLinkedPRs — no PRs linked to this issue.
// Should be excluded entirely (returns C4 with NaN stages).
func TestClassifyIssue_C4NoLinkedPRs(t *testing.T) {
	t3 := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	iss := models.EnrichedIssue{Key: "DEVOPS-3"}
	window := models.TimeRange{Start: t3.AddDate(0, -3, 0), End: t3.AddDate(0, 1, 0)}
	_, kase := classifyIssue(iss, nil, t3, "c", window)
	if kase != "C4" {
		t.Fatalf("case = %q, want C4", kase)
	}
}

// TestClassifyIssue_E9LongDev — first_commit_at is 200 days before the
// window start. The issue should be dropped (excluded_long_dev) to
// avoid dragging the Dev/Total p50.
func TestClassifyIssue_E9LongDev(t *testing.T) {
	t3 := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	t0 := time.Date(2025, 5, 1, 0, 0, 0, 0, time.UTC) // 365d before t3
	t1 := t3.Add(-48 * time.Hour)
	t2 := t3.Add(-24 * time.Hour)

	iss := models.EnrichedIssue{Key: "DEVOPS-4"}
	linked := []models.EnrichedPR{{ID: "x#1", JiraKey: "DEVOPS-4",
		FirstCommitAt: ptrTime(t0), CreatedAt: t1, MergedAt: ptrTime(t2)}}
	// Window starts 30 days before release; t0 is well outside the
	// 180-day soft cap.
	window := models.TimeRange{Start: t3.AddDate(0, 0, -30), End: t3}
	_, kase := classifyIssue(iss, linked, t3, "c", window)
	if kase != "long_dev" {
		t.Fatalf("case = %q, want long_dev", kase)
	}
}

// --- matchReleasedVersion --------------------------------------------

func TestMatchReleasedVersion_PicksEarliestInWindow(t *testing.T) {
	a := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC) // in window — earlier
	b := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC) // in window — later
	c := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC) // outside window
	releases := map[string]models.EnrichedRelease{
		"tektoncd-operator-v4.5.0": {Name: "tektoncd-operator-v4.5.0", Component: "tektoncd-operator", Released: true, ReleaseDate: a},
		"tektoncd-operator-v4.6.0": {Name: "tektoncd-operator-v4.6.0", Component: "tektoncd-operator", Released: true, ReleaseDate: b},
		"tektoncd-operator-v4.7.0": {Name: "tektoncd-operator-v4.7.0", Component: "tektoncd-operator", Released: true, ReleaseDate: c},
	}
	window := models.TimeRange{
		Start: time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC),
		End:   time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC),
	}
	names := []string{"tektoncd-operator-v4.7.0", "tektoncd-operator-v4.6.0", "tektoncd-operator-v4.5.0"}
	gotDate, gotRelease := matchReleasedVersion(names, releases, window)
	// gotDate is normalised to end-of-day (P1-2); compare calendar day.
	if gotDate.Year() != a.Year() || gotDate.Month() != a.Month() || gotDate.Day() != a.Day() {
		t.Errorf("date = %v, want same day as %v", gotDate, a)
	}
	if gotRelease.Name != "tektoncd-operator-v4.5.0" {
		t.Errorf("name = %q, want tektoncd-operator-v4.5.0", gotRelease.Name)
	}
	if gotRelease.Component != "tektoncd-operator" {
		t.Errorf("component = %q, want tektoncd-operator", gotRelease.Component)
	}
}

// TestMatchReleasedVersion_NormalisesToEndOfDay verifies T3 is snapped
// to 23:59:59.999999999, so a PR merged later on release day is still
// inside the release window (and not treated as a hotfix).
func TestMatchReleasedVersion_NormalisesToEndOfDay(t *testing.T) {
	midnight := time.Date(2026, 4, 10, 0, 0, 0, 0, time.UTC)
	releases := map[string]models.EnrichedRelease{
		"tektoncd-operator-v4.6.3": {Name: "tektoncd-operator-v4.6.3", Component: "tektoncd-operator", Released: true, ReleaseDate: midnight},
	}
	window := models.TimeRange{Start: midnight.AddDate(0, -1, 0), End: midnight.AddDate(0, 1, 0)}

	gotDate, _ := matchReleasedVersion([]string{"tektoncd-operator-v4.6.3"}, releases, window)
	if gotDate.Hour() != 23 || gotDate.Minute() != 59 || gotDate.Second() != 59 {
		t.Errorf("releaseDate not normalised to end-of-day: %s", gotDate)
	}
	if gotDate.Day() != midnight.Day() {
		t.Errorf("calendar day shifted: got %s, want same day as %s", gotDate, midnight)
	}

	// A PR merged at 14:00 on the same day must survive the hotfix filter.
	pr := models.EnrichedPR{
		ID:        "p1",
		JiraKey:   "X-1",
		CreatedAt: midnight.Add(-72 * time.Hour),
		MergedAt:  ptrTime(midnight.Add(14 * time.Hour)),
	}
	if kept := filterPreReleasePRs([]models.EnrichedPR{pr}, gotDate); len(kept) != 1 {
		t.Fatalf("same-day PR dropped by filterPreReleasePRs: kept %d, want 1", len(kept))
	}
}

// TestMatchReleasedVersion_HandlesNoVPrefix verifies the calculator
// reuses the collector-parsed Component field, so version names that
// do not match a `{component}-vX.Y.Z` shape (e.g. `argo-cd-2.9.0`)
// still bucket correctly under the configured component, matching
// how release_frequency and other metrics group their results.
func TestMatchReleasedVersion_HandlesNoVPrefix(t *testing.T) {
	d := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	releases := map[string]models.EnrichedRelease{
		"argo-cd-2.9.0": {Name: "argo-cd-2.9.0", Component: "argo-cd", Released: true, ReleaseDate: d},
	}
	window := models.TimeRange{
		Start: time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC),
		End:   time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC),
	}
	_, got := matchReleasedVersion([]string{"argo-cd-2.9.0"}, releases, window)
	if got.Component != "argo-cd" {
		t.Errorf("component = %q, want argo-cd (collector-parsed, not regex-derived)", got.Component)
	}
}

// --- pickWorstPerStage -----------------------------------------------

// TestPickWorstPerStage_OnePerStage verifies that the function picks
// one issue per stage where that stage is the issue's *dominant*
// contributor — preventing the "release segment always wins on
// absolute hours" trap (plan §7 P2-4).
func TestPickWorstPerStage_OnePerStage(t *testing.T) {
	issues := []stagedIssue{
		// Dev-dominant
		{Key: "A", Summary: "dev hog", DevHours: 150, ReviewHours: 10, ReleaseHours: 20, TotalHours: 180},
		// Review-dominant
		{Key: "B", Summary: "rev hog", DevHours: 10, ReviewHours: 200, ReleaseHours: 30, TotalHours: 240},
		// Release-dominant + absolute biggest
		{Key: "C", Summary: "rls hog", DevHours: 5, ReviewHours: 10, ReleaseHours: 900, TotalHours: 915},
		// Smaller Dev-dominant — should NOT displace A
		{Key: "D", Summary: "small dev", DevHours: 100, ReviewHours: 1, ReleaseHours: 1, TotalHours: 102},
	}
	worst := pickWorstPerStage(issues, nil)
	if len(worst) != 3 {
		t.Fatalf("len(worst) = %d, want 3 (one per stage)", len(worst))
	}
	got := map[string]string{}
	for _, w := range worst {
		got[w.BottleneckStage] = w.JiraKey
	}
	if got[stageDev] != "A" {
		t.Errorf("dev winner = %q, want A", got[stageDev])
	}
	if got[stageReview] != "B" {
		t.Errorf("review winner = %q, want B", got[stageReview])
	}
	if got[stageRelease] != "C" {
		t.Errorf("release winner = %q, want C", got[stageRelease])
	}
}

// --- bottleneck rule -------------------------------------------------

// TestStageBreakdown_BottleneckRequiresBothConditions exercises D15:
// stage is flagged bottleneck iff p50 > 40% of total AND p50 >= team
// baseline p75. One condition alone must not trigger.
func TestStageBreakdown_BottleneckRequiresBothConditions(t *testing.T) {
	// Issues where Review absolutely dominates and is way above team baseline.
	issues := []stagedIssue{
		{DevHours: 10, ReviewHours: 200, ReleaseHours: 40, TotalHours: 250},
		{DevHours: 12, ReviewHours: 240, ReleaseHours: 30, TotalHours: 282},
		{DevHours: 8, ReviewHours: 180, ReleaseHours: 20, TotalHours: 208},
	}
	// Baseline p75 set so Review (>=190) trips it but Dev/Release do not.
	baseline := map[string]float64{stageDev: 100, stageReview: 100, stageRelease: 100}

	stages := computeStageBreakdowns(issues, baseline)

	var rev, dev, rls stageBreakdown
	for _, s := range stages {
		switch s.Name {
		case stageReview:
			rev = s
		case stageDev:
			dev = s
		case stageRelease:
			rls = s
		}
	}
	if !rev.Bottleneck {
		t.Errorf("Review should be a bottleneck (share > 40%% and >= P75): %+v", rev)
	}
	if dev.Bottleneck {
		t.Errorf("Dev should not be a bottleneck (share < 40%%): %+v", dev)
	}
	if rls.Bottleneck {
		t.Errorf("Release should not be a bottleneck (share < 40%%): %+v", rls)
	}
}

// --- trend MoM / QoQ --------------------------------------------------

// TestTrend_MoMDirection — building a trend over 9 months with the
// current month showing improvement (lower Lead Time) and the quarter
// showing degradation. Verifies the +/- sign mapping is right and the
// "improved/degraded" direction string matches.
func TestTrend_MoMDirection(t *testing.T) {
	end := time.Date(2026, 5, 21, 0, 0, 0, 0, time.UTC)
	window := models.TimeRange{Start: end.AddDate(0, -9, 0), End: end}

	// Pre-build the per-month synthetic issues using one issue per
	// month (sample == 1) → these are below minTrendSamples=3 so the
	// point would be null. Use three issues per month instead.
	hoursByMonth := map[string]float64{
		"2025-09": 576, "2025-10": 504, "2025-11": 624,
		"2025-12": 456, "2026-01": 420, "2026-02": 380,
		"2026-03": 528, "2026-04": 456, "2026-05": 432, // MoM ≈ -5.3%, QoQ degraded
	}
	var issues []stagedIssue
	for monthKey, h := range hoursByMonth {
		m, _ := time.Parse("2006-01", monthKey)
		// Three samples per month so trend bucket meets minTrendSamples.
		for i := 0; i < 3; i++ {
			issues = append(issues, stagedIssue{
				Key: monthKey + "-" + string(rune('a'+i)),
				ReleaseDate: m.AddDate(0, 0, 5),
				TotalHours:  h,
			})
		}
	}

	tr := buildTrend(issues, window)
	if tr == nil {
		t.Fatal("buildTrend returned nil")
	}
	if len(tr.Points) != 9 {
		t.Fatalf("len(points) = %d, want 9", len(tr.Points))
	}
	if tr.MoMPct == nil {
		t.Fatal("MoMPct is nil")
	}
	// MoM = (432 - 456) / 456 * 100 ≈ -5.26
	if math.Abs(*tr.MoMPct - -5.263158) > 0.01 {
		t.Errorf("MoMPct = %.4f, want ≈ -5.26", *tr.MoMPct)
	}
	if tr.MoMDirection != "improved" {
		t.Errorf("MoM direction = %q, want improved", tr.MoMDirection)
	}
	// QoQ: current 3 (528, 456, 432) avg = 472; prev 3 (456, 420, 380) avg = 418.667
	// pct = (472 - 418.667) / 418.667 * 100 ≈ 12.7
	if tr.QoQPct == nil {
		t.Fatal("QoQPct is nil")
	}
	if math.Abs(*tr.QoQPct - 12.738854) > 0.05 {
		t.Errorf("QoQPct = %.4f, want ≈ 12.74", *tr.QoQPct)
	}
	if tr.QoQDirection != "degraded" {
		t.Errorf("QoQ direction = %q, want degraded", tr.QoQDirection)
	}
}

// TestCalculate_FallbackWhenNoPRStore exercises the path that fires
// when storage.enabled is false: PRStoreAvailable=false makes
// Calculate return the legacy Jira-only days output, with the degraded
// flag set so the UI can warn. Prevents the regression where Lead Time
// would otherwise disappear on minimal deployments.
func TestCalculate_FallbackWhenNoPRStore(t *testing.T) {
	c := NewLeadTimeCalculator(nil)

	relDate := time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC)
	window := models.TimeRange{
		Start: relDate.AddDate(0, -9, 0),
		End:   relDate.AddDate(0, 1, 0),
	}
	releases := []models.EnrichedRelease{
		{ID: "v1", Name: "argo-cd-2.9.0", Component: "argo-cd", Released: true, ReleaseDate: relDate},
	}
	issues := []models.EnrichedIssue{
		{Key: "DEVOPS-1", Name: "one", IssueType: "Story",
			Versions:    []string{"argo-cd-2.9.0"},
			CreatedDate: relDate.AddDate(0, 0, -10), ReleaseDate: relDate},
		{Key: "DEVOPS-2", Name: "two", IssueType: "Bug",
			Versions:    []string{"argo-cd-2.9.0"},
			CreatedDate: relDate.AddDate(0, 0, -30), ReleaseDate: relDate},
	}

	ctx := &models.CalculationContext{
		Releases: releases, Issues: issues,
		PullRequests:     nil,
		PRStoreAvailable: false, // ← storage.enabled = false scenario
		TimeRange:        window,
	}
	results, err := c.Calculate(context.Background(), ctx)
	if err != nil {
		t.Fatalf("Calculate: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("len(results) = %d, want 1 (Lead Time must NOT disappear when PR store is unavailable)", len(results))
	}
	r := results[0]
	if r.Unit != "days" {
		t.Errorf("fallback Unit = %q, want days", r.Unit)
	}
	if r.Value <= 0 {
		t.Errorf("fallback Value = %v, want > 0 (median of {10, 30} days)", r.Value)
	}
	if got := r.Metadata["degraded"]; got != "no_pr_store" {
		t.Errorf("metadata.degraded = %v, want \"no_pr_store\" so UI knows to warn", got)
	}
	if _, ok := r.Metadata["degraded_reason"].(string); !ok {
		t.Error("metadata.degraded_reason missing — operator needs the failure mode spelled out")
	}
	// Stage / trend / coverage are intentionally absent in fallback.
	for _, missing := range []string{"stages", "worst_issues", "trend", "coverage"} {
		if _, present := r.Metadata[missing]; present {
			t.Errorf("metadata.%s leaked into fallback path — fallback must stay minimal", missing)
		}
	}
}

// TestCalculate_SkipsEmptyComponentReleases locks in the calculator-side
// guard for the invalid-component fix: a release whose name did not
// parse (Component == "", e.g. legacy "0.3") must not produce a metric
// bucket — neither under its raw name nor under "" — on both the
// PR-backed and the Jira-only fallback paths. The collector already
// drops such releases; this guards the in-calculator defense line
// against a silent revert.
func TestCalculate_SkipsEmptyComponentReleases(t *testing.T) {
	relDate := time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC)
	window := models.TimeRange{
		Start: relDate.AddDate(0, -9, 0),
		End:   relDate.AddDate(0, 1, 0),
	}
	releases := []models.EnrichedRelease{
		{ID: "v1", Name: "argo-cd-2.9.0", Component: "argo-cd", Released: true, ReleaseDate: relDate},
		{ID: "v2", Name: "0.3", Component: "", Released: true, ReleaseDate: relDate},
	}
	issues := []models.EnrichedIssue{
		{Key: "DEVOPS-1", Name: "valid", IssueType: "Story",
			Versions:    []string{"argo-cd-2.9.0"},
			CreatedDate: relDate.AddDate(0, 0, -10), ReleaseDate: relDate},
		{Key: "DEVOPS-2", Name: "legacy", IssueType: "Bug",
			Versions:    []string{"0.3"},
			CreatedDate: relDate.AddDate(0, 0, -30), ReleaseDate: relDate},
	}

	for _, tc := range []struct {
		name             string
		prStoreAvailable bool
	}{
		{"PR-backed path", true},
		{"Jira-only fallback path", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := NewLeadTimeCalculator(map[string]interface{}{"with_trend": false})
			ctx := &models.CalculationContext{
				Releases: releases, Issues: issues,
				PRStoreAvailable: tc.prStoreAvailable,
				TimeRange:        window,
			}
			results, err := c.Calculate(context.Background(), ctx)
			if err != nil {
				t.Fatalf("Calculate: %v", err)
			}
			for _, r := range results {
				switch r.Labels["component"] {
				case "argo-cd": // the valid control bucket
				case "", "0.3":
					t.Errorf("empty-component release leaked into bucket %q", r.Labels["component"])
				default:
					t.Errorf("unexpected component bucket %q", r.Labels["component"])
				}
			}
		})
	}
}

// --- end-to-end Calculate sanity check --------------------------------

// TestCalculate_EndToEnd builds a tiny CalculationContext with two
// issues across one component and verifies Calculate produces a sane
// MetricResult: stages populated, coverage filled, trend present.
func TestCalculate_EndToEnd(t *testing.T) {
	c := NewLeadTimeCalculator(map[string]interface{}{
		"include_bots": false,
		"with_trend":   true,
	})

	relDate := time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC)
	window := models.TimeRange{
		Start: relDate.AddDate(0, -9, 0),
		End:   relDate.AddDate(0, 1, 0),
	}

	releases := []models.EnrichedRelease{
		{ID: "v1", Name: "tektoncd-operator-v4.6.3", Component: "tektoncd-operator", Released: true, ReleaseDate: relDate},
	}
	issues := []models.EnrichedIssue{
		{Key: "DEVOPS-1", Name: "thing one", IssueType: "Story",
			Versions:    []string{"tektoncd-operator-v4.6.3"},
			CreatedDate: relDate.AddDate(0, 0, -30), ReleaseDate: relDate},
		{Key: "DEVOPS-2", Name: "thing two", IssueType: "Bug",
			Versions:    []string{"tektoncd-operator-v4.6.3"},
			CreatedDate: relDate.AddDate(0, 0, -20), ReleaseDate: relDate},
	}
	prs := []models.EnrichedPR{
		// DEVOPS-1: C1 (full data)
		{ID: "p1", Source: "github", JiraKey: "DEVOPS-1",
			AuthorLogin: "alice", IsBot: false,
			FirstCommitAt: ptrTime(relDate.AddDate(0, 0, -10)),
			CreatedAt:     relDate.AddDate(0, 0, -8),
			MergedAt:      ptrTime(relDate.AddDate(0, 0, -3))},
		// DEVOPS-2: C3 (no first_commit_at)
		{ID: "p2", Source: "github", JiraKey: "DEVOPS-2",
			AuthorLogin: "bob",
			FirstCommitAt: nil,
			CreatedAt:     relDate.AddDate(0, 0, -5),
			MergedAt:      ptrTime(relDate.AddDate(0, 0, -1))},
	}

	ctx := &models.CalculationContext{
		Releases: releases, Issues: issues, PullRequests: prs,
		PRStoreAvailable: true,
		TimeRange:        window,
	}
	results, err := c.Calculate(context.Background(), ctx)
	if err != nil {
		t.Fatalf("Calculate: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("len(results) = %d, want 1", len(results))
	}
	r := results[0]
	if r.Labels["component"] != "tektoncd-operator" {
		t.Errorf("component label = %q, want tektoncd-operator", r.Labels["component"])
	}
	if r.Unit != "days" {
		t.Errorf("Unit = %q, want days (backward-compat with MetricCard / Prometheus)", r.Unit)
	}
	if r.Value <= 0 {
		t.Errorf("Value = %v, want > 0", r.Value)
	}
	// Sanity check that Value is days, not hours: a 30-day window
	// here can never produce > 60 days of Lead Time.
	if r.Value > 60 {
		t.Errorf("Value = %v days, suspiciously high — is Value still hours?", r.Value)
	}

	cov, ok := r.Metadata["coverage"].(*coverageStats)
	if !ok {
		t.Fatalf("coverage missing or wrong type: %T", r.Metadata["coverage"])
	}
	if cov.IssuesFull != 1 || cov.IssuesDevMissing != 1 {
		t.Errorf("coverage = %+v, want IssuesFull=1 IssuesDevMissing=1", cov)
	}

	// MetricBreakdown.jsx reads legacy days fields directly off the
	// Metadata map. P2 review fix — make sure they are populated so
	// the UI does not show Min=0 / Max=0 / Count=0.
	for _, key := range []string{"min", "max", "average"} {
		v, ok := r.Metadata[key].(float64)
		if !ok || v <= 0 {
			t.Errorf("metadata[%q] = %v (%T), want positive days value", key, r.Metadata[key], r.Metadata[key])
		}
	}
	for _, key := range []string{"count", "sample_size"} {
		v, ok := r.Metadata[key].(int)
		if !ok || v <= 0 {
			t.Errorf("metadata[%q] = %v (%T), want positive int sample count", key, r.Metadata[key], r.Metadata[key])
		}
	}
}
