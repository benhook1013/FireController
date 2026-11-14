package reviewexec

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviewprovider"
)

// Merged GitHub PRs cannot reopen. Reuse repository-bound terminal facts, not
// a cached ready front. Local obligations are still evaluated on every pass.
// Closed-unmerged and open PRs never acquire this exemption.
func readRunHistory(ctx context.Context, tx *sql.Tx, project string, repo int64, queue []int64) (map[int64]SourcePlan, error) {
	return readRunHistoryHints(ctx, tx, project, repo, queue, nil)
}

// knownLive only narrows speculative concurrency. It never supplies source identity,
// eligibility, or coverage: every ordered live member still gets a fresh read.
func readRunHistoryHints(ctx context.Context, tx *sql.Tx, project string, repo int64, queue []int64, knownLive map[int64]bool) (map[int64]SourcePlan, error) {
	out := map[int64]SourcePlan{}
	var hasProjection bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_schema WHERE type='table' AND name='review_current_pulls')`).Scan(&hasProjection); err != nil {
		return nil, err
	}
	visible := map[int64]bool{}
	if hasProjection && len(queue) > 0 {
		args := []any{project, repo}
		for _, n := range queue {
			args = append(args, n)
		}
		rows, err := tx.QueryContext(ctx, `SELECT c.number,v.state,v.head_sha,v.base_sha,v.base_ref,s.observed_at,
   EXISTS(SELECT 1 FROM review_current_retired r WHERE r.project_id=c.project_id AND r.number=c.number)
   FROM review_current_pulls c CROSS JOIN review_pull_memberships m ON m.project_id=c.project_id AND m.number=c.number AND m.start_revision=c.start_revision
   CROSS JOIN review_pull_versions v ON v.project_id=m.project_id AND v.number=m.number AND v.version_revision=m.version_revision
   CROSS JOIN review_snapshots s ON s.project_id=v.project_id AND s.revision=v.version_revision
   WHERE c.project_id=? AND s.repository_id=? AND c.number IN (`+strings.TrimSuffix(strings.Repeat("?,", len(queue)), ",")+`)`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var n int64
			var state, head, base, ref, observed string
			var retired bool
			if err = rows.Scan(&n, &state, &head, &base, &ref, &observed, &retired); err != nil {
				break
			}
			visible[n] = true
			if knownLive != nil && state == "open" {
				knownLive[n] = true
			}
			if state != "merged" || retired || !sha.MatchString(head) || !sha.MatchString(base) || ref == "" || !safeText(ref, 256) || !roundTime(observed) {
				continue
			}
			out[n] = SourcePlan{Repository: reviewprovider.Repository{ID: repo}, Pull: reviewprovider.Pull{RepositoryID: repo, Number: n, State: "closed", Merged: true, HeadSHA: head, BaseSHA: base, BaseRef: ref}, ParentSHA: base, ObservedAt: observed, retainedMerged: true}
		}
		if err == nil {
			err = rows.Err()
		}
		rows.Close()
		if err != nil {
			return nil, err
		}
	}
	// Unimported historical predecessors may exist only in the bounded source
	// frame. Never let that older frame override a current workspace correction.
	latest := map[int64]time.Time{}
	for _, channel := range []string{"cli", "hosted"} {
		frame, found, err := readFrontFrame(ctx, tx, project, channel)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			// This optional cache can only save reads. Invalid cached display
			// facts must not prevent fresh selection or couple the channels.
			continue
		}
		if !found || frame.repository != repo {
			continue
		}
		for _, p := range frame.plans() {
			n := p.Pull.Number
			if visible[n] || p.schedulingPaused || !slices.Contains(queue, n) {
				continue
			}
			checked, _ := time.Parse(time.RFC3339Nano, p.ObservedAt)
			if previous, ok := latest[n]; ok && !checked.After(previous) {
				continue
			}
			latest[n] = checked
			delete(out, n)
			if !p.Pull.Merged {
				continue
			}
			if hasProjection {
				var retired bool
				if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM review_current_retired WHERE project_id=? AND number=?)`, project, n).Scan(&retired); err != nil {
					return nil, err
				}
				if retired {
					continue
				}
			}
			// A predecessor previously visible cannot silently survive its removal.
			for _, fp := range frame.prefix {
				if fp.Number == n && fp.VisibleAtAcquisition {
					p = SourcePlan{}
					break
				}
			}
			if p.Pull.Number == 0 {
				continue
			}
			p.retainedMerged = true
			out[n] = p
		}
	}
	return out, nil
}

func sameRunHistory(a, b SourcePlan) bool {
	return a.Repository.ID == b.Repository.ID && a.Pull.Number == b.Pull.Number && a.Pull.State == b.Pull.State && a.Pull.Merged == b.Pull.Merged && a.Pull.HeadSHA == b.Pull.HeadSHA && a.Pull.BaseSHA == b.Pull.BaseSHA && a.Pull.BaseRef == b.Pull.BaseRef
}

func runHistoryCurrent(ctx context.Context, tx *sql.Tx, r Run, repo int64, c Configuration, choice runChoice) (bool, error) {
	var history map[int64]SourcePlan
	if slices.ContainsFunc(choice.prefix, func(p SourcePlan) bool { return p.retainedMerged }) {
		var err error
		history, err = readRunHistory(ctx, tx, r.ProjectID, repo, c.Queue)
		if err != nil {
			return false, err
		}
	}
	for _, plan := range choice.prefix {
		if plan.retainedMerged && !sameRunHistory(plan, history[plan.Pull.Number]) {
			return false, nil
		}
		if plan.schedulingPaused {
			pause, err := readPullScheduling(ctx, tx, r.ProjectID, plan.Pull.Number)
			if err != nil {
				return false, err
			}
			if !pause.Paused {
				return false, nil
			}
		}
	}
	return true, nil
}

// Paused entries have no provider facts. Resolve only a paused/retained entry
// actually named by a fresh comparison, including chains and earlier parents.
func (e *Engine) resolveRunParents(ctx context.Context, source *runSourceBudget, reads *runReads, repo int64, c Configuration, channel string, prefix []SourcePlan) error {
	for {
		replaced := false
		for _, child := range prefix {
			if child.ParentPullNumber == 0 {
				continue
			}
			for i, parent := range prefix {
				if parent.Pull.Number != child.ParentPullNumber || !parent.schedulingPaused && !parent.retainedMerged {
					continue
				}
				plan, err := readRunPlan(ctx, source, reads, repo, c, channel, parent.Pull.Number)
				if err != nil {
					return err
				}
				prefix[i] = plan
				replaced = true
			}
		}
		if !replaced {
			return nil
		}
	}
}

func readRunPlan(ctx context.Context, source *runSourceBudget, reads *runReads, repo int64, c Configuration, channel string, n int64) (SourcePlan, error) {
	var repository reviewprovider.Repository
	var pull reviewprovider.Pull
	err := source.read(ctx, func(ctx context.Context) error {
		var err error
		repository, err = reads.Repository(ctx, repo)
		if err != nil {
			return err
		}
		pull, err = reads.Pull(ctx, repository.FullName, n)
		return err
	})
	if err != nil {
		return SourcePlan{}, err
	}
	if !sameTarget(pull, repo, n) || pull.State != "open" && pull.State != "closed" || pull.Merged && pull.State != "closed" {
		return SourcePlan{}, sourceHold("source_response_invalid")
	}
	plan := SourcePlan{Repository: repository, Pull: pull, ParentSHA: pull.BaseSHA, ObservedAt: stamp()}
	if !pull.Merged && pull.State == "open" {
		err = source.read(ctx, func(ctx context.Context) error {
			var err error
			plan, err = ResolveSource(ctx, reads, repo, c, Request{PullNumber: n, HeadSHA: pull.HeadSHA, Channel: channel})
			return err
		})
		if err != nil {
			reason := runReason(err)
			var refusal *core.Error
			if !errors.As(err, &refusal) || refusal.Code != "review_held" || !perPullSourceHold(reason) {
				return SourcePlan{}, err
			}
			plan = SourcePlan{Repository: repository, Pull: pull, ParentSHA: pull.BaseSHA, ObservedAt: stamp(), SourceReason: reason}
		}
	}
	return plan, nil
}
