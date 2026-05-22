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
	"testing"
	"time"

	"github.com/AlaudaDevops/toolbox/roadmap-planner/backend/internal/config"
	"github.com/AlaudaDevops/toolbox/roadmap-planner/backend/internal/storage"
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
