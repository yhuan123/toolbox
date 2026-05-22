/*
Copyright 2024 The AlaudaDevops Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

// Package storage is the persistence layer for roadmap-planner team analytics.
//
// The package owns the schema (migrations/) and exposes one interface,
// Store, with a single in-tree SQLite implementation. The interface is
// shaped so a Postgres implementation can be added later without touching
// callers — we keep SQL portable (no SQLite-isms in the application
// queries) and use database/sql throughout.
//
// See docs/team-analytics/PROPOSAL.md §6 for schema rationale.
package storage

import (
	"context"
	"database/sql"
	"time"
)

// Store is the persistence boundary for team-analytics data.
//
// Two responsibilities:
//   - durable history (snapshots written every collection cycle, immutable)
//   - rollups (member_week_metrics — derived, droppable, queried by the API)
//
// All methods must be safe for concurrent use; implementations rely on the
// underlying *sql.DB pool for that.
type Store interface {
	// Lifecycle.
	Close() error
	Migrate(ctx context.Context) error
	DB() *sql.DB

	// Collection runs — every Collect() call ends with one of these.
	WriteCollectionRun(ctx context.Context, r CollectionRun) error
	LatestCollectionRun(ctx context.Context, source string) (*CollectionRun, error)

	// Issue snapshots (one row per issue per run).
	WriteIssueSnapshots(ctx context.Context, runID string, issues []IssueSnapshot) error

	// GitHub.
	UpsertPullRequests(ctx context.Context, prs []PullRequest) error
	UpsertPRReviews(ctx context.Context, reviews []PRReview) error
	// ListPullRequestsSince returns all PRs (github + gitlab) that merged
	// on or after `since`. Open / closed-without-merge rows are excluded —
	// the DORA Lead Time calculator only attributes shipped work.
	ListPullRequestsSince(ctx context.Context, since time.Time) ([]PullRequest, error)
	// ListMergedPRsMissingFirstCommit returns up to `limit` merged PRs
	// for the given source that still have first_commit_at IS NULL and
	// merged on or after `since`. Drives the post-0009 backfill: rows
	// ingested before migration 0009 (or by an older syncer build) keep
	// NULL first_commit_at until a later sync touches them, which the
	// incremental PR fetch can never do for already-merged history.
	// Without backfill those rows fall to Lead Time C3 (no Dev stage),
	// distorting the metric on deployments upgraded across 0009.
	// Ordered by merged_at DESC so recent history is rehydrated first.
	ListMergedPRsMissingFirstCommit(ctx context.Context, source string, since time.Time, limit int) ([]PullRequest, error)
	// UpdatePRFirstCommitAt writes a single first_commit_at value
	// without touching any other column. Use for backfill paths — full
	// UpsertPullRequests would require re-fetching all PR fields just to
	// move one column.
	UpdatePRFirstCommitAt(ctx context.Context, id string, firstCommitAt *time.Time) error

	// Members.
	UpsertMember(ctx context.Context, m Member) error
	// SetMemberIdentity writes the operator-editable identity fields
	// literally — empty values clear the field. Use for the PATCH
	// /api/contributions/members/:id path, where UpsertMember's
	// COALESCE-preserve semantics would silently drop an explicit clear.
	SetMemberIdentity(ctx context.Context, id string, ident MemberIdentity) error
	ListMembers(ctx context.Context) ([]Member, error)

	// Read paths used by the contributions service.
	MemberWeekMetrics(ctx context.Context, q MemberWeekQuery) ([]MemberWeekRow, error)
}

// ----------------------------------------------------------------------
// Domain shapes — kept narrow on purpose, mapped 1:1 to the schema.
// ----------------------------------------------------------------------

// CollectionRun is the audit record for a single fetch cycle.
type CollectionRun struct {
	ID          string
	CapturedAt  time.Time
	Source      string // "jira" | "github"
	DurationMs  int64
	RecordCount int
	Error       string
}

// IssueSnapshot is the durable shape of a Jira issue at a point in time.
// It's deliberately denormalised so a single SELECT serves the dashboards.
type IssueSnapshot struct {
	IssueKey    string
	IssueType   string
	Status      string
	AssigneeID  string // FK to members.id, may be empty
	PillarID    string
	Components  []string // serialised JSON in storage
	Versions    []string
	SprintID    string
	StoryPoints float64
	CreatedAt   time.Time
	ResolvedAt  *time.Time
}

// PullRequest is a GitHub PR or GitLab MR record. Linked to a Jira
// issue via JiraKey when the configured Linker can resolve one
// (branch regex, title, etc.). JiraKey is *not* restricted to Epics —
// the regex matches any DEVOPS-NNN id, which in prod is mostly
// Stories / Bugs (see migration 0006 for the audit data behind the
// rename from `epic_key`).
//
// AuthorLogin is the raw login string returned by the source API (lower-
// cased on write). It's stored alongside the resolved AuthorID so the
// aggregator can re-link history when an operator later fills in
// `members.github_login` (or `members.gitlab_username`) via PATCH — see
// migrations/0002 and 0003.
//
// Source distinguishes "github" from "gitlab" so the two providers can
// share the same table without losing provenance. Defaults to "github"
// for rows ingested before the 0003 migration.
type PullRequest struct {
	ID                 string // "owner/name#number" (github) or "group/sub/proj!iid" (gitlab)
	Source             string // "github" | "gitlab"
	RepoID             string
	Number             int
	Title              string
	State              string // "open" | "merged" | "closed"
	AuthorID           string // FK to members.id, empty if no match
	AuthorLogin        string // raw login from the source API, lower-cased
	HeadBranch         string
	BaseBranch         string
	Additions          int
	Deletions          int
	ChangedFiles       int
	JiraKey            string // any Jira key matched by the linker; was misnamed `EpicKey` pre-W5
	CreatedAt          time.Time
	FirstCommitAt      *time.Time // DORA Phase 2: MIN(commit.author.date) from PR/MR commits API
	FirstReviewAt      *time.Time
	FirstHumanReviewAt *time.Time // W2: MIN(submitted_at) over non-bot reviews
	MergedAt           *time.Time
	ClosedAt           *time.Time
	FetchedAt          time.Time
}

// PRReview is one review event on a PR or MR.
//
// ReviewerLogin mirrors PullRequest.AuthorLogin: the raw login enables
// retroactive re-linking after an identity edit.
//
// On GitLab the surrogate for "review" is a non-system, non-author MR
// note: a `/lgtm` body lands as state="approved", any other substantive
// note as state="commented". Procedural prow commands (/retest, /hold,
// /cherry-pick, ...) are filtered out at sync time.
type PRReview struct {
	ID            string
	PRID          string
	Source        string // "github" | "gitlab"
	ReviewerID    string
	ReviewerLogin string // raw login from the source API, lower-cased
	State         string // approved | changes_requested | commented
	SubmittedAt   time.Time
	IsBot         bool // W2: reviewer_login matched the configured bot allowlist
}

// MemberIdentity is the operator-editable identity payload for the PATCH
// endpoint and config-driven prefills. All fields are writes-take-precedence
// — empty strings clear the corresponding column.
type MemberIdentity struct {
	DisplayName    string
	GitHubLogin    string
	GitLabUsername string
}

// Member is the join entity across Jira, GitHub, and GitLab.
//
// JSON tags use snake_case to match what the frontend (and any future
// API consumer) expects; without them the default marshaller emits
// PascalCase field names and the Team dashboard breaks because
// `m.display_name` is undefined.
type Member struct {
	ID             string    `json:"id"`
	DisplayName    string    `json:"display_name"`
	Email          string    `json:"email,omitempty"`
	JiraAccountID  string    `json:"jira_account_id,omitempty"`
	GitHubLogin    string    `json:"github_login,omitempty"`
	GitLabUsername string    `json:"gitlab_username,omitempty"`
	Active         bool      `json:"active"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// ----------------------------------------------------------------------
// Query parameters / result shapes
// ----------------------------------------------------------------------

// MemberWeekQuery filters the rollup read.
type MemberWeekQuery struct {
	From      time.Time
	To        time.Time
	MemberIDs []string
	PillarIDs []string
	Component string
}

// MemberWeekRow is one row of the contributions feed used by the Team
// dashboard. Mirrors member_week_metrics 1:1.
type MemberWeekRow struct {
	MemberID              string    `json:"member_id"`
	WeekStart             time.Time `json:"week_start"`
	PillarID              string    `json:"pillar_id"`
	Component             string    `json:"component"`
	JiraIssuesDone        int       `json:"jira_issues_done"`
	JiraPointsDone        float64   `json:"jira_points_done"`
	PRsMerged             int       `json:"prs_merged"`
	PRsOpened             int       `json:"prs_opened"`
	PRsReviewed           int       `json:"prs_reviewed"`
	ReviewLatencyP50Hours *float64  `json:"review_latency_p50_hours,omitempty"`
}
