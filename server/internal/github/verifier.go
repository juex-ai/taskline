package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"taskline_server/internal/service"
)

const defaultGraphQLEndpoint = "https://api.github.com/graphql"

var blockingPriorityLinePattern = regexp.MustCompile(`(?i)^(?:\*\*)?(?:[-*]\s+)?(?:\[\s*P[01]\s*\]|priority\s*:\s*P?[01]\b|P[01]\s*:)`)

const pullRequestQuery = `
query TasklinePullRequestEvidence($owner: String!, $name: String!, $number: Int!, $after: String) {
  repository(owner: $owner, name: $name) {
    pullRequest(number: $number) {
      url
      state
      merged
      reviewThreads(first: 100, after: $after) {
        nodes {
          isResolved
          comments(first: 100) {
            nodes { body }
          }
        }
        pageInfo { hasNextPage endCursor }
      }
      commits(last: 1) {
        nodes {
          commit {
            statusCheckRollup { state }
          }
        }
      }
    }
  }
}`

// TokenSource supplies a GitHub token without exposing credential storage to
// the workflow service.
type TokenSource interface {
	Token(context.Context) (string, error)
}

// TokenSourceFunc adapts a function into a TokenSource.
type TokenSourceFunc func(context.Context) (string, error)

func (f TokenSourceFunc) Token(ctx context.Context) (string, error) { return f(ctx) }

// Verifier implements service.PullRequestVerifier with GitHub GraphQL.
type Verifier struct {
	endpoint   string
	httpClient *http.Client
	tokens     TokenSource
}

// Option customizes the GitHub adapter, primarily for isolated tests.
type Option func(*Verifier)

func WithEndpoint(endpoint string) Option {
	return func(verifier *Verifier) {
		if strings.TrimSpace(endpoint) != "" {
			verifier.endpoint = endpoint
		}
	}
}

func WithHTTPClient(client *http.Client) Option {
	return func(verifier *Verifier) {
		if client != nil {
			verifier.httpClient = client
		}
	}
}

func WithTokenSource(source TokenSource) Option {
	return func(verifier *Verifier) {
		if source != nil {
			verifier.tokens = source
		}
	}
}

func NewVerifier(options ...Option) *Verifier {
	verifier := &Verifier{
		endpoint:   defaultGraphQLEndpoint,
		httpClient: &http.Client{Timeout: 15 * time.Second},
		tokens:     DefaultTokenSource(),
	}
	for _, option := range options {
		option(verifier)
	}
	return verifier
}

func (v *Verifier) VerifyPullRequest(ctx context.Context, ref service.PullRequestRef) (service.PullRequestStatus, error) {
	token, err := v.tokens.Token(ctx)
	if err != nil || strings.TrimSpace(token) == "" {
		if err == nil {
			err = errors.New("empty token")
		}
		return service.PullRequestStatus{}, fmt.Errorf(
			"GitHub authentication unavailable: set TASKLINE_GITHUB_TOKEN, GITHUB_TOKEN, or GH_TOKEN for taskline-server, or run gh auth login on the server host: %w",
			err,
		)
	}

	var status service.PullRequestStatus
	var after *string
	for {
		response, err := v.query(ctx, token, ref, after)
		if err != nil {
			return service.PullRequestStatus{}, err
		}
		if response.Data.Repository == nil || response.Data.Repository.PullRequest == nil {
			return service.PullRequestStatus{}, fmt.Errorf("%w: %s", service.ErrPullRequestNotFound, ref.URL)
		}

		pr := response.Data.Repository.PullRequest
		status.State = strings.ToUpper(pr.State)
		status.Merged = pr.Merged
		for _, thread := range pr.ReviewThreads.Nodes {
			if thread.IsResolved {
				continue
			}
			if hasBlockingPriority(thread.Comments.Nodes) {
				status.UnresolvedP0P1ReviewThreads++
			}
		}
		if len(pr.Commits.Nodes) > 0 && pr.Commits.Nodes[0].Commit.StatusCheckRollup != nil {
			status.CheckRollupState = strings.ToUpper(pr.Commits.Nodes[0].Commit.StatusCheckRollup.State)
		}
		if !pr.ReviewThreads.PageInfo.HasNextPage {
			return status, nil
		}
		if pr.ReviewThreads.PageInfo.EndCursor == nil || *pr.ReviewThreads.PageInfo.EndCursor == "" {
			return service.PullRequestStatus{}, errors.New("GitHub review thread pagination returned an empty cursor")
		}
		after = pr.ReviewThreads.PageInfo.EndCursor
	}
}

type graphQLResponse struct {
	Data struct {
		Repository *struct {
			PullRequest *struct {
				URL           string `json:"url"`
				State         string `json:"state"`
				Merged        bool   `json:"merged"`
				ReviewThreads struct {
					Nodes []struct {
						IsResolved bool `json:"isResolved"`
						Comments   struct {
							Nodes []struct {
								Body string `json:"body"`
							} `json:"nodes"`
						} `json:"comments"`
					} `json:"nodes"`
					PageInfo struct {
						HasNextPage bool    `json:"hasNextPage"`
						EndCursor   *string `json:"endCursor"`
					} `json:"pageInfo"`
				} `json:"reviewThreads"`
				Commits struct {
					Nodes []struct {
						Commit struct {
							StatusCheckRollup *struct {
								State string `json:"state"`
							} `json:"statusCheckRollup"`
						} `json:"commit"`
					} `json:"nodes"`
				} `json:"commits"`
			} `json:"pullRequest"`
		} `json:"repository"`
	} `json:"data"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
}

func hasBlockingPriority(comments []struct {
	Body string `json:"body"`
}) bool {
	for _, comment := range comments {
		line := firstNonEmptyLine(comment.Body)
		if blockingPriorityLinePattern.MatchString(line) {
			return true
		}
	}
	return false
}

func firstNonEmptyLine(body string) string {
	for _, line := range strings.Split(body, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			return line
		}
	}
	return ""
}

func (v *Verifier) query(ctx context.Context, token string, ref service.PullRequestRef, after *string) (*graphQLResponse, error) {
	payload := map[string]any{
		"query": pullRequestQuery,
		"variables": map[string]any{
			"owner":  ref.Owner,
			"name":   ref.Repository,
			"number": ref.Number,
			"after":  after,
		},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("encode GitHub GraphQL request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, v.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create GitHub GraphQL request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "taskline-server")

	resp, err := v.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("query GitHub PR %s: %w", ref.URL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		message, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			return nil, fmt.Errorf(
				"GitHub authentication or authorization failed (HTTP %d): check TASKLINE_GITHUB_TOKEN, GITHUB_TOKEN, or GH_TOKEN, or run gh auth login on the server host: %s",
				resp.StatusCode,
				strings.TrimSpace(string(message)),
			)
		}
		return nil, fmt.Errorf("query GitHub PR %s: HTTP %d: %s", ref.URL, resp.StatusCode, strings.TrimSpace(string(message)))
	}

	var result graphQLResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode GitHub PR %s: %w", ref.URL, err)
	}
	if len(result.Errors) > 0 {
		messages := make([]string, 0, len(result.Errors))
		for _, graphQLError := range result.Errors {
			messages = append(messages, graphQLError.Message)
		}
		if isResolutionError(messages) {
			return nil, fmt.Errorf("%w: %s", service.ErrPullRequestNotFound, ref.URL)
		}
		return nil, fmt.Errorf("query GitHub PR %s: %s", ref.URL, strings.Join(messages, "; "))
	}
	return &result, nil
}

func isResolutionError(messages []string) bool {
	for _, message := range messages {
		normalized := strings.ToLower(message)
		if strings.Contains(normalized, "could not resolve to a repository") ||
			strings.Contains(normalized, "could not resolve to a pullrequest") ||
			strings.Contains(normalized, "could not resolve to a pull request") {
			return true
		}
	}
	return false
}

type defaultTokenSource struct {
	mu    sync.Mutex
	token string
}

// DefaultTokenSource reads an explicit service token first, then asks the
// installed gh CLI so formal local deployments can reuse keychain credentials.
func DefaultTokenSource() TokenSource { return &defaultTokenSource{} }

func (s *defaultTokenSource) Token(ctx context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.token != "" {
		return s.token, nil
	}
	for _, key := range []string{"TASKLINE_GITHUB_TOKEN", "GITHUB_TOKEN", "GH_TOKEN"} {
		if token := strings.TrimSpace(os.Getenv(key)); token != "" {
			s.token = token
			return s.token, nil
		}
	}

	ghPath, err := findGHExecutable()
	if err != nil {
		return "", err
	}
	output, err := exec.CommandContext(ctx, ghPath, "auth", "token", "--hostname", "github.com").Output()
	if err != nil {
		return "", fmt.Errorf("run %s auth token: %w", ghPath, err)
	}
	s.token = strings.TrimSpace(string(output))
	if s.token == "" {
		return "", errors.New("gh auth token returned an empty token")
	}
	return s.token, nil
}

func findGHExecutable() (string, error) {
	if path, err := exec.LookPath("gh"); err == nil {
		return path, nil
	}
	candidates := []string{"/opt/homebrew/bin/gh", "/usr/local/bin/gh"}
	if home, err := os.UserHomeDir(); err == nil {
		candidates = append(candidates, filepath.Join(home, ".local", "bin", "gh"))
	}
	for _, candidate := range candidates {
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
			return candidate, nil
		}
	}
	return "", errors.New("gh executable not found")
}
