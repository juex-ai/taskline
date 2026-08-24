package service_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"taskline_server/api/model"
	"taskline_server/internal/service"
	"taskline_server/internal/store"
)

type fakePullRequestVerifier struct {
	status service.PullRequestStatus
	err    error
	calls  int
}

func (f *fakePullRequestVerifier) VerifyPullRequest(_ context.Context, _ service.PullRequestRef) (service.PullRequestStatus, error) {
	f.calls++
	return f.status, f.err
}

func newWorkflowSvc(t *testing.T, verifier service.PullRequestVerifier) *service.Service {
	t.Helper()
	st, err := store.New(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })
	return service.New(st, service.WithPullRequestVerifier(verifier))
}

func newWorkflowTask(t *testing.T, s *service.Service) *model.Task {
	t.Helper()
	ctx := context.Background()
	p, err := s.CreateProject(ctx, "workflow", "")
	require.NoError(t, err)
	task, err := s.CreateTask(ctx, p.ID, "guard transitions", "", model.TaskTypeFeature, 0, true)
	require.NoError(t, err)
	return task
}

func attachPullRequest(t *testing.T, s *service.Service, taskID string) {
	t.Helper()
	_, err := s.AddLink(context.Background(), taskID, "https://github.com/celalnsa/taskline/pull/123", "PR #123")
	require.NoError(t, err)
}

func TestReviewEntryRequiresValidPullRequestLink(t *testing.T) {
	ctx := context.Background()
	verifier := &fakePullRequestVerifier{status: service.PullRequestStatus{State: service.PullRequestOpen}}
	s := newWorkflowSvc(t, verifier)
	task := newWorkflowTask(t, s)

	review := model.StateReview
	_, err := s.UpdateTask(ctx, task.ID, store.TaskUpdate{State: &review})
	require.ErrorIs(t, err, service.ErrStateEntryBlocked)
	require.Contains(t, err.Error(), "attach a valid GitHub PR")
	require.Contains(t, err.Error(), "taskline task link")
	require.Zero(t, verifier.calls)

	unchanged, getErr := s.GetTask(ctx, task.ID)
	require.NoError(t, getErr)
	require.Equal(t, model.StateStart, unchanged.State)

	attachPullRequest(t, s, task.ID)
	updated, err := s.UpdateTask(ctx, task.ID, store.TaskUpdate{State: &review})
	require.NoError(t, err)
	require.Equal(t, model.StateReview, updated.State)
	require.Equal(t, 1, verifier.calls)
}

func TestReviewEntryRejectsClosedUnmergedPullRequestEvenWithForce(t *testing.T) {
	ctx := context.Background()
	verifier := &fakePullRequestVerifier{status: service.PullRequestStatus{State: service.PullRequestClosed}}
	s := newWorkflowSvc(t, verifier)
	task := newWorkflowTask(t, s)
	attachPullRequest(t, s, task.ID)

	review := model.StateReview
	_, err := s.UpdateTask(ctx, task.ID, store.TaskUpdate{State: &review, Force: true})
	require.ErrorIs(t, err, service.ErrStateEntryBlocked)
	require.Contains(t, err.Error(), "closed without being merged")
}

func TestDoneEntryRequiresMergedResolvedGreenPullRequest(t *testing.T) {
	tests := []struct {
		name       string
		status     service.PullRequestStatus
		wantReason string
	}{
		{
			name:       "not merged",
			status:     service.PullRequestStatus{State: service.PullRequestOpen, CheckRollupState: service.CheckRollupSuccess},
			wantReason: "has not been merged",
		},
		{
			name:       "unresolved P0 or P1 comments",
			status:     service.PullRequestStatus{State: service.PullRequestMerged, Merged: true, UnresolvedP0P1ReviewThreads: 2, CheckRollupState: service.CheckRollupSuccess},
			wantReason: "2 unresolved P0/P1 review threads",
		},
		{
			name:       "ci pending",
			status:     service.PullRequestStatus{State: service.PullRequestMerged, Merged: true, CheckRollupState: service.CheckRollupPending},
			wantReason: "CI checks are PENDING",
		},
		{
			name:       "ci failed",
			status:     service.PullRequestStatus{State: service.PullRequestMerged, Merged: true, CheckRollupState: service.CheckRollupFailure},
			wantReason: "CI checks are FAILURE",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			verifier := &fakePullRequestVerifier{status: tt.status}
			s := newWorkflowSvc(t, verifier)
			task := newWorkflowTask(t, s)
			attachPullRequest(t, s, task.ID)

			done := model.StateDone
			_, err := s.UpdateTask(ctx, task.ID, store.TaskUpdate{State: &done, Force: true})
			require.ErrorIs(t, err, service.ErrStateEntryBlocked)
			require.Contains(t, err.Error(), tt.wantReason)
			require.Contains(t, err.Error(), "resolve P0/P1 review comments, wait for CI, merge the PR, then retry")
		})
	}
}

func TestDoneEntryWithoutPullRequestExplainsHowToAttachOne(t *testing.T) {
	ctx := context.Background()
	s := newWorkflowSvc(t, &fakePullRequestVerifier{})
	task := newWorkflowTask(t, s)

	done := model.StateDone
	_, err := s.UpdateTask(ctx, task.ID, store.TaskUpdate{State: &done})
	require.ErrorIs(t, err, service.ErrStateEntryBlocked)
	require.Contains(t, err.Error(), "taskline task link "+task.ID)
}

func TestDoneEntryAcceptsMergedResolvedPullRequestWithGreenOrNoChecks(t *testing.T) {
	for _, rollup := range []string{service.CheckRollupSuccess, ""} {
		t.Run("rollup "+rollup, func(t *testing.T) {
			ctx := context.Background()
			verifier := &fakePullRequestVerifier{status: service.PullRequestStatus{
				State:            service.PullRequestMerged,
				Merged:           true,
				CheckRollupState: rollup,
			}}
			s := newWorkflowSvc(t, verifier)
			task := newWorkflowTask(t, s)
			attachPullRequest(t, s, task.ID)

			done := model.StateDone
			updated, err := s.UpdateTask(ctx, task.ID, store.TaskUpdate{State: &done})
			require.NoError(t, err)
			require.Equal(t, model.StateDone, updated.State)
		})
	}
}

func TestDoneEntryAcceptsMergedPullRequestWithOnlyUnresolvedP2P3Threads(t *testing.T) {
	ctx := context.Background()
	verifier := &fakePullRequestVerifier{status: service.PullRequestStatus{
		State:            service.PullRequestMerged,
		Merged:           true,
		CheckRollupState: service.CheckRollupSuccess,
	}}
	s := newWorkflowSvc(t, verifier)
	task := newWorkflowTask(t, s)
	attachPullRequest(t, s, task.ID)

	done := model.StateDone
	updated, err := s.UpdateTask(ctx, task.ID, store.TaskUpdate{State: &done})
	require.NoError(t, err)
	require.Equal(t, model.StateDone, updated.State)
}

func TestStateEntryVerificationFailureIsDistinctFromBlockedEvidence(t *testing.T) {
	ctx := context.Background()
	verifier := &fakePullRequestVerifier{err: errors.New("github unavailable")}
	s := newWorkflowSvc(t, verifier)
	task := newWorkflowTask(t, s)
	attachPullRequest(t, s, task.ID)

	review := model.StateReview
	_, err := s.UpdateTask(ctx, task.ID, store.TaskUpdate{State: &review})
	require.ErrorIs(t, err, service.ErrStateEntryVerificationUnavailable)
	require.True(t, strings.Contains(err.Error(), "github unavailable"), err)
}

func TestSameStateUpdateDoesNotReverifyPullRequest(t *testing.T) {
	ctx := context.Background()
	verifier := &fakePullRequestVerifier{status: service.PullRequestStatus{State: service.PullRequestOpen}}
	s := newWorkflowSvc(t, verifier)
	task := newWorkflowTask(t, s)

	start := model.StateStart
	title := "renamed"
	updated, err := s.UpdateTask(ctx, task.ID, store.TaskUpdate{State: &start, Title: &title})
	require.NoError(t, err)
	require.Equal(t, title, updated.Title)
	require.Zero(t, verifier.calls)
}

func TestParsePullRequestURL(t *testing.T) {
	ref, ok := service.ParsePullRequestURL("https://github.com/celalnsa/taskline/pull/42/files?diff=split")
	require.True(t, ok)
	require.Equal(t, "celalnsa", ref.Owner)
	require.Equal(t, "taskline", ref.Repository)
	require.Equal(t, 42, ref.Number)
	require.Equal(t, "https://github.com/celalnsa/taskline/pull/42", ref.URL)

	for _, raw := range []string{
		"http://github.com/celalnsa/taskline/pull/42",
		"https://example.com/celalnsa/taskline/pull/42",
		"https://github.com/celalnsa/taskline/issues/42",
		"https://github.com/celalnsa/taskline/pull/not-a-number",
	} {
		_, ok := service.ParsePullRequestURL(raw)
		require.False(t, ok, raw)
	}
}
