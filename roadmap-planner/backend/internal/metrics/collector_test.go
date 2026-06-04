/*
Copyright 2024 The AlaudaDevops Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package metrics

import (
	"context"
	"database/sql"
	"reflect"
	"testing"
	"time"

	"github.com/AlaudaDevops/toolbox/roadmap-planner/backend/internal/config"
	"github.com/AlaudaDevops/toolbox/roadmap-planner/backend/internal/metrics/models"
	"github.com/AlaudaDevops/toolbox/roadmap-planner/backend/internal/storage"
	"go.uber.org/zap"
)

// prStoreStub satisfies storage.Store for the GetData truth-table test.
// Only ListPullRequestsSince is meaningful; the rest panic so any
// accidental dependency on a real store surfaces immediately instead of
// returning silent zero values.
type prStoreStub struct{}

func (prStoreStub) ListPullRequestsSince(context.Context, time.Time) ([]storage.PullRequest, error) {
	return nil, nil
}
func (prStoreStub) ListMergedPRsMissingFirstCommit(context.Context, string, time.Time, int) ([]storage.PullRequest, error) {
	panic("not used")
}
func (prStoreStub) UpdatePRFirstCommitAt(context.Context, string, *time.Time) error {
	panic("not used")
}
func (prStoreStub) Close() error                                                    { panic("not used") }
func (prStoreStub) Migrate(context.Context) error                                   { panic("not used") }
func (prStoreStub) DB() *sql.DB                                                     { panic("not used") }
func (prStoreStub) WriteCollectionRun(context.Context, storage.CollectionRun) error { panic("not used") }
func (prStoreStub) LatestCollectionRun(context.Context, string) (*storage.CollectionRun, error) {
	panic("not used")
}
func (prStoreStub) WriteIssueSnapshots(context.Context, string, []storage.IssueSnapshot) error {
	panic("not used")
}
func (prStoreStub) UpsertPullRequests(context.Context, []storage.PullRequest) error {
	panic("not used")
}
func (prStoreStub) UpsertPRReviews(context.Context, []storage.PRReview) error { panic("not used") }
func (prStoreStub) UpsertMember(context.Context, storage.Member) error        { panic("not used") }
func (prStoreStub) SetMemberIdentity(context.Context, string, storage.MemberIdentity) error {
	panic("not used")
}
func (prStoreStub) ListMembers(context.Context) ([]storage.Member, error) { panic("not used") }
func (prStoreStub) MemberWeekMetrics(context.Context, storage.MemberWeekQuery) ([]storage.MemberWeekRow, error) {
	panic("not used")
}

// TestGetData_PRStoreAvailable_ReflectsLastFetchOutcome guards the P2
// fix: PRStoreAvailable must encode runtime fetch health, not just
// "store is configured". When fetchPullRequests last failed against a
// configured store, GetData has to report PRStoreAvailable=false so the
// Lead Time calculator falls back to its Jira-only days path —
// otherwise the PR-backed path runs against an empty PR slice, every
// issue is classified C4, and the metric disappears.
func TestGetData_PRStoreAvailable_ReflectsLastFetchOutcome(t *testing.T) {
	cases := []struct {
		name      string
		store     storage.Store
		prFetchOK bool
		want      bool
	}{
		{"no store configured: legacy minimal deployment", nil, true, false},
		{"store configured, last fetch ok: PR-backed path", prStoreStub{}, true, true},
		{"store configured, last fetch failed: must degrade", prStoreStub{}, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := &Collector{
				store:     tc.store,
				prFetchOK: tc.prFetchOK,
				config:    &config.Metrics{HistoricalDays: 90},
			}
			data, err := c.GetData()
			if err != nil {
				t.Fatalf("GetData: %v", err)
			}
			if data.PRStoreAvailable != tc.want {
				t.Errorf("PRStoreAvailable = %v, want %v", data.PRStoreAvailable, tc.want)
			}
		})
	}
}

// TestParseVersionName guards the invalid-component fix: names that do
// not carry a component-X.Y.Z pattern must return an empty component
// instead of the legacy whole-name fallback that polluted every
// per-component metric with buckets like "0.3" or "v2.1".
func TestParseVersionName(t *testing.T) {
	cases := []struct {
		name                string
		wantComponent       string
		major, minor, patch int
	}{
		// Valid names — component extracted.
		{"argo-cd-2.9.0", "argo-cd", 2, 9, 0},
		{"tektoncd-operator-v4.6.3", "tektoncd-operator", 4, 6, 3},
		{"harbor 1.2.3", "harbor", 1, 2, 3},
		{"connectors-operator-1.2.3-rc1", "connectors-operator", 1, 2, 3},
		// Legacy / invalid names — no component.
		{"0.3", "", 0, 0, 0},
		{"v2.1", "", 0, 0, 0},
		{"1.0", "", 0, 0, 0},
		{"Sprint 2024", "", 0, 0, 0},
		{"", "", 0, 0, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			component, major, minor, patch := parseVersionName(tc.name)
			if component != tc.wantComponent || major != tc.major || minor != tc.minor || patch != tc.patch {
				t.Errorf("parseVersionName(%q) = (%q, %d, %d, %d), want (%q, %d, %d, %d)",
					tc.name, component, major, minor, patch,
					tc.wantComponent, tc.major, tc.minor, tc.patch)
			}
		})
	}
}

// TestDropInvalidComponentReleases covers both invalid classes: empty
// component (unparsable version name) and metrics.exclude_plugins (D6 —
// v3-era plugins).
func TestDropInvalidComponentReleases(t *testing.T) {
	c := &Collector{
		config: &config.Metrics{
			ExcludePlugins: []string{"katanomi", "knative", "jenkins", "tekton-operator"},
		},
		logger: zap.NewNop(),
	}
	in := []models.EnrichedRelease{
		{Name: "tektoncd-operator-v4.6.3", Component: "tektoncd-operator"},
		{Name: "0.3", Component: ""},                                // unparsable → dropped
		{Name: "katanomi-v3.1.0", Component: "katanomi"},            // D6 → dropped
		{Name: "tekton-operator-v3.20.0", Component: "tekton-operator"}, // D6 → dropped
		{Name: "argo-cd-2.9.0", Component: "argo-cd"},
	}
	got := c.dropInvalidComponentReleases(in)
	want := []string{"tektoncd-operator", "argo-cd"}
	gotComponents := make([]string, 0, len(got))
	for _, r := range got {
		gotComponents = append(gotComponents, r.Component)
	}
	if !reflect.DeepEqual(gotComponents, want) {
		t.Errorf("kept components = %v, want %v", gotComponents, want)
	}
}

// TestDropExcludedComponents covers the issue-side path: D6 plugins are
// removed from issue component lists while everything else is kept.
func TestDropExcludedComponents(t *testing.T) {
	c := &Collector{config: &config.Metrics{ExcludePlugins: []string{"katanomi", "jenkins"}}}
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{"mixed", []string{"katanomi", "tektoncd-operator", "jenkins"}, []string{"tektoncd-operator"}},
		{"all excluded", []string{"katanomi"}, []string{}},
		{"none excluded", []string{"argo-cd"}, []string{"argo-cd"}},
		{"empty", nil, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := c.dropExcludedComponents(tc.in); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("dropExcludedComponents(%v) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}
