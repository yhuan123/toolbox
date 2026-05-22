/*
Copyright 2024 The AlaudaDevops Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package github

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/AlaudaDevops/toolbox/roadmap-planner/backend/internal/storage"
)

// TestParsePRID locks the PR id grammar (owner/name#number) the
// post-0009 backfill depends on. Whoever changes the id shape must
// update parsePRID too — otherwise backfillFirstCommit silently
// skips every row.
func TestParsePRID(t *testing.T) {
	cases := []struct {
		in        string
		wantOK    bool
		wantOwner string
		wantName  string
		wantNum   int
	}{
		{"AlaudaDevops/toolbox#173", true, "AlaudaDevops", "toolbox", 173},
		{"alaudadevops/tektoncd-operator#9999", true, "alaudadevops", "tektoncd-operator", 9999},
		{"org/with.dots_and-dashes#1", true, "org", "with.dots_and-dashes", 1},
		{"missing-number-marker", false, "", "", 0},
		{"org/repo#", false, "", "", 0},
		{"#42", false, "", "", 0},
		{"org/repo#abc", false, "", "", 0},
		{"org/repo#-1", false, "", "", 0},
		{"#", false, "", "", 0},
		{"", false, "", "", 0},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			gotOwner, gotName, gotNum, gotOK := parsePRID(tc.in)
			if gotOK != tc.wantOK {
				t.Fatalf("ok = %v, want %v", gotOK, tc.wantOK)
			}
			if !tc.wantOK {
				return
			}
			if gotOwner != tc.wantOwner || gotName != tc.wantName || gotNum != tc.wantNum {
				t.Errorf("parsed = (%q, %q, %d), want (%q, %q, %d)",
					gotOwner, gotName, gotNum, tc.wantOwner, tc.wantName, tc.wantNum)
			}
		})
	}
}

// TestBackfillFirstCommit_EndToEnd exercises the full backfill path
// against a real SQLite store and a mocked PR-commits endpoint:
//   - one PR with NULL first_commit_at in window  → must be updated
//   - one PR already populated                    → must stay untouched
//   - one PR whose commits API errors             → row stays NULL,
//     other rows still get updated (single failure must not abort)
// This is the regression barrier for the P2 review: without the
// backfill the first row stays NULL and Lead Time misclassifies it
// as C3.
func TestBackfillFirstCommit_EndToEnd(t *testing.T) {
	dir := t.TempDir()
	store, err := storage.OpenSQLite(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	ctx := context.Background()
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	now := time.Now().UTC().Truncate(time.Second)
	merged := now.AddDate(0, 0, -10)
	existing := merged.Add(-3 * 24 * time.Hour)

	if err := store.UpsertPullRequests(ctx, []storage.PullRequest{
		{ // target — must get backfilled
			ID: "org/repo#1", Source: "github", RepoID: "org/repo", Number: 1,
			Title: "needs backfill", State: "merged",
			CreatedAt: merged.Add(-time.Hour), MergedAt: &merged, FetchedAt: now,
		},
		{ // already populated — must be left alone
			ID: "org/repo#2", Source: "github", RepoID: "org/repo", Number: 2,
			Title: "already has commit", State: "merged",
			CreatedAt:     merged.Add(-time.Hour),
			FirstCommitAt: &existing,
			MergedAt:      &merged, FetchedAt: now,
		},
		{ // commits API will error — must stay NULL but not abort batch
			ID: "org/repo#3", Source: "github", RepoID: "org/repo", Number: 3,
			Title: "api fails", State: "merged",
			CreatedAt: merged.Add(-time.Hour), MergedAt: &merged, FetchedAt: now,
		},
	}); err != nil {
		t.Fatalf("seed PRs: %v", err)
	}

	commit1Date := merged.Add(-5 * 24 * time.Hour)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-RateLimit-Remaining", "5000")
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/repos/org/repo/pulls/1/commits":
			_, _ = fmt.Fprintf(w, `[{"sha":"a","commit":{"author":{"date":%q}}}]`,
				commit1Date.Format(time.RFC3339))
		case "/repos/org/repo/pulls/3/commits":
			http.Error(w, "boom", http.StatusInternalServerError)
		default:
			t.Errorf("unexpected commits request: %s", r.URL.Path)
			http.Error(w, "not found", http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	syncer := NewSyncer(New(srv.URL, "tok", srv.Client()), store, nil, nil, 30)
	if err := syncer.backfillFirstCommit(ctx); err != nil {
		t.Fatalf("backfillFirstCommit: %v", err)
	}

	// Sanity-check: read the rows back via ListPullRequestsSince.
	prs, err := store.ListPullRequestsSince(ctx, now.AddDate(0, 0, -60))
	if err != nil {
		t.Fatalf("list PRs: %v", err)
	}
	got := map[string]*time.Time{}
	for _, p := range prs {
		got[p.ID] = p.FirstCommitAt
	}
	if got["org/repo#1"] == nil || !got["org/repo#1"].Equal(commit1Date) {
		t.Errorf("org/repo#1 first_commit_at = %v, want %v (backfill failed)",
			got["org/repo#1"], commit1Date)
	}
	if got["org/repo#2"] == nil || !got["org/repo#2"].Equal(existing) {
		t.Errorf("org/repo#2 first_commit_at = %v, want %v (backfill overwrote existing value)",
			got["org/repo#2"], existing)
	}
	if got["org/repo#3"] != nil {
		t.Errorf("org/repo#3 first_commit_at = %v, want nil (API errored — row must stay eligible for retry)",
			got["org/repo#3"])
	}
}
