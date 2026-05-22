/*
Copyright 2026 The AlaudaDevops Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package gitlab

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/AlaudaDevops/toolbox/roadmap-planner/backend/internal/storage"
)

// TestParseMRID locks the MR id grammar (group/.../proj!iid) the
// post-0009 backfill depends on. The project path is multi-segment
// (groups + subgroups), so we split on the trailing "!" only.
func TestParseMRID(t *testing.T) {
	cases := []struct {
		in       string
		wantOK   bool
		wantPath string
		wantIID  int
	}{
		{"devops/edge!42", true, "devops/edge", 42},
		{"group/sub/sub/proj!9999", true, "group/sub/sub/proj", 9999},
		{"single!1", true, "single", 1},
		{"missing-bang", false, "", 0},
		{"trailing!", false, "", 0},
		{"!42", false, "", 0},
		{"path!abc", false, "", 0},
		{"path!-1", false, "", 0},
		{"path!0", false, "", 0},
		{"", false, "", 0},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			gotPath, gotIID, gotOK := parseMRID(tc.in)
			if gotOK != tc.wantOK {
				t.Fatalf("ok = %v, want %v", gotOK, tc.wantOK)
			}
			if !tc.wantOK {
				return
			}
			if gotPath != tc.wantPath || gotIID != tc.wantIID {
				t.Errorf("parsed = (%q, %d), want (%q, %d)",
					gotPath, gotIID, tc.wantPath, tc.wantIID)
			}
		})
	}
}

// TestBackfillFirstCommit_GitLab_EndToEnd mirrors the GitHub backfill
// end-to-end test against a real SQLite store and a mocked GitLab
// API. Verifies:
//   - target row gets the min(authored_date) written,
//   - already-populated row is untouched,
//   - GetProject(path) is called once per project (cache hit on the
//     second MR from the same project),
//   - a commits-API error on one row does not abort the batch.
func TestBackfillFirstCommit_GitLab_EndToEnd(t *testing.T) {
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
			ID: "devops/edge!1", Source: "gitlab", RepoID: "devops/edge", Number: 1,
			Title: "needs backfill", State: "merged",
			CreatedAt: merged.Add(-time.Hour), MergedAt: &merged, FetchedAt: now,
		},
		{ // second MR same project → exercises projectID cache
			ID: "devops/edge!2", Source: "gitlab", RepoID: "devops/edge", Number: 2,
			Title: "same project", State: "merged",
			CreatedAt: merged.Add(-30 * time.Minute), MergedAt: &merged, FetchedAt: now,
		},
		{ // already populated — must be left alone (and not show up in batch)
			ID: "devops/edge!3", Source: "gitlab", RepoID: "devops/edge", Number: 3,
			Title: "already has commit", State: "merged",
			CreatedAt:     merged.Add(-time.Hour),
			FirstCommitAt: &existing,
			MergedAt:      &merged, FetchedAt: now,
		},
		{ // commits API will 500 — row stays NULL, batch must continue
			ID: "devops/edge!4", Source: "gitlab", RepoID: "devops/edge", Number: 4,
			Title: "api fails", State: "merged",
			CreatedAt: merged.Add(-time.Hour), MergedAt: &merged, FetchedAt: now,
		},
	}); err != nil {
		t.Fatalf("seed PRs: %v", err)
	}

	commit1Date := merged.Add(-5 * 24 * time.Hour)
	commit2Date := merged.Add(-7 * 24 * time.Hour)

	var projectHits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// The client URL-escapes the project path, but Go's HTTP server
		// decodes r.URL.Path. RawPath preserves the escaped form (when
		// non-canonical) so we can match the wire format directly.
		raw := r.URL.RawPath
		if raw == "" {
			raw = r.URL.Path
		}
		switch {
		case raw == "/api/v4/projects/devops%2Fedge":
			projectHits++
			_, _ = fmt.Fprintln(w, `{"id":42,"path_with_namespace":"devops/edge"}`)
		case strings.HasPrefix(raw, "/api/v4/projects/42/merge_requests/"):
			switch {
			case strings.HasSuffix(raw, "/1/commits"):
				_, _ = fmt.Fprintf(w, `[{"id":"a","authored_date":%q}]`, commit1Date.Format(time.RFC3339))
			case strings.HasSuffix(raw, "/2/commits"):
				_, _ = fmt.Fprintf(w, `[{"id":"b","authored_date":%q}]`, commit2Date.Format(time.RFC3339))
			case strings.HasSuffix(raw, "/4/commits"):
				http.Error(w, "boom", http.StatusInternalServerError)
			default:
				t.Errorf("unexpected commits path: %s", raw)
				http.Error(w, "not found", http.StatusNotFound)
			}
		default:
			t.Errorf("unexpected request: %s", raw)
			http.Error(w, "not found", http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	syncer := NewSyncer(New(srv.URL, "tok", srv.Client()), store, nil, nil, 30)
	if err := syncer.backfillFirstCommit(ctx); err != nil {
		t.Fatalf("backfillFirstCommit: %v", err)
	}

	if projectHits != 1 {
		t.Errorf("GetProject called %d times, want 1 (path→projectID cache must dedupe)", projectHits)
	}

	prs, err := store.ListPullRequestsSince(ctx, now.AddDate(0, 0, -60))
	if err != nil {
		t.Fatalf("list PRs: %v", err)
	}
	got := map[string]*time.Time{}
	for _, p := range prs {
		got[p.ID] = p.FirstCommitAt
	}
	if got["devops/edge!1"] == nil || !got["devops/edge!1"].Equal(commit1Date) {
		t.Errorf("!1 first_commit_at = %v, want %v", got["devops/edge!1"], commit1Date)
	}
	if got["devops/edge!2"] == nil || !got["devops/edge!2"].Equal(commit2Date) {
		t.Errorf("!2 first_commit_at = %v, want %v", got["devops/edge!2"], commit2Date)
	}
	if got["devops/edge!3"] == nil || !got["devops/edge!3"].Equal(existing) {
		t.Errorf("!3 first_commit_at = %v, want %v (backfill overwrote existing value)",
			got["devops/edge!3"], existing)
	}
	if got["devops/edge!4"] != nil {
		t.Errorf("!4 first_commit_at = %v, want nil (API errored — row must stay eligible for retry)",
			got["devops/edge!4"])
	}
}
