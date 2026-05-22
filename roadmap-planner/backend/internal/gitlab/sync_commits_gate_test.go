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

// TestSync_ListMRCommitsOnlyForMergedMRs is the GitLab counterpart of
// TestSync_ListPRCommitsOnlyForMergedPRs. Pass A path only — Pass B
// is exercised in TestSyncPassBMemberSweep. Verifies:
//   - commits endpoint hit exactly once (merged MR)
//   - notes endpoint hit three times (everything except the draft)
//   - draft skip still in place
func TestSync_ListMRCommitsOnlyForMergedMRs(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	store, err := storage.OpenSQLite(filepath.Join(dir, "gl.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	now := time.Now().UTC().Truncate(time.Second)
	merged := now.Add(-3 * 24 * time.Hour)
	closedRecently := now.Add(-1 * 24 * time.Hour)

	const projectID = 42
	const projectPath = "devops/edge"

	// Four MRs covering the relevant skip permutations:
	//   1 merged                → commits + notes
	//   2 open non-draft        → notes only
	//   3 closed unmerged <7d   → notes only
	//   4 draft                 → neither (top-level skip)
	mrs := []MergeRequest{
		{IID: 1, ProjectID: projectID, Title: "merged mr", State: "merged",
			CreatedAt: now.Add(-5 * 24 * time.Hour), UpdatedAt: now,
			MergedAt: &merged},
		{IID: 2, ProjectID: projectID, Title: "open mr", State: "opened",
			CreatedAt: now.Add(-2 * 24 * time.Hour), UpdatedAt: now},
		{IID: 3, ProjectID: projectID, Title: "closed unmerged recently", State: "closed",
			CreatedAt: now.Add(-2 * 24 * time.Hour), UpdatedAt: now,
			ClosedAt: &closedRecently},
		{IID: 4, ProjectID: projectID, Title: "draft", State: "opened", Draft: true,
			CreatedAt: now.Add(-1 * 24 * time.Hour), UpdatedAt: now},
	}

	var commitsHits, notesHits int32
	commitsSeen := make(map[int]bool)
	notesSeen := make(map[int]bool)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		raw := r.URL.RawPath
		if raw == "" {
			raw = r.URL.Path
		}
		switch {
		// GetProject (URL-escaped path → "devops%2Fedge").
		case raw == "/api/v4/projects/devops%2Fedge":
			_, _ = fmt.Fprintf(w, `{"id":%d,"path_with_namespace":%q}`, projectID, projectPath)
		case r.URL.Path == fmt.Sprintf("/api/v4/projects/%d/merge_requests", projectID):
			if r.URL.Query().Get("page") != "1" {
				_, _ = w.Write([]byte(`[]`))
				return
			}
			_ = json.NewEncoder(w).Encode(mrs)
		case strings.HasPrefix(r.URL.Path, fmt.Sprintf("/api/v4/projects/%d/merge_requests/", projectID)):
			var iid int
			tail := strings.TrimPrefix(r.URL.Path,
				fmt.Sprintf("/api/v4/projects/%d/merge_requests/", projectID))
			switch {
			case strings.HasSuffix(tail, "/commits"):
				_, _ = fmt.Sscanf(tail, "%d/commits", &iid)
				atomic.AddInt32(&commitsHits, 1)
				commitsSeen[iid] = true
				_, _ = fmt.Fprintf(w, `[{"id":"a","authored_date":%q}]`,
					merged.Add(-2*24*time.Hour).Format(time.RFC3339))
			case strings.HasSuffix(tail, "/notes"):
				_, _ = fmt.Sscanf(tail, "%d/notes", &iid)
				atomic.AddInt32(&notesHits, 1)
				notesSeen[iid] = true
				_, _ = w.Write([]byte(`[]`))
			default:
				t.Errorf("unexpected MR subpath: %s", r.URL.Path)
				http.Error(w, "not found", http.StatusNotFound)
			}
		default:
			t.Errorf("unexpected request: %s (raw %s)", r.URL.Path, raw)
			http.Error(w, "not found", http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	spec, ok := ParseGroupSpec(projectPath)
	if !ok {
		t.Fatalf("ParseGroupSpec failed")
	}
	syncer := NewSyncer(New(srv.URL, "tok", srv.Client()), store, []GroupSpec{spec}, nil, 30)
	syncer.HydrateDiff = false // avoid an extra GetMergeRequest call we don't care about here
	if err := syncer.Sync(ctx); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	if got := atomic.LoadInt32(&commitsHits); got != 1 {
		t.Errorf("commits endpoint hits = %d, want 1 (only merged MR #1)", got)
	}
	if !commitsSeen[1] {
		t.Errorf("commits not called for merged MR #1: %+v", commitsSeen)
	}
	for _, n := range []int{2, 3, 4} {
		if commitsSeen[n] {
			t.Errorf("commits called for unmerged MR #%d — gate must skip non-merged MRs", n)
		}
	}

	if got := atomic.LoadInt32(&notesHits); got != 3 {
		t.Errorf("notes endpoint hits = %d, want 3 (skip only the draft #4)", got)
	}
	if notesSeen[4] {
		t.Error("notes fetched for draft MR #4 — draft skip regressed")
	}
}
