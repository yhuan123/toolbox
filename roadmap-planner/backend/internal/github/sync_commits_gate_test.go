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
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AlaudaDevops/toolbox/roadmap-planner/backend/internal/storage"
)

// TestSync_ListPRCommitsOnlyForMergedPRs guards the P2 fix: the
// commits endpoint must be called for merged PRs only, not for every
// non-draft / recently-closed PR. Reviews stay broader (same skip
// rules as before) since they accrue on open PRs too.
//
// The Lead Time calculator drops unmerged PRs (filterPreReleasePRs)
// and ListPullRequestsSince filters merged_at IS NOT NULL — fetching
// first_commit_at on open / closed-unmerged PRs is rate-budget waste.
func TestSync_ListPRCommitsOnlyForMergedPRs(t *testing.T) {
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

	now := time.Now().UTC()
	merged := now.Add(-3 * 24 * time.Hour)
	closedRecently := now.Add(-1 * 24 * time.Hour) // <7d → reviews still fetched

	// Four PRs covering the relevant skip permutations:
	//   #1 merged           → commits + reviews
	//   #2 open non-draft   → reviews only (no commits)
	//   #3 closed unmerged <7d → reviews only (no commits)
	//   #4 draft            → neither (top-level skip)
	prs := []PullRequest{
		{Number: 1, Title: "merged pr", State: "closed", Draft: false,
			CreatedAt: now.Add(-5 * 24 * time.Hour), UpdatedAt: now,
			MergedAt: &merged},
		{Number: 2, Title: "open pr", State: "open", Draft: false,
			CreatedAt: now.Add(-2 * 24 * time.Hour), UpdatedAt: now},
		{Number: 3, Title: "closed unmerged recently", State: "closed", Draft: false,
			CreatedAt: now.Add(-2 * 24 * time.Hour), UpdatedAt: now,
			ClosedAt: &closedRecently},
		{Number: 4, Title: "draft pr", State: "open", Draft: true,
			CreatedAt: now.Add(-1 * 24 * time.Hour), UpdatedAt: now},
	}

	var commitsHits, reviewsHits int32
	commitsSeen := make(map[int]bool)
	reviewsSeen := make(map[int]bool)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-RateLimit-Remaining", "5000")
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/repos/org/repo/pulls":
			page := r.URL.Query().Get("page")
			if page != "1" {
				_, _ = w.Write([]byte(`[]`))
				return
			}
			_ = json.NewEncoder(w).Encode(prs)
		case strings.HasPrefix(r.URL.Path, "/repos/org/repo/pulls/") &&
			strings.HasSuffix(r.URL.Path, "/commits"):
			atomic.AddInt32(&commitsHits, 1)
			var n int
			_, _ = fmt.Sscanf(r.URL.Path, "/repos/org/repo/pulls/%d/commits", &n)
			commitsSeen[n] = true
			_, _ = fmt.Fprintf(w, `[{"sha":"a","commit":{"author":{"date":%q}}}]`,
				merged.Add(-2*24*time.Hour).Format(time.RFC3339))
		case strings.HasPrefix(r.URL.Path, "/repos/org/repo/pulls/") &&
			strings.HasSuffix(r.URL.Path, "/reviews"):
			atomic.AddInt32(&reviewsHits, 1)
			var n int
			_, _ = fmt.Sscanf(r.URL.Path, "/repos/org/repo/pulls/%d/reviews", &n)
			reviewsSeen[n] = true
			_, _ = w.Write([]byte(`[]`))
		default:
			t.Errorf("unexpected request: %s", r.URL.Path)
			http.Error(w, "not found", http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	syncer := NewSyncer(
		New(srv.URL, "tok", srv.Client()),
		store,
		[]RepoConfig{{Owner: "org", Name: "repo"}},
		nil,
		30,
	)
	if err := syncer.Sync(ctx); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	if got := atomic.LoadInt32(&commitsHits); got != 1 {
		t.Errorf("commits endpoint hits = %d, want 1 (only merged PR #1)", got)
	}
	if !commitsSeen[1] {
		t.Errorf("commits not called for merged PR #1: %+v", commitsSeen)
	}
	for _, n := range []int{2, 3, 4} {
		if commitsSeen[n] {
			t.Errorf("commits called for unmerged PR #%d — gate must skip non-merged PRs", n)
		}
	}

	// Reviews: PRs #1, #2, #3 (draft #4 still skipped).
	if got := atomic.LoadInt32(&reviewsHits); got != 3 {
		t.Errorf("reviews endpoint hits = %d, want 3 (skip only the draft #4)", got)
	}
	if reviewsSeen[4] {
		t.Error("reviews fetched for draft PR #4 — draft skip regressed")
	}
}
