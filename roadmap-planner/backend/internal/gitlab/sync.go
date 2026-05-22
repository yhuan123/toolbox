/*
Copyright 2026 The AlaudaDevops Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
*/

package gitlab

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/AlaudaDevops/toolbox/roadmap-planner/backend/internal/logger"
	"github.com/AlaudaDevops/toolbox/roadmap-planner/backend/internal/storage"
	"go.uber.org/zap"
)

// Linker resolves an MR to a Jira epic key. Mirrors the github.Linker
// interface but operates on a GitLab MergeRequest.
type Linker interface {
	Link(mr MergeRequest) string
}

// DefaultLinker matches against the source branch and the MR title.
//
// Compiled regexes are memoised by project-key so repeated DefaultLinker
// calls (e.g. across tests) skip the recompilation. The cache is small —
// one entry per Jira project key the process touches in its lifetime.
var (
	defaultLinkerCache = map[string]*defaultLinker{}
	defaultLinkerMu    sync.Mutex
)

func DefaultLinker(projectKey string) Linker {
	key := strings.ToUpper(projectKey)
	defaultLinkerMu.Lock()
	defer defaultLinkerMu.Unlock()
	if l, ok := defaultLinkerCache[key]; ok {
		return l
	}
	l := &defaultLinker{
		key: key,
		re:  regexp.MustCompile(`(?i)\b(` + regexp.QuoteMeta(projectKey) + `-\d+)\b`),
	}
	defaultLinkerCache[key] = l
	return l
}

type defaultLinker struct {
	key string
	re  *regexp.Regexp
}

func (d *defaultLinker) Link(mr MergeRequest) string {
	for _, candidate := range []string{mr.SourceBranch, mr.Title} {
		if m := d.re.FindString(candidate); m != "" {
			return strings.ToUpper(m)
		}
	}
	return ""
}

// Syncer pulls MRs and notes-as-reviews into the storage.Store. Mirrors
// the github.Syncer shape: one process-wide instance, run on a ticker.
//
// Reviews from notes:
//
//	GitLab has no first-class review event. We treat any non-system,
//	non-author MR note as a review touch. Bodies that match
//	approvalRegex (`/lgtm` on its own line) are stored as state="approved";
//	anything else as state="commented". Procedural prow commands
//	(/retest, /hold, /cherry-pick, /uncc, /assign, /unassign, /label,
//	/milestone, /retitle, /priority, /kind, /area, /sig) are skipped
//	entirely — they're noise on the dashboard.
type Syncer struct {
	client       *Client
	store        storage.Store
	specs        []GroupSpec
	linker       Linker
	logger       *zap.Logger
	backfillDays int

	WildcardTTL     time.Duration
	IncludeArchived bool
	// HydrateDiff controls whether merged MRs get a follow-up show-MR
	// call to populate additions/deletions/changed_files. Default true
	// since W6 (2026-05-19) — the data is cheap (~1 extra call per
	// merged MR, ~+2% of the per-cycle API budget) and the Dashboard
	// tab consumes it. Operators on tight rate budgets can disable via
	// `gitlab.hydrate_diff: false`.
	HydrateDiff bool

	// MemberInstanceSweep enables Pass B (W1): after the project-scoped
	// fetch finishes, sweep `/merge_requests?scope=all&author_username=<u>`
	// for each Jira member whose GitLab username we know. Captures MRs
	// our members file in projects that aren't on the configured
	// gitlab.groups list. Off by default; main.go flips it on when
	// gitlab.member_instance_sweep is true. No-op when AllowedMemberIDs
	// is empty.
	MemberInstanceSweep bool

	// AllowedMemberIDs is the W1 set that Pass B walks. Each id must be
	// a `members.id` (slugified email). Pass B looks up the matching
	// `members.gitlab_username` and calls
	// `/merge_requests?scope=all&author_username=<u>`. Nil/empty
	// disables Pass B entirely.
	AllowedMemberIDs map[string]struct{}

	// IsBotLogin is the W2 predicate that reclassifies a raw GitLab
	// username as the synthetic `bot` member. Mirrors the github
	// syncer's field; see that doc for behavior. Nil disables.
	IsBotLogin func(login string) bool

	wildcardCache *wildcardCache
	nowFn         func() time.Time
}

// NewSyncer builds a Syncer. backfillDays sets the first-run window;
// pass 0 to use the package default (180).
func NewSyncer(client *Client, store storage.Store, specs []GroupSpec, linker Linker, backfillDays int) *Syncer {
	if backfillDays <= 0 {
		backfillDays = 180
	}
	return &Syncer{
		client:        client,
		store:         store,
		specs:         specs,
		linker:        linker,
		logger:        logger.WithComponent("gitlab-syncer"),
		backfillDays:  backfillDays,
		WildcardTTL:   DefaultWildcardTTL,
		HydrateDiff:   true,
		wildcardCache: newWildcardCache(),
		nowFn:         time.Now,
	}
}

// approvalRegex matches a /lgtm body that's the entire line (allows
// trailing whitespace). Some users write "/lgtm cancel" — we don't treat
// that as approval.
var approvalRegex = regexp.MustCompile(`(?m)^\s*/lgtm\s*$`)

// procedureRegex matches prow commands we want to drop entirely (so
// they don't inflate the "PRs reviewed" count). The list mirrors what
// alauda's prow-style flow uses.
var procedureRegex = regexp.MustCompile(`(?m)^\s*/(retest|hold|cherry-pick|uncc|cc|assign|unassign|label|remove-label|milestone|retitle|priority|kind|area|sig|approve|close|reopen|wip)(\b.*)?$`)

// classifyNote returns the review state to store for a non-system note,
// or ("", false) if the note should be skipped entirely.
//
// Rules (in priority order):
//   - body contains an `/lgtm` line → "approved"
//   - body is purely a procedural prow command → skip
//   - empty/whitespace body → skip
//   - otherwise → "commented"
func classifyNote(body string) (string, bool) {
	if approvalRegex.MatchString(body) {
		return "approved", true
	}
	trimmed := strings.TrimSpace(body)
	if trimmed == "" {
		return "", false
	}
	// A note that's exclusively prow commands (one or more lines, no
	// substantive content) is procedural. We approximate "exclusively"
	// by stripping all matching procedure lines and checking what's
	// left; if there's still text after the strip, treat it as a
	// regular comment.
	stripped := procedureRegex.ReplaceAllString(body, "")
	if strings.TrimSpace(stripped) == "" {
		return "", false
	}
	return "commented", true
}

// firstCommitBackfillBatch caps how many historical MRs are rehydrated
// per Sync cycle. See github.firstCommitBackfillBatch for the rationale
// — same budget reasoning applies: keep the post-0009 catch-up bounded
// so it never starves the regular incremental sync.
const firstCommitBackfillBatch = 200

// Sync runs one cycle: expand specs → list MRs updated since the
// previous run → upsert PRs + notes-as-reviews → write a collection_run.
//
// The shape mirrors github.Syncer.Sync — see those comments for first-
// run vs. incremental behaviour. Sync also runs a bounded backfill of
// `first_commit_at` for already-merged MRs that the incremental window
// can never revisit (rows from before migration 0009). Without this the
// Lead Time calculator under-attributes the Dev stage on upgraded
// deployments — see backfillFirstCommit.
func (s *Syncer) Sync(ctx context.Context) error {
	passBActive := s.MemberInstanceSweep && len(s.AllowedMemberIDs) > 0
	if len(s.specs) == 0 && !passBActive {
		return nil
	}

	// Best-effort: backfill errors must not block the regular sync.
	if err := s.backfillFirstCommit(ctx); err != nil {
		s.logger.Warn("first_commit_at backfill failed (continuing with incremental sync)", zap.Error(err))
	}

	now := s.nowFn()
	var resolved []Project
	var resolveErr error
	if len(s.specs) > 0 {
		resolved, resolveErr = s.resolveProjects(ctx, now)
		if resolveErr != nil {
			s.logger.Warn("group spec resolution failed (partial)", zap.Error(resolveErr))
		}
	}
	if len(resolved) == 0 && !passBActive {
		if resolveErr != nil {
			return resolveErr
		}
		return nil
	}

	since := time.Now().AddDate(0, 0, -s.backfillDays)
	mode := "backfill"
	if last, err := s.store.LatestCollectionRun(ctx, "gitlab"); err == nil && last != nil {
		since = last.CapturedAt.Add(-1 * time.Hour)
		mode = "incremental"
	}
	s.logger.Info("gitlab sync starting",
		zap.String("mode", mode),
		zap.Time("since", since),
		zap.Int("specs", len(s.specs)),
		zap.Int("projects", len(resolved)))

	runStart := time.Now()
	runID := fmt.Sprintf("gl-%d", runStart.UnixNano())
	totalMRs, totalReviews := 0, 0
	firstErr := resolveErr

	members, err := s.store.ListMembers(ctx)
	if err != nil {
		return err
	}
	byUsername := make(map[string]string, len(members))
	for _, m := range members {
		if m.GitLabUsername != "" {
			byUsername[strings.ToLower(m.GitLabUsername)] = m.ID
		}
	}

	// Track every storage id we've already written this cycle so Pass B
	// can skip MRs Pass A already covered. Cheaper than asking the
	// store after each upsert; uses the same key shape as the storage
	// PK ("<group/proj>!<iid>").
	seenIDs := make(map[string]struct{}, 256)

	for _, p := range resolved {
		mrs, err := s.client.ListMergeRequests(ctx, p.ID, ListMergeRequestsOptions{
			State: "all", UpdatedAfter: since, MaxPages: 10,
		})
		if err != nil {
			s.logger.Warn("ListMergeRequests failed", zap.String("project", p.PathWithNamespace), zap.Error(err))
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		s.logger.Debug("Fetched MRs", zap.String("project", p.PathWithNamespace), zap.Int("count", len(mrs)))

		toStore := make([]storage.PullRequest, 0, len(mrs))
		reviewBatch := make([]storage.PRReview, 0)

		for _, mr := range mrs {
			state := "open"
			if mr.MergedAt != nil {
				state = "merged"
			} else if mr.State == "closed" || mr.State == "locked" {
				state = "closed"
			}
			authorLogin := strings.ToLower(mr.Author.Username)
			authorID := byUsername[authorLogin]
			if s.IsBotLogin != nil && s.IsBotLogin(authorLogin) {
				authorID = "bot"
			}
			epicKey := ""
			if s.linker != nil {
				epicKey = s.linker.Link(mr)
			}
			id := fmt.Sprintf("%s!%d", p.PathWithNamespace, mr.IID)
			rec := storage.PullRequest{
				ID:          id,
				Source:      "gitlab",
				RepoID:      p.PathWithNamespace,
				Number:      mr.IID,
				Title:       mr.Title,
				State:       state,
				AuthorID:    authorID,
				AuthorLogin: authorLogin,
				HeadBranch:  mr.SourceBranch,
				BaseBranch:  mr.TargetBranch,
				JiraKey:     epicKey,
				CreatedAt:   mr.CreatedAt,
				MergedAt:    mr.MergedAt,
				ClosedAt:    mr.ClosedAt,
				FetchedAt:   runStart,
			}
			// Optional diff hydration. Off by default — turn on once
			// the dashboard wants additions/deletions on MRs.
			if s.HydrateDiff && mr.MergedAt != nil {
				if full, err := s.client.GetMergeRequest(ctx, p.ID, mr.IID); err == nil {
					rec.Additions = full.Diff.Additions
					rec.Deletions = full.Diff.Deletions
					rec.ChangedFiles = full.Diff.ChangedFiles
				}
			}

			// Notes → reviews. Same skip rules as the github side:
			// drop drafts, drop closed-without-merge older than 7d.
			skipMRAPIs := mr.Draft || mr.WorkInProg
			if !skipMRAPIs && mr.MergedAt == nil && mr.ClosedAt != nil &&
				time.Since(*mr.ClosedAt) > 7*24*time.Hour {
				skipMRAPIs = true
			}
			if mr.MergedAt != nil {
				// Commits → first_commit_at. Only merged MRs are read
				// by the Lead Time calculator (filterPreReleasePRs
				// drops MergedAt==nil) and ListPullRequestsSince
				// filters merged_at IS NOT NULL — fetching commits for
				// open / closed-unmerged MRs is pure API budget burn,
				// especially on Pass B sweeps over large GitLab
				// instances. Failures are logged but never block the
				// MR upsert; backfillFirstCommit catches up later.
				commits, err := s.client.ListMRCommits(ctx, p.ID, mr.IID)
				if err != nil {
					s.logger.Warn("ListMRCommits failed",
						zap.String("project", p.PathWithNamespace),
						zap.Int("iid", mr.IID), zap.Error(err))
				} else {
					var firstCommit *time.Time
					for _, cm := range commits {
						d := cm.AuthoredDate
						if firstCommit == nil || d.Before(*firstCommit) {
							firstCommit = &d
						}
					}
					rec.FirstCommitAt = firstCommit
				}
			}
			if !skipMRAPIs {
				notes, err := s.client.ListMRNotes(ctx, p.ID, mr.IID)
				if err != nil {
					s.logger.Warn("ListMRNotes failed",
						zap.String("project", p.PathWithNamespace),
						zap.Int("iid", mr.IID), zap.Error(err))
				} else {
					var first, firstHuman *time.Time
					for _, n := range notes {
						if n.System {
							continue
						}
						reviewerLogin := strings.ToLower(n.Author.Username)
						if reviewerLogin == authorLogin {
							continue
						}
						st, ok := classifyNote(n.Body)
						if !ok {
							continue
						}
						reviewerID := byUsername[reviewerLogin]
						isBot := s.IsBotLogin != nil && s.IsBotLogin(reviewerLogin)
						if isBot {
							reviewerID = "bot"
						}
						reviewBatch = append(reviewBatch, storage.PRReview{
							ID:            fmt.Sprintf("%s!%d/n%d", p.PathWithNamespace, mr.IID, n.ID),
							PRID:          rec.ID,
							Source:        "gitlab",
							ReviewerID:    reviewerID,
							ReviewerLogin: reviewerLogin,
							State:         st,
							SubmittedAt:   n.CreatedAt,
							IsBot:         isBot,
						})
						if first == nil || n.CreatedAt.Before(*first) {
							first = &n.CreatedAt
						}
						if !isBot && (firstHuman == nil || n.CreatedAt.Before(*firstHuman)) {
							firstHuman = &n.CreatedAt
						}
					}
					rec.FirstReviewAt = first
					rec.FirstHumanReviewAt = firstHuman
				}
			}
			toStore = append(toStore, rec)
		}

		if err := s.store.UpsertPullRequests(ctx, toStore); err != nil {
			return fmt.Errorf("upsert MRs for %s: %w", p.PathWithNamespace, err)
		}
		if err := s.store.UpsertPRReviews(ctx, reviewBatch); err != nil {
			return fmt.Errorf("upsert MR reviews for %s: %w", p.PathWithNamespace, err)
		}
		for _, r := range toStore {
			seenIDs[r.ID] = struct{}{}
		}
		totalMRs += len(toStore)
		totalReviews += len(reviewBatch)
	}

	// Pass B (W1): per-allowlisted-member instance-wide MR sweep.
	// Captures MRs our team filed in projects outside the configured
	// `gitlab.groups` list. De-dupes against Pass A via seenIDs.
	if s.MemberInstanceSweep && len(s.AllowedMemberIDs) > 0 {
		passBMRs, passBReviews, passBErr := s.sweepInstanceByMember(ctx, members, since, runStart, seenIDs)
		if passBErr != nil && firstErr == nil {
			firstErr = passBErr
		}
		totalMRs += passBMRs
		totalReviews += passBReviews
	}

	run := storage.CollectionRun{
		ID:          runID,
		CapturedAt:  runStart,
		Source:      "gitlab",
		DurationMs:  time.Since(runStart).Milliseconds(),
		RecordCount: totalMRs + totalReviews,
	}
	if firstErr != nil {
		run.Error = firstErr.Error()
	}
	if err := s.store.WriteCollectionRun(ctx, run); err != nil {
		return fmt.Errorf("write collection run: %w", err)
	}

	s.logger.Info("gitlab sync complete",
		zap.Int("mrs", totalMRs),
		zap.Int("reviews", totalReviews),
		zap.Duration("duration", time.Since(runStart)))
	return firstErr
}

// sweepInstanceByMember runs Pass B (W1): for each allowlisted member
// whose `members.gitlab_username` is populated, sweep
// `/merge_requests?scope=all&author_username=<u>` over the entire
// instance and upsert every MR not already covered by Pass A.
//
// `seenIDs` is mutated as we accept new rows so back-to-back members
// referencing the same MR (rare but possible — cross-author MRs are
// surfaced as the author's row only) don't double-upsert.
//
// Errors from any single member are logged and skipped: we don't want
// one bad ticker tick to abort the whole sweep. The first error is
// returned for the caller to bubble up into the collection_run.
func (s *Syncer) sweepInstanceByMember(
	ctx context.Context,
	members []storage.Member,
	since time.Time,
	runStart time.Time,
	seenIDs map[string]struct{},
) (int, int, error) {
	byID := make(map[string]storage.Member, len(members))
	byUsername := make(map[string]string, len(members))
	for _, m := range members {
		byID[m.ID] = m
		if m.GitLabUsername != "" {
			byUsername[strings.ToLower(m.GitLabUsername)] = m.ID
		}
	}

	totalMRs, totalReviews := 0, 0
	var firstErr error
	for memberID := range s.AllowedMemberIDs {
		m, ok := byID[memberID]
		if !ok {
			// Allowlisted member doesn't exist in the directory yet —
			// either the Jira sync hasn't created the row yet, or the
			// operator typo'd the denylist/prefill key. Either way Pass
			// B has nothing to fetch for them; skip silently.
			continue
		}
		if m.GitLabUsername == "" {
			continue
		}
		mrs, err := s.client.ListInstanceMergeRequests(ctx, ListInstanceMergeRequestsOptions{
			AuthorUsername: m.GitLabUsername,
			State:          "all",
			UpdatedAfter:   since,
			MaxPages:       20,
		})
		if err != nil {
			s.logger.Warn("Pass B: ListInstanceMergeRequests failed",
				zap.String("member", memberID),
				zap.String("gitlab_username", m.GitLabUsername),
				zap.Error(err))
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		s.logger.Debug("Pass B: fetched member MRs",
			zap.String("member", memberID), zap.Int("count", len(mrs)))

		toStore := make([]storage.PullRequest, 0, len(mrs))
		reviewBatch := make([]storage.PRReview, 0)
		for _, mr := range mrs {
			projectPath := mr.ProjectPath()
			if projectPath == "" {
				s.logger.Warn("Pass B: skipping MR with empty project path",
					zap.String("member", memberID), zap.Int("iid", mr.IID), zap.String("web_url", mr.WebURL))
				continue
			}
			id := fmt.Sprintf("%s!%d", projectPath, mr.IID)
			if _, dup := seenIDs[id]; dup {
				continue
			}

			state := "open"
			if mr.MergedAt != nil {
				state = "merged"
			} else if mr.State == "closed" || mr.State == "locked" {
				state = "closed"
			}
			authorLogin := strings.ToLower(mr.Author.Username)
			epicKey := ""
			if s.linker != nil {
				epicKey = s.linker.Link(mr)
			}
			rec := storage.PullRequest{
				ID:          id,
				Source:      "gitlab",
				RepoID:      projectPath,
				Number:      mr.IID,
				Title:       mr.Title,
				State:       state,
				AuthorID:    byUsername[authorLogin],
				AuthorLogin: authorLogin,
				HeadBranch:  mr.SourceBranch,
				BaseBranch:  mr.TargetBranch,
				JiraKey:     epicKey,
				CreatedAt:   mr.CreatedAt,
				MergedAt:    mr.MergedAt,
				ClosedAt:    mr.ClosedAt,
				FetchedAt:   runStart,
			}
			if s.HydrateDiff && mr.MergedAt != nil && mr.ProjectID > 0 {
				if full, err := s.client.GetMergeRequest(ctx, mr.ProjectID, mr.IID); err == nil {
					rec.Additions = full.Diff.Additions
					rec.Deletions = full.Diff.Deletions
					rec.ChangedFiles = full.Diff.ChangedFiles
				}
			}

			skipMRAPIs := mr.Draft || mr.WorkInProg
			if !skipMRAPIs && mr.MergedAt == nil && mr.ClosedAt != nil &&
				time.Since(*mr.ClosedAt) > 7*24*time.Hour {
				skipMRAPIs = true
			}
			if mr.MergedAt != nil && mr.ProjectID > 0 {
				// Commits → first_commit_at. See Pass A comment: only
				// merged MRs are read by Lead Time, so non-merged MRs
				// would burn rate budget for data that is discarded.
				commits, err := s.client.ListMRCommits(ctx, mr.ProjectID, mr.IID)
				if err != nil {
					s.logger.Warn("Pass B: ListMRCommits failed",
						zap.String("project", projectPath),
						zap.Int("iid", mr.IID), zap.Error(err))
				} else {
					var firstCommit *time.Time
					for _, cm := range commits {
						d := cm.AuthoredDate
						if firstCommit == nil || d.Before(*firstCommit) {
							firstCommit = &d
						}
					}
					rec.FirstCommitAt = firstCommit
				}
			}
			if !skipMRAPIs && mr.ProjectID > 0 {
				notes, err := s.client.ListMRNotes(ctx, mr.ProjectID, mr.IID)
				if err != nil {
					s.logger.Warn("Pass B: ListMRNotes failed",
						zap.String("project", projectPath),
						zap.Int("iid", mr.IID), zap.Error(err))
				} else {
					var first *time.Time
					for _, n := range notes {
						if n.System {
							continue
						}
						reviewerLogin := strings.ToLower(n.Author.Username)
						if reviewerLogin == authorLogin {
							continue
						}
						st, ok := classifyNote(n.Body)
						if !ok {
							continue
						}
						reviewBatch = append(reviewBatch, storage.PRReview{
							ID:            fmt.Sprintf("%s!%d/n%d", projectPath, mr.IID, n.ID),
							PRID:          rec.ID,
							Source:        "gitlab",
							ReviewerID:    byUsername[reviewerLogin],
							ReviewerLogin: reviewerLogin,
							State:         st,
							SubmittedAt:   n.CreatedAt,
						})
						if first == nil || n.CreatedAt.Before(*first) {
							first = &n.CreatedAt
						}
					}
					rec.FirstReviewAt = first
				}
			}
			toStore = append(toStore, rec)
			seenIDs[id] = struct{}{}
		}

		if len(toStore) == 0 {
			continue
		}
		if err := s.store.UpsertPullRequests(ctx, toStore); err != nil {
			return totalMRs, totalReviews, fmt.Errorf("Pass B upsert MRs for member %s: %w", memberID, err)
		}
		if err := s.store.UpsertPRReviews(ctx, reviewBatch); err != nil {
			return totalMRs, totalReviews, fmt.Errorf("Pass B upsert MR reviews for member %s: %w", memberID, err)
		}
		totalMRs += len(toStore)
		totalReviews += len(reviewBatch)
	}
	return totalMRs, totalReviews, firstErr
}

// resolveProjects expands every spec, dropping archived projects when
// IncludeArchived is false. Duplicates across overlapping specs are
// dropped here so we don't waste API budget syncing the same project
// twice in one cycle.
func (s *Syncer) resolveProjects(ctx context.Context, now time.Time) ([]Project, error) {
	if len(s.specs) == 0 {
		return nil, nil
	}
	seen := make(map[int64]struct{}, 32)
	out := make([]Project, 0, 64)
	var firstErr error
	for _, spec := range s.specs {
		projects, err := s.expandSpec(ctx, spec, now)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		for _, p := range projects {
			if !s.IncludeArchived && p.Archived {
				continue
			}
			if _, ok := seen[p.ID]; ok {
				continue
			}
			seen[p.ID] = struct{}{}
			out = append(out, p)
		}
	}
	return out, firstErr
}

// backfillFirstCommit fetches `first_commit_at` for at most
// firstCommitBackfillBatch already-merged MRs whose row predates
// migration 0009. Mirrors github.Syncer.backfillFirstCommit; the only
// twist is GitLab's commits endpoint needs a numeric project_id while
// the storage row only carries the namespace path, so we cache
// path → projectID per cycle to avoid re-resolving the same project
// for every MR.
func (s *Syncer) backfillFirstCommit(ctx context.Context) error {
	since := time.Now().AddDate(0, 0, -s.backfillDays)
	prs, err := s.store.ListMergedPRsMissingFirstCommit(ctx, "gitlab", since, firstCommitBackfillBatch)
	if err != nil {
		return err
	}
	if len(prs) == 0 {
		return nil
	}
	s.logger.Info("gitlab backfill first_commit_at",
		zap.Int("batch", len(prs)),
		zap.Int("backfill_days", s.backfillDays))

	projectIDCache := make(map[string]int64, len(prs))
	updated, failed := 0, 0
	for _, p := range prs {
		path, iid, ok := parseMRID(p.ID)
		if !ok {
			s.logger.Warn("backfill: unparseable MR id (skipping)", zap.String("mr_id", p.ID))
			failed++
			continue
		}
		// Prefer the stored RepoID — it's already the namespace path
		// and stays correct even if the MR id changed shape across
		// migrations. parseMRID is the fallback.
		if p.RepoID != "" {
			path = p.RepoID
		}
		projectID, ok := projectIDCache[path]
		if !ok {
			proj, err := s.client.GetProject(ctx, path)
			if err != nil {
				s.logger.Warn("backfill: GetProject failed",
					zap.String("mr_id", p.ID), zap.String("path", path), zap.Error(err))
				failed++
				continue
			}
			projectID = proj.ID
			projectIDCache[path] = projectID
		}
		commits, err := s.client.ListMRCommits(ctx, projectID, iid)
		if err != nil {
			s.logger.Warn("backfill: ListMRCommits failed",
				zap.String("mr_id", p.ID), zap.Error(err))
			failed++
			continue
		}
		var firstCommit *time.Time
		for _, cm := range commits {
			d := cm.AuthoredDate
			if firstCommit == nil || d.Before(*firstCommit) {
				firstCommit = &d
			}
		}
		if firstCommit == nil {
			continue
		}
		if err := s.store.UpdatePRFirstCommitAt(ctx, p.ID, firstCommit); err != nil {
			s.logger.Warn("backfill: UpdatePRFirstCommitAt failed",
				zap.String("mr_id", p.ID), zap.Error(err))
			failed++
			continue
		}
		updated++
	}
	s.logger.Info("gitlab backfill first_commit_at complete",
		zap.Int("updated", updated), zap.Int("failed", failed))
	return nil
}

// parseMRID splits a gitlab MR id of the form "group/sub/proj!iid".
// The project path may contain any number of "/" segments, so we split
// on the trailing "!" only. Returns ok=false on any structural mismatch.
func parseMRID(id string) (path string, iid int, ok bool) {
	bang := strings.LastIndexByte(id, '!')
	if bang <= 0 || bang == len(id)-1 {
		return "", 0, false
	}
	n, err := strconv.Atoi(id[bang+1:])
	if err != nil || n <= 0 {
		return "", 0, false
	}
	return id[:bang], n, true
}
