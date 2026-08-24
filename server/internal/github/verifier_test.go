package github_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	githubapi "taskline_server/internal/github"
	"taskline_server/internal/service"
)

func TestVerifierReadsPullRequestEvidenceAndPaginatesReviewThreads(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		require.Equal(t, "Bearer test-token", r.Header.Get("Authorization"))
		var body struct {
			Variables map[string]any `json:"variables"`
		}
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		require.Equal(t, "celalnsa", body.Variables["owner"])
		require.Equal(t, "taskline", body.Variables["name"])
		require.Equal(t, float64(123), body.Variables["number"])

		w.Header().Set("Content-Type", "application/json")
		if requests == 1 {
			require.Nil(t, body.Variables["after"])
			_, _ = w.Write([]byte(`{
                "data": {"repository": {"pullRequest": {
                    "url": "https://github.com/celalnsa/taskline/pull/123",
                    "state": "MERGED",
                    "merged": true,
                    "reviewThreads": {
                        "nodes": [
                            {"isResolved": false, "comments": {"totalCount": 1, "nodes": [{"body": "[P2] optional cleanup"}]}},
                            {"isResolved": true, "comments": {"totalCount": 1, "nodes": [{"body": "[P0] already resolved"}]}},
                            {"isResolved": true, "comments": {"totalCount": 1, "nodes": [{"body": "resolved without classification"}]}}
                        ],
                        "pageInfo": {"hasNextPage": true, "endCursor": "cursor-1"}
                    },
                    "commits": {"nodes": [{"commit": {"statusCheckRollup": {"state": "SUCCESS"}}}]}
                }}}}
            }`))
			return
		}
		require.Equal(t, "cursor-1", body.Variables["after"])
		_, _ = w.Write([]byte(`{
            "data": {"repository": {"pullRequest": {
                "url": "https://github.com/celalnsa/taskline/pull/123",
                "state": "MERGED",
                "merged": true,
                "reviewThreads": {
                    "nodes": [
                        {"isResolved": false, "comments": {"totalCount": 1, "nodes": [{"body": "[P1] fix correctness"}]}},
                        {"isResolved": false, "comments": {"totalCount": 1, "nodes": [{"body": "needs classification"}]}}
                    ],
                    "pageInfo": {"hasNextPage": false, "endCursor": null}
                },
                "commits": {"nodes": [{"commit": {"statusCheckRollup": {"state": "SUCCESS"}}}]}
            }}}}
        }`))
	}))
	t.Cleanup(server.Close)

	verifier := githubapi.NewVerifier(
		githubapi.WithEndpoint(server.URL),
		githubapi.WithHTTPClient(server.Client()),
		githubapi.WithTokenSource(githubapi.TokenSourceFunc(func(context.Context) (string, error) {
			return "test-token", nil
		})),
	)
	status, err := verifier.VerifyPullRequest(context.Background(), service.PullRequestRef{
		URL:        "https://github.com/celalnsa/taskline/pull/123",
		Owner:      "celalnsa",
		Repository: "taskline",
		Number:     123,
	})
	require.NoError(t, err)
	require.Equal(t, service.PullRequestMerged, status.State)
	require.True(t, status.Merged)
	require.Equal(t, 1, status.UnresolvedP0P1ReviewThreads)
	require.Equal(t, service.CheckRollupSuccess, status.CheckRollupState)
	require.Equal(t, 2, requests)
}

func TestVerifierFindsP0P1AcrossReviewThreadComments(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
            "data": {"repository": {"pullRequest": {
                "state": "MERGED",
                "merged": true,
                "reviewThreads": {
                    "nodes": [
                        {"isResolved": false, "comments": {"totalCount": 2, "nodes": [{"body": "[P2] initial"}, {"body": "Priority: P1"}]}},
                        {"isResolved": false, "comments": {"totalCount": 2, "nodes": [{"body": "discussion mentions P0 later but is not a marker"}, {"body": "Priority: P3"}]}}
                    ],
                    "pageInfo": {"hasNextPage": false, "endCursor": null}
                },
                "commits": {"nodes": [{"commit": {"statusCheckRollup": {"state": "SUCCESS"}}}]}
            }}}}
        }`))
	}))
	t.Cleanup(server.Close)

	verifier := githubapi.NewVerifier(
		githubapi.WithEndpoint(server.URL),
		githubapi.WithHTTPClient(server.Client()),
		githubapi.WithTokenSource(githubapi.TokenSourceFunc(func(context.Context) (string, error) {
			return "test-token", nil
		})),
	)
	status, err := verifier.VerifyPullRequest(context.Background(), service.PullRequestRef{URL: "https://github.com/o/r/pull/9", Owner: "o", Repository: "r", Number: 9})
	require.NoError(t, err)
	require.Equal(t, 1, status.UnresolvedP0P1ReviewThreads)
}

func TestVerifierReturnsNotFoundForMissingPullRequest(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"repository":{"pullRequest":null}}}`))
	}))
	t.Cleanup(server.Close)

	verifier := githubapi.NewVerifier(
		githubapi.WithEndpoint(server.URL),
		githubapi.WithHTTPClient(server.Client()),
		githubapi.WithTokenSource(githubapi.TokenSourceFunc(func(context.Context) (string, error) {
			return "test-token", nil
		})),
	)
	_, err := verifier.VerifyPullRequest(context.Background(), service.PullRequestRef{Owner: "o", Repository: "r", Number: 9})
	require.ErrorIs(t, err, service.ErrPullRequestNotFound)
}

func TestVerifierReturnsNotFoundForGitHubResolutionError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"repository":null},"errors":[{"message":"Could not resolve to a Repository with the name 'missing'."}]}`))
	}))
	t.Cleanup(server.Close)

	verifier := githubapi.NewVerifier(
		githubapi.WithEndpoint(server.URL),
		githubapi.WithHTTPClient(server.Client()),
		githubapi.WithTokenSource(githubapi.TokenSourceFunc(func(context.Context) (string, error) {
			return "test-token", nil
		})),
	)
	_, err := verifier.VerifyPullRequest(context.Background(), service.PullRequestRef{URL: "https://github.com/o/missing/pull/9", Owner: "o", Repository: "missing", Number: 9})
	require.ErrorIs(t, err, service.ErrPullRequestNotFound)
}

func TestVerifierSurfacesRejectedTokenGuidance(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "Bad credentials", http.StatusUnauthorized)
	}))
	t.Cleanup(server.Close)

	verifier := githubapi.NewVerifier(
		githubapi.WithEndpoint(server.URL),
		githubapi.WithHTTPClient(server.Client()),
		githubapi.WithTokenSource(githubapi.TokenSourceFunc(func(context.Context) (string, error) {
			return "expired-token", nil
		})),
	)
	_, err := verifier.VerifyPullRequest(context.Background(), service.PullRequestRef{URL: "https://github.com/o/r/pull/9", Owner: "o", Repository: "r", Number: 9})
	require.Error(t, err)
	require.Contains(t, err.Error(), "TASKLINE_GITHUB_TOKEN")
	require.Contains(t, err.Error(), "gh auth login")
}

func TestVerifierSurfacesAuthenticationGuidance(t *testing.T) {
	verifier := githubapi.NewVerifier(githubapi.WithTokenSource(githubapi.TokenSourceFunc(func(context.Context) (string, error) {
		return "", errors.New("no credential")
	})))
	_, err := verifier.VerifyPullRequest(context.Background(), service.PullRequestRef{Owner: "o", Repository: "r", Number: 9})
	require.Error(t, err)
	require.Contains(t, err.Error(), "TASKLINE_GITHUB_TOKEN")
	require.Contains(t, err.Error(), "gh auth login")
}

func TestDefaultTokenSourcePrefersTasklineEnvironmentToken(t *testing.T) {
	t.Setenv("TASKLINE_GITHUB_TOKEN", "taskline-token")
	t.Setenv("GITHUB_TOKEN", "github-token")
	t.Setenv("GH_TOKEN", "gh-token")

	token, err := githubapi.DefaultTokenSource().Token(context.Background())
	require.NoError(t, err)
	require.Equal(t, "taskline-token", token)
}
