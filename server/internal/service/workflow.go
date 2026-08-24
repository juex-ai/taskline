package service

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"taskline_server/api/model"
)

var (
	// ErrStateEntryBlocked means the task is missing required workflow evidence.
	ErrStateEntryBlocked = errors.New("state entry blocked")
	// ErrStateEntryVerificationUnavailable means external evidence could not be checked.
	ErrStateEntryVerificationUnavailable = errors.New("state entry verification unavailable")
	// ErrPullRequestNotFound distinguishes an invalid PR artifact from an integration outage.
	ErrPullRequestNotFound = errors.New("pull request not found")
)

const (
	PullRequestOpen   = "OPEN"
	PullRequestClosed = "CLOSED"
	PullRequestMerged = "MERGED"

	CheckRollupSuccess  = "SUCCESS"
	CheckRollupPending  = "PENDING"
	CheckRollupFailure  = "FAILURE"
	CheckRollupError    = "ERROR"
	CheckRollupExpected = "EXPECTED"
)

// PullRequestRef is the canonical identity parsed from a linked GitHub PR.
type PullRequestRef struct {
	URL        string
	Owner      string
	Repository string
	Number     int
}

// PullRequestStatus contains only the external facts needed by workflow rules.
type PullRequestStatus struct {
	State                       string
	Merged                      bool
	UnresolvedP0P1ReviewThreads int
	CheckRollupState            string
}

// PullRequestVerifier keeps GitHub-specific API details outside the service.
type PullRequestVerifier interface {
	VerifyPullRequest(context.Context, PullRequestRef) (PullRequestStatus, error)
}

// StateEntryRule validates evidence before a task can enter a target state.
type StateEntryRule interface {
	ValidateStateEntry(context.Context, *model.Task) error
}

// StateEntryRuleFunc adapts a function into a StateEntryRule.
type StateEntryRuleFunc func(context.Context, *model.Task) error

func (f StateEntryRuleFunc) ValidateStateEntry(ctx context.Context, task *model.Task) error {
	return f(ctx, task)
}

type unavailablePullRequestVerifier struct{}

func (unavailablePullRequestVerifier) VerifyPullRequest(context.Context, PullRequestRef) (PullRequestStatus, error) {
	return PullRequestStatus{}, errors.New("GitHub verifier is not configured")
}

func defaultStateEntryRules(verifier PullRequestVerifier) map[model.TaskState][]StateEntryRule {
	return map[model.TaskState][]StateEntryRule{
		model.StateReview: {StateEntryRuleFunc(reviewEntryRule(verifier))},
		model.StateDone:   {StateEntryRuleFunc(doneEntryRule(verifier))},
	}
}

func (s *Service) validateStateEntry(ctx context.Context, task *model.Task, target model.TaskState) error {
	for _, rule := range s.stateEntryRules[target] {
		if err := rule.ValidateStateEntry(ctx, task); err != nil {
			return err
		}
	}
	return nil
}

func reviewEntryRule(verifier PullRequestVerifier) func(context.Context, *model.Task) error {
	return func(ctx context.Context, task *model.Task) error {
		refs := linkedPullRequests(task.Links)
		if len(refs) == 0 {
			return blockedReviewError(task.ID, "no valid GitHub PR link is attached")
		}

		var blockers []string
		var verificationErrors []error
		for _, ref := range refs {
			status, err := verifier.VerifyPullRequest(ctx, ref)
			if err != nil {
				if errors.Is(err, ErrPullRequestNotFound) {
					blockers = append(blockers, fmt.Sprintf("PR #%d does not exist or is not accessible", ref.Number))
					continue
				}
				verificationErrors = append(verificationErrors, fmt.Errorf("verify %s: %w", ref.URL, err))
				continue
			}
			if status.Merged || strings.EqualFold(status.State, PullRequestMerged) || strings.EqualFold(status.State, PullRequestOpen) {
				return nil
			}
			blockers = append(blockers, fmt.Sprintf("PR #%d is closed without being merged", ref.Number))
		}

		if len(verificationErrors) > 0 {
			return verificationUnavailableError(verificationErrors)
		}
		return blockedReviewError(task.ID, strings.Join(blockers, "; "))
	}
}

func doneEntryRule(verifier PullRequestVerifier) func(context.Context, *model.Task) error {
	return func(ctx context.Context, task *model.Task) error {
		refs := linkedPullRequests(task.Links)
		if len(refs) == 0 {
			return blockedDoneError(task.ID, "no valid GitHub PR link is attached")
		}

		var blockers []string
		var verificationErrors []error
		for _, ref := range refs {
			status, err := verifier.VerifyPullRequest(ctx, ref)
			if err != nil {
				if errors.Is(err, ErrPullRequestNotFound) {
					blockers = append(blockers, fmt.Sprintf("PR #%d does not exist or is not accessible", ref.Number))
					continue
				}
				verificationErrors = append(verificationErrors, fmt.Errorf("verify %s: %w", ref.URL, err))
				continue
			}

			reasons := doneBlockers(status)
			if len(reasons) == 0 {
				return nil
			}
			blockers = append(blockers, fmt.Sprintf("PR #%d: %s", ref.Number, strings.Join(reasons, ", ")))
		}

		if len(verificationErrors) > 0 {
			return verificationUnavailableError(verificationErrors)
		}
		return blockedDoneError(task.ID, strings.Join(blockers, "; "))
	}
}

func doneBlockers(status PullRequestStatus) []string {
	var reasons []string
	if !status.Merged && !strings.EqualFold(status.State, PullRequestMerged) {
		reasons = append(reasons, "has not been merged")
	}
	if status.UnresolvedP0P1ReviewThreads > 0 {
		reasons = append(reasons, fmt.Sprintf("has %d unresolved P0/P1 review threads", status.UnresolvedP0P1ReviewThreads))
	}
	rollup := strings.ToUpper(strings.TrimSpace(status.CheckRollupState))
	if rollup != "" && rollup != CheckRollupSuccess {
		reasons = append(reasons, fmt.Sprintf("CI checks are %s", rollup))
	}
	return reasons
}

func blockedReviewError(taskID, reason string) error {
	return fmt.Errorf(
		"%w: cannot enter review: %s; attach a valid GitHub PR first with taskline task link %s --url https://github.com/<owner>/<repo>/pull/<number> --label \"PR #<number>\", then retry",
		ErrStateEntryBlocked,
		reason,
		taskID,
	)
}

func blockedDoneError(taskID, reason string) error {
	return fmt.Errorf(
		"%w: cannot enter done: %s; attach the PR if missing with taskline task link %s --url https://github.com/<owner>/<repo>/pull/<number> --label \"PR #<number>\"; resolve P0/P1 review comments, wait for CI, merge the PR, then retry taskline task update %s --state done",
		ErrStateEntryBlocked,
		reason,
		taskID,
		taskID,
	)
}

func verificationUnavailableError(errs []error) error {
	messages := make([]string, 0, len(errs))
	for _, err := range errs {
		messages = append(messages, err.Error())
	}
	return fmt.Errorf("%w: cannot verify GitHub PR state: %s", ErrStateEntryVerificationUnavailable, strings.Join(messages, "; "))
}

func linkedPullRequests(links []model.Link) []PullRequestRef {
	refs := make([]PullRequestRef, 0, len(links))
	seen := make(map[string]struct{})
	for _, link := range links {
		ref, ok := ParsePullRequestURL(link.URL)
		if !ok {
			continue
		}
		key := strings.ToLower(fmt.Sprintf("%s/%s#%d", ref.Owner, ref.Repository, ref.Number))
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		refs = append(refs, ref)
	}
	return refs
}

// ParsePullRequestURL accepts canonical HTTPS GitHub pull request URLs.
func ParsePullRequestURL(raw string) (PullRequestRef, bool) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || !strings.EqualFold(parsed.Scheme, "https") {
		return PullRequestRef{}, false
	}
	host := strings.ToLower(parsed.Hostname())
	if host != "github.com" && host != "www.github.com" {
		return PullRequestRef{}, false
	}
	parts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	if len(parts) < 4 || parts[0] == "" || parts[1] == "" || parts[2] != "pull" {
		return PullRequestRef{}, false
	}
	number, err := strconv.Atoi(parts[3])
	if err != nil || number <= 0 {
		return PullRequestRef{}, false
	}
	return PullRequestRef{
		URL:        fmt.Sprintf("https://github.com/%s/%s/pull/%d", parts[0], parts[1], number),
		Owner:      parts[0],
		Repository: parts[1],
		Number:     number,
	}, true
}
