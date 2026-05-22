/*
Copyright 2024 The AlaudaDevops Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
*/

package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// genericStore is the single Store implementation, parameterised by a
// Dialect (SQLite or Postgres). It speaks `?`-placeholder SQL and lets
// rebind() shift to `$1`-style at exec time when the dialect requires.
//
// The schema itself is portable — see migrations/0001_init.sql. The only
// dialect-specific SQL lives in the aggregator (week-start expression),
// which routes through dialect.WeekStart.
type genericStore struct {
	db *sql.DB
	d  Dialect
}

func (s *genericStore) Close() error     { return s.db.Close() }
func (s *genericStore) DB() *sql.DB      { return s.db }
func (s *genericStore) Dialect() Dialect { return s.d }

func (s *genericStore) Migrate(ctx context.Context) error {
	return runMigrations(ctx, s.db, s.d)
}

// ----------------------------------------------------------------------
// collection_runs
// ----------------------------------------------------------------------

func (s *genericStore) WriteCollectionRun(ctx context.Context, r CollectionRun) error {
	q := rebind(s.d, `
		INSERT INTO collection_runs (id, captured_at, source, duration_ms, record_count, error)
		VALUES (?, ?, ?, ?, ?, ?)`)
	_, err := s.db.ExecContext(ctx, q,
		r.ID, r.CapturedAt, r.Source, r.DurationMs, r.RecordCount, nullable(r.Error))
	return err
}

func (s *genericStore) LatestCollectionRun(ctx context.Context, source string) (*CollectionRun, error) {
	q := rebind(s.d, `
		SELECT id, captured_at, source, duration_ms, record_count, COALESCE(error, '')
		FROM collection_runs
		WHERE source = ?
		ORDER BY captured_at DESC
		LIMIT 1`)
	row := s.db.QueryRowContext(ctx, q, source)
	var r CollectionRun
	if err := row.Scan(&r.ID, &r.CapturedAt, &r.Source, &r.DurationMs, &r.RecordCount, &r.Error); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	return &r, nil
}

// ----------------------------------------------------------------------
// issue_snapshots
// ----------------------------------------------------------------------

func (s *genericStore) WriteIssueSnapshots(ctx context.Context, runID string, issues []IssueSnapshot) error {
	if len(issues) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	q := rebind(s.d, `
		INSERT INTO issue_snapshots (
			run_id, issue_key, issue_type, status, assignee_id, pillar_id,
			components, versions, sprint_id, story_points, created_at, resolved_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	stmt, err := tx.PrepareContext(ctx, q)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, it := range issues {
		comps, _ := json.Marshal(it.Components)
		vers, _ := json.Marshal(it.Versions)
		_, err := stmt.ExecContext(ctx,
			runID, it.IssueKey, it.IssueType, it.Status,
			nullable(it.AssigneeID), nullable(it.PillarID),
			string(comps), string(vers),
			nullable(it.SprintID), it.StoryPoints,
			it.CreatedAt, it.ResolvedAt,
		)
		if err != nil {
			return fmt.Errorf("insert issue %s: %w", it.IssueKey, err)
		}
	}
	return tx.Commit()
}

// ----------------------------------------------------------------------
// pull_requests / pr_reviews — UPSERT on natural key
// ----------------------------------------------------------------------

func (s *genericStore) UpsertPullRequests(ctx context.Context, prs []PullRequest) error {
	if len(prs) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	q := rebind(s.d, `
		INSERT INTO pull_requests (
			id, source, repo_id, number, title, state, author_id, author_login,
			head_branch, base_branch, additions, deletions, changed_files,
			jira_key, created_at, first_commit_at, first_review_at, first_human_review_at,
			merged_at, closed_at, fetched_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			source = excluded.source,
			title = excluded.title,
			state = excluded.state,
			author_id = excluded.author_id,
			author_login = excluded.author_login,
			additions = excluded.additions,
			deletions = excluded.deletions,
			changed_files = excluded.changed_files,
			jira_key = excluded.jira_key,
			first_commit_at = COALESCE(excluded.first_commit_at, pull_requests.first_commit_at),
			first_review_at = excluded.first_review_at,
			first_human_review_at = excluded.first_human_review_at,
			merged_at = excluded.merged_at,
			closed_at = excluded.closed_at,
			fetched_at = excluded.fetched_at`)
	stmt, err := tx.PrepareContext(ctx, q)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, p := range prs {
		source := p.Source
		if source == "" {
			source = "github"
		}
		_, err := stmt.ExecContext(ctx,
			p.ID, source, p.RepoID, p.Number, p.Title, p.State, nullable(p.AuthorID), nullable(p.AuthorLogin),
			nullable(p.HeadBranch), nullable(p.BaseBranch), p.Additions, p.Deletions, p.ChangedFiles,
			nullable(p.JiraKey), p.CreatedAt, p.FirstCommitAt, p.FirstReviewAt, p.FirstHumanReviewAt, p.MergedAt, p.ClosedAt, p.FetchedAt,
		)
		if err != nil {
			return fmt.Errorf("upsert pr %s: %w", p.ID, err)
		}
	}
	return tx.Commit()
}

func (s *genericStore) ListPullRequestsSince(ctx context.Context, since time.Time) ([]PullRequest, error) {
	q := rebind(s.d, `
		SELECT id, source, repo_id, number, title, state,
		       COALESCE(author_id, '') AS author_id,
		       COALESCE(author_login, '') AS author_login,
		       COALESCE(head_branch, '') AS head_branch,
		       COALESCE(base_branch, '') AS base_branch,
		       COALESCE(additions, 0) AS additions,
		       COALESCE(deletions, 0) AS deletions,
		       COALESCE(changed_files, 0) AS changed_files,
		       COALESCE(jira_key, '') AS jira_key,
		       created_at, first_commit_at, first_review_at, first_human_review_at,
		       merged_at, closed_at, fetched_at
		FROM pull_requests
		WHERE merged_at IS NOT NULL AND merged_at >= ?
		ORDER BY merged_at ASC`)
	rows, err := s.db.QueryContext(ctx, q, since)
	if err != nil {
		return nil, fmt.Errorf("list PRs since %s: %w", since.Format(time.RFC3339), err)
	}
	defer rows.Close()
	out := make([]PullRequest, 0, 256)
	for rows.Next() {
		var p PullRequest
		if err := rows.Scan(
			&p.ID, &p.Source, &p.RepoID, &p.Number, &p.Title, &p.State,
			&p.AuthorID, &p.AuthorLogin, &p.HeadBranch, &p.BaseBranch,
			&p.Additions, &p.Deletions, &p.ChangedFiles, &p.JiraKey,
			&p.CreatedAt, &p.FirstCommitAt, &p.FirstReviewAt, &p.FirstHumanReviewAt,
			&p.MergedAt, &p.ClosedAt, &p.FetchedAt,
		); err != nil {
			return nil, fmt.Errorf("scan PR: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *genericStore) ListMergedPRsMissingFirstCommit(ctx context.Context, source string, since time.Time, limit int) ([]PullRequest, error) {
	if limit <= 0 {
		return nil, nil
	}
	q := rebind(s.d, `
		SELECT id, source, repo_id, number, title, state,
		       COALESCE(author_id, '') AS author_id,
		       COALESCE(author_login, '') AS author_login,
		       COALESCE(head_branch, '') AS head_branch,
		       COALESCE(base_branch, '') AS base_branch,
		       COALESCE(additions, 0) AS additions,
		       COALESCE(deletions, 0) AS deletions,
		       COALESCE(changed_files, 0) AS changed_files,
		       COALESCE(jira_key, '') AS jira_key,
		       created_at, first_commit_at, first_review_at, first_human_review_at,
		       merged_at, closed_at, fetched_at
		FROM pull_requests
		WHERE source = ?
		  AND merged_at IS NOT NULL
		  AND merged_at >= ?
		  AND first_commit_at IS NULL
		ORDER BY merged_at DESC
		LIMIT ?`)
	rows, err := s.db.QueryContext(ctx, q, source, since, limit)
	if err != nil {
		return nil, fmt.Errorf("list PRs missing first_commit_at (%s since %s): %w",
			source, since.Format(time.RFC3339), err)
	}
	defer rows.Close()
	out := make([]PullRequest, 0, limit)
	for rows.Next() {
		var p PullRequest
		if err := rows.Scan(
			&p.ID, &p.Source, &p.RepoID, &p.Number, &p.Title, &p.State,
			&p.AuthorID, &p.AuthorLogin, &p.HeadBranch, &p.BaseBranch,
			&p.Additions, &p.Deletions, &p.ChangedFiles, &p.JiraKey,
			&p.CreatedAt, &p.FirstCommitAt, &p.FirstReviewAt, &p.FirstHumanReviewAt,
			&p.MergedAt, &p.ClosedAt, &p.FetchedAt,
		); err != nil {
			return nil, fmt.Errorf("scan PR: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *genericStore) UpdatePRFirstCommitAt(ctx context.Context, id string, firstCommitAt *time.Time) error {
	q := rebind(s.d, `UPDATE pull_requests SET first_commit_at = ? WHERE id = ?`)
	_, err := s.db.ExecContext(ctx, q, firstCommitAt, id)
	if err != nil {
		return fmt.Errorf("update first_commit_at for %s: %w", id, err)
	}
	return nil
}

func (s *genericStore) UpsertPRReviews(ctx context.Context, reviews []PRReview) error {
	if len(reviews) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	q := rebind(s.d, `
		INSERT INTO pr_reviews (id, pr_id, source, reviewer_id, reviewer_login, state, submitted_at, is_bot)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			source = excluded.source,
			reviewer_id = excluded.reviewer_id,
			reviewer_login = excluded.reviewer_login,
			state = excluded.state,
			submitted_at = excluded.submitted_at,
			is_bot = excluded.is_bot`)
	stmt, err := tx.PrepareContext(ctx, q)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, r := range reviews {
		source := r.Source
		if source == "" {
			source = "github"
		}
		isBot := 0
		if r.IsBot {
			isBot = 1
		}
		_, err := stmt.ExecContext(ctx, r.ID, r.PRID, source, nullable(r.ReviewerID),
			nullable(r.ReviewerLogin), r.State, r.SubmittedAt, isBot)
		if err != nil {
			return fmt.Errorf("upsert review %s: %w", r.ID, err)
		}
	}
	return tx.Commit()
}

// ----------------------------------------------------------------------
// members
// ----------------------------------------------------------------------

// UpsertMember writes one member row. On conflict (existing id), Jira-
// derived fields (display_name / email / jira_account_id / active) are
// always overwritten with the latest value, while *operator-set* fields
// (github_login, gitlab_username, pillar_id) are preserved when the
// incoming row has them empty. Without this guard, the Jira sync
// goroutine — which never reads those columns — would clobber them
// every 30 minutes, silently undoing any PATCH on
// /api/contributions/members/:id. The PATCH endpoint goes through
// SetMemberIdentity so its writes still land (including intentional
// clears).
func (s *genericStore) UpsertMember(ctx context.Context, m Member) error {
	now := time.Now().UTC()
	if m.CreatedAt.IsZero() {
		m.CreatedAt = now
	}
	m.UpdatedAt = now
	active := 0
	if m.Active {
		active = 1
	}
	q := rebind(s.d, `
		INSERT INTO members (id, display_name, email, jira_account_id, github_login, gitlab_username, active, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			display_name = excluded.display_name,
			email = excluded.email,
			jira_account_id = excluded.jira_account_id,
			github_login = COALESCE(NULLIF(excluded.github_login, ''), members.github_login),
			gitlab_username = COALESCE(NULLIF(excluded.gitlab_username, ''), members.gitlab_username),
			active = excluded.active,
			updated_at = excluded.updated_at`)
	_, err := s.db.ExecContext(ctx, q,
		m.ID, m.DisplayName, nullable(m.Email),
		nullable(m.JiraAccountID), nullable(m.GitHubLogin), nullable(m.GitLabUsername),
		active, m.CreatedAt, m.UpdatedAt)
	return err
}

// SetMemberIdentity is the literal-overwrite counterpart to UpsertMember.
// Used by the PATCH endpoint, where explicit clears are meaningful.
// All fields land verbatim (empty string → NULL on the column);
// updated_at is bumped.
func (s *genericStore) SetMemberIdentity(ctx context.Context, id string, ident MemberIdentity) error {
	q := rebind(s.d, `
		UPDATE members
		   SET display_name = ?, github_login = ?, gitlab_username = ?, updated_at = ?
		 WHERE id = ?`)
	res, err := s.db.ExecContext(ctx, q,
		ident.DisplayName, nullable(ident.GitHubLogin), nullable(ident.GitLabUsername),
		time.Now().UTC(), id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func (s *genericStore) ListMembers(ctx context.Context) ([]Member, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, display_name, COALESCE(email, ''), COALESCE(jira_account_id, ''),
		       COALESCE(github_login, ''), COALESCE(gitlab_username, ''),
		       active, created_at, updated_at
		FROM members
		ORDER BY display_name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Member
	for rows.Next() {
		var m Member
		var active int
		if err := rows.Scan(&m.ID, &m.DisplayName, &m.Email, &m.JiraAccountID,
			&m.GitHubLogin, &m.GitLabUsername, &active, &m.CreatedAt, &m.UpdatedAt); err != nil {
			return nil, err
		}
		m.Active = active != 0
		out = append(out, m)
	}
	return out, rows.Err()
}

// ----------------------------------------------------------------------
// member_week_metrics — read path
// ----------------------------------------------------------------------

func (s *genericStore) MemberWeekMetrics(ctx context.Context, q MemberWeekQuery) ([]MemberWeekRow, error) {
	conds := []string{"week_start >= ?", "week_start <= ?"}
	args := []interface{}{q.From, q.To}

	if len(q.MemberIDs) > 0 {
		ph := make([]string, len(q.MemberIDs))
		for i, id := range q.MemberIDs {
			ph[i] = "?"
			args = append(args, id)
		}
		conds = append(conds, "member_id IN ("+strings.Join(ph, ",")+")")
	}
	if len(q.PillarIDs) > 0 {
		ph := make([]string, len(q.PillarIDs))
		for i, id := range q.PillarIDs {
			ph[i] = "?"
			args = append(args, id)
		}
		conds = append(conds, "pillar_id IN ("+strings.Join(ph, ",")+")")
	}
	if q.Component != "" {
		conds = append(conds, "component = ?")
		args = append(args, q.Component)
	}

	query := rebind(s.d, `
		SELECT member_id, week_start, pillar_id, component,
		       jira_issues_done, jira_points_done, prs_merged, prs_opened, prs_reviewed,
		       review_latency_p50_hours
		FROM member_week_metrics
		WHERE `+strings.Join(conds, " AND ")+`
		ORDER BY week_start, member_id`)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MemberWeekRow
	for rows.Next() {
		var r MemberWeekRow
		var lat sql.NullFloat64
		if err := rows.Scan(&r.MemberID, &r.WeekStart, &r.PillarID, &r.Component,
			&r.JiraIssuesDone, &r.JiraPointsDone, &r.PRsMerged, &r.PRsOpened, &r.PRsReviewed, &lat); err != nil {
			return nil, err
		}
		if lat.Valid {
			v := lat.Float64
			r.ReviewLatencyP50Hours = &v
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// nullable converts an empty Go string to a NULL on the wire.
func nullable(s string) interface{} {
	if s == "" {
		return nil
	}
	return s
}
