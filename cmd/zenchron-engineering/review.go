package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"

	"github.com/bogdaniel/zenchron-engineering/review"
	"github.com/bogdaniel/zenchron-engineering/runtime"
)

const reviewUsage = "usage: zenchron-engineering autonomy review " +
	"{pr <number> --agent <id> [--publish]|status <number> [--text]} [--repo owner/name] [--config <path>]"

// autonomyReview is the first-class independent PR review operation (#233):
// `review pr <number> --agent <id>` builds the complete review context
// automatically, invokes the named agent as an independent reviewer through
// the existing execution Port, and records a durable decision - without the
// operator manually gathering the run, candidate, evidence, CI state and
// feedback history into a hand-assembled prompt.
func autonomyReview(ctx context.Context, args []string, overrides autonomyOverrides, stdout io.Writer) (int, error) {
	if len(args) < 2 {
		return runtime.ExitInvalid, errors.New(reviewUsage)
	}
	switch args[0] {
	case "pr":
		number, err := strconv.Atoi(args[1])
		if err != nil || number <= 0 {
			return runtime.ExitInvalid, fmt.Errorf("pull request number must be a positive integer, got %q", args[1])
		}
		flags, err := parseAutonomyFlags(args[2:])
		if err != nil {
			return runtime.ExitInvalid, err
		}
		if flags.Agent == "" {
			return runtime.ExitInvalid, errors.New("an independent review names its reviewing agent explicitly: pass --agent <id>")
		}
		return reviewPR(ctx, number, flags, overrides, stdout)
	case "status":
		number, err := strconv.Atoi(args[1])
		if err != nil || number <= 0 {
			return runtime.ExitInvalid, fmt.Errorf("pull request number must be a positive integer, got %q", args[1])
		}
		flags, err := parseAutonomyFlags(args[2:])
		if err != nil {
			return runtime.ExitInvalid, err
		}
		return reviewStatus(number, flags, overrides, stdout)
	}
	return runtime.ExitInvalid, errors.New(reviewUsage)
}

// reviewPR drives one independent review directly in this process (#233's
// first version; routing this through a running supervisor the way
// `orchestrate`/`workgraph` do is an outstanding integration dependency, not
// solved here - see the PR description).
func reviewPR(ctx context.Context, number int, flags autonomyFlags, overrides autonomyOverrides, stdout io.Writer) (int, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return runtime.ExitFailed, err
	}
	target, err := repositoryTarget(cwd, flags.Repo)
	if err != nil {
		return runtime.ExitInvalid, err
	}
	built, err := newComposition(flags, overrides)
	if err != nil {
		return runtime.ExitInvalid, err
	}
	defer built.release()
	engine, err := built.engine(target)
	if err != nil {
		return runtime.ExitInvalid, err
	}
	repo, err := runtime.ParseGitHubRepo(target.Identity)
	if err != nil {
		return runtime.ExitInvalid, err
	}
	out, err := runtime.RunIndependentReview(ctx, runtime.RunIndependentReviewInput{
		Repo: repo, PRNumber: number, Reviewer: built.agent, Provider: built.provider,
		ResolveAgent: built.agents.Agent, Store: built.store, GitHub: built.forge,
		StateDir: built.config.StateDir, ControllerID: engine.ControllerIdentityID(),
		Source: cwd, Clock: runtime.RealClock{}, Publish: flags.Publish,
	})
	if err != nil {
		// The review itself may have durably succeeded even though this call
		// still fails overall (publication failed after the decision was
		// already recorded): the decision is not lost, so show it rather than
		// leaving the operator with only an opaque error and no indication
		// that `review status` already has something to read back.
		if out.Decision.ID != "" {
			if flags.Text {
				renderReviewDecision(stdout, out.Decision, out.Created)
			} else {
				writeJSON(stdout, out)
			}
		}
		return runtime.ExitFailed, err
	}
	if !flags.Text {
		return runtime.ExitCompleted, writeJSON(stdout, out)
	}
	renderReviewDecision(stdout, out.Decision, out.Created)
	if out.Publication != nil {
		fmt.Fprintf(stdout, "published: %v (verdict sent: %s)\n", out.Publication.Published, out.Publication.PublishedVerdict)
	} else {
		fmt.Fprintln(stdout, "published: false (pass --publish to authorize GitHub publication)")
	}
	return runtime.ExitCompleted, nil
}

// reviewStatus answers #233's status/observability questions - current
// decision, blocking/non-blocking finding counts, staleness - without
// performing a review. It takes no run lock: it only reads durable state.
func reviewStatus(number int, flags autonomyFlags, overrides autonomyOverrides, stdout io.Writer) (int, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return runtime.ExitFailed, err
	}
	target, err := repositoryTarget(cwd, flags.Repo)
	if err != nil {
		return runtime.ExitInvalid, err
	}
	built, err := newComposition(flags, overrides)
	if err != nil {
		return runtime.ExitInvalid, err
	}
	defer built.release()
	repo, err := runtime.ParseGitHubRepo(target.Identity)
	if err != nil {
		return runtime.ExitInvalid, err
	}
	port := &runtime.SupervisorReviewPort{Store: built.store, GitHub: built.forge}
	decision, found, err := port.LatestDecision(repo, number)
	if err != nil {
		return runtime.ExitFailed, err
	}
	if !found {
		if !flags.Text {
			return runtime.ExitCompleted, writeJSON(stdout, map[string]any{"found": false})
		}
		fmt.Fprintf(stdout, "pull request #%d: no independent review has been reached yet\n", number)
		return runtime.ExitCompleted, nil
	}
	stale, err := port.IsStale(context.Background(), repo, number)
	if err != nil {
		return runtime.ExitFailed, err
	}
	publication, publicationFound, err := built.store.ReviewPublication(decision.ID)
	if err != nil {
		return runtime.ExitFailed, err
	}
	if !flags.Text {
		return runtime.ExitCompleted, writeJSON(stdout, map[string]any{
			"found": true, "decision": decision, "stale": stale,
			"published": publicationFound && publication.Published,
		})
	}
	renderReviewDecision(stdout, decision, false)
	fmt.Fprintf(stdout, "stale: %v\n", stale)
	fmt.Fprintf(stdout, "published: %v\n", publicationFound && publication.Published)
	return runtime.ExitCompleted, nil
}

func renderReviewDecision(stdout io.Writer, decision review.Decision, created bool) {
	blocking, nonBlocking := 0, 0
	for _, finding := range decision.Findings {
		if finding.Severity == review.SeverityBlocking {
			blocking++
		} else {
			nonBlocking++
		}
	}
	fmt.Fprintf(stdout, "review %s: verdict=%s head=%s reviewer=%s producer=%s blocking=%d non_blocking=%d created=%v\n",
		decision.ID, decision.Verdict, shortSHA(decision.Subject.HeadSHA), decision.ReviewerAgentID, decision.ProducerAgentID,
		blocking, nonBlocking, created)
	if decision.Reason != "" {
		fmt.Fprintf(stdout, "reason: %s\n", decision.Reason)
	}
}

func shortSHA(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}
