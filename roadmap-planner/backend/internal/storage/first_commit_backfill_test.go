/*
Copyright 2024 The AlaudaDevops Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package storage

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// TestListMergedPRsMissingFirstCommit_Backfill guards the post-0009
// backfill path. The query must return:
//   - only the requested source (no cross-platform leak),
//   - only merged PRs (open / closed-without-merge are excluded),
//   - only rows still missing first_commit_at,
//   - only rows whose merged_at is within the window,
//   - at most `limit` rows, ordered merged_at DESC (recent history first).
// UpdatePRFirstCommitAt should populate the column so a follow-up
// listing returns one fewer row — that is the "convergence" property
// the syncer relies on.
func TestListMergedPRsMissingFirstCommit_Backfill(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenSQLite(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	ctx := context.Background()
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	now := time.Now().UTC().Truncate(time.Second)
	merged60d := now.AddDate(0, 0, -60)
	merged30d := now.AddDate(0, 0, -30)
	merged1d := now.AddDate(0, 0, -1)
	existingFirstCommit := merged30d.Add(-2 * 24 * time.Hour)

	prs := []PullRequest{
		// In window, gitlab, missing first_commit_at — must show up.
		{
			ID: "org/proj!10", Source: "gitlab", RepoID: "org/proj", Number: 10,
			Title: "gl old", State: "merged",
			CreatedAt: merged60d.Add(-time.Hour), MergedAt: &merged60d, FetchedAt: now,
		},
		// In window, github, missing first_commit_at — must show up.
		{
			ID: "org/repo#11", Source: "github", RepoID: "org/repo", Number: 11,
			Title: "gh recent", State: "merged",
			CreatedAt: merged1d.Add(-time.Hour), MergedAt: &merged1d, FetchedAt: now,
		},
		// In window, github, ALREADY has first_commit_at — must be excluded.
		{
			ID: "org/repo#12", Source: "github", RepoID: "org/repo", Number: 12,
			Title: "gh with commit", State: "merged",
			CreatedAt:     merged30d.Add(-time.Hour),
			FirstCommitAt: &existingFirstCommit,
			MergedAt:      &merged30d, FetchedAt: now,
		},
		// Open (no merged_at) — must be excluded even though first_commit_at is NULL.
		{
			ID: "org/repo#13", Source: "github", RepoID: "org/repo", Number: 13,
			Title: "gh open", State: "open",
			CreatedAt: now.Add(-time.Hour), FetchedAt: now,
		},
		// Merged BEFORE the window — must be excluded.
		{
			ID: "org/repo#14", Source: "github", RepoID: "org/repo", Number: 14,
			Title: "gh ancient", State: "merged",
			CreatedAt: now.AddDate(-1, 0, 0), MergedAt: ptrTime(now.AddDate(-1, 0, 0)), FetchedAt: now,
		},
		// In window, github, also missing — second github row to exercise ordering.
		{
			ID: "org/repo#15", Source: "github", RepoID: "org/repo", Number: 15,
			Title: "gh mid", State: "merged",
			CreatedAt: merged30d.Add(-time.Hour), MergedAt: &merged30d, FetchedAt: now,
		},
	}
	if err := store.UpsertPullRequests(ctx, prs); err != nil {
		t.Fatalf("seed PRs: %v", err)
	}

	windowStart := now.AddDate(0, 0, -180)

	// --- GitHub source: exactly the two NULL rows, recent first. ---
	gh, err := store.ListMergedPRsMissingFirstCommit(ctx, "github", windowStart, 10)
	if err != nil {
		t.Fatalf("list github: %v", err)
	}
	if len(gh) != 2 {
		t.Fatalf("github rows = %d, want 2 (excludes #12 with commit, #13 open, #14 out-of-window)", len(gh))
	}
	if gh[0].ID != "org/repo#11" || gh[1].ID != "org/repo#15" {
		t.Errorf("github order = [%s, %s], want [org/repo#11, org/repo#15] (DESC by merged_at)",
			gh[0].ID, gh[1].ID)
	}

	// --- GitLab source: one row, no cross-platform leak. ---
	gl, err := store.ListMergedPRsMissingFirstCommit(ctx, "gitlab", windowStart, 10)
	if err != nil {
		t.Fatalf("list gitlab: %v", err)
	}
	if len(gl) != 1 || gl[0].ID != "org/proj!10" {
		t.Fatalf("gitlab rows = %+v, want [org/proj!10] only", gl)
	}

	// --- limit honoured. ---
	limited, err := store.ListMergedPRsMissingFirstCommit(ctx, "github", windowStart, 1)
	if err != nil {
		t.Fatalf("list limited: %v", err)
	}
	if len(limited) != 1 {
		t.Fatalf("limited rows = %d, want 1", len(limited))
	}

	// --- UpdatePRFirstCommitAt removes a row from the result set. ---
	commitDate := merged1d.Add(-3 * 24 * time.Hour)
	if err := store.UpdatePRFirstCommitAt(ctx, "org/repo#11", &commitDate); err != nil {
		t.Fatalf("update first_commit_at: %v", err)
	}
	after, err := store.ListMergedPRsMissingFirstCommit(ctx, "github", windowStart, 10)
	if err != nil {
		t.Fatalf("list after update: %v", err)
	}
	if len(after) != 1 || after[0].ID != "org/repo#15" {
		t.Fatalf("after update = %+v, want only org/repo#15 (the still-NULL row)", after)
	}
	// Sanity: the update actually persisted the value (so a future
	// listing won't oscillate).
	listed, err := store.ListPullRequestsSince(ctx, windowStart)
	if err != nil {
		t.Fatalf("list since: %v", err)
	}
	var got *time.Time
	for _, p := range listed {
		if p.ID == "org/repo#11" {
			got = p.FirstCommitAt
			break
		}
	}
	if got == nil || !got.Equal(commitDate) {
		t.Errorf("persisted first_commit_at = %v, want %v", got, commitDate)
	}
}

func ptrTime(t time.Time) *time.Time { return &t }
