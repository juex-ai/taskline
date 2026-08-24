package tests

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/hertz/pkg/app/server"
	"github.com/stretchr/testify/require"

	"taskline_server/api/handler"
	"taskline_server/api/model"
	"taskline_server/internal/config"
	"taskline_server/internal/service"
	"taskline_server/internal/store"
)

// startServer boots a taskline-server instance backed by a temp SQLite file +
// random port. Returns the base URL and a shutdown func.
func startServer(t *testing.T) (string, func()) {
	return startServerWithVerifier(t, &mutablePullRequestVerifier{status: service.PullRequestStatus{
		State:            service.PullRequestMerged,
		Merged:           true,
		CheckRollupState: service.CheckRollupSuccess,
	}})
}

type mutablePullRequestVerifier struct {
	status service.PullRequestStatus
	err    error
}

func (v *mutablePullRequestVerifier) VerifyPullRequest(context.Context, service.PullRequestRef) (service.PullRequestStatus, error) {
	return v.status, v.err
}

func startServerWithVerifier(t *testing.T, verifier service.PullRequestVerifier) (string, func()) {
	t.Helper()
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "taskline.db")
	imagesDir := filepath.Join(tmp, "images")
	docsDir := filepath.Join(tmp, "docs")
	require.NoError(t, os.MkdirAll(imagesDir, 0o700))
	require.NoError(t, os.MkdirAll(docsDir, 0o700))

	st, err := store.New(dbPath)
	require.NoError(t, err)
	svc := service.New(st, service.WithPullRequestVerifier(verifier))

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := ln.Addr().String()
	require.NoError(t, ln.Close())

	cfg := &config.Config{DBPath: dbPath, ListenAddr: addr, ImagesDir: imagesDir, DocsDir: docsDir}
	h := handler.New(svc, cfg)

	hz := server.New(server.WithHostPorts(addr))
	h.Register(hz)
	go hz.Spin()

	base := "http://" + addr
	// Poll /healthz until the listener is accepting.
	deadline := time.Now().Add(5 * time.Second)
	for {
		resp, err := http.Get(base + "/healthz")
		if err == nil {
			resp.Body.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("server did not become ready: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	return base, func() {
		_ = hz.Shutdown(context.Background())
		_ = st.Close()
	}
}

func attachTestPullRequest(t *testing.T, base, taskID string) {
	t.Helper()
	st := jsonReq(t, http.MethodPost, base+"/api/v1/tasks/"+taskID+"/links", map[string]any{
		"url":   "https://github.com/celalnsa/taskline/pull/123",
		"label": "PR #123",
	}, nil)
	require.Equal(t, http.StatusCreated, st)
}

func jsonReqError(t *testing.T, method, requestURL string, body any) (int, string) {
	return jsonReqErrorWithToken(t, method, requestURL, body, "")
}

func jsonReqErrorWithToken(t *testing.T, method, requestURL string, body any, token string) (int, string) {
	t.Helper()
	raw, err := json.Marshal(body)
	require.NoError(t, err)
	req, err := http.NewRequest(method, requestURL, bytes.NewReader(raw))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	responseBody, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, string(responseBody)
}

// jsonReq performs a single JSON request, decoding into out (nil to skip).
// Returns the HTTP status so callers can assert on it.
func jsonReq(t *testing.T, method, url string, body any, out any) int {
	t.Helper()
	return jsonReqWithToken(t, method, url, body, out, "")
}

func jsonReqWithToken(t *testing.T, method, url string, body any, out any, token string) int {
	return jsonReqWithHeaders(t, method, url, body, out, token, nil)
}

func jsonReqWithHeaders(t *testing.T, method, url string, body any, out any, token string, headers map[string]string) int {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		require.NoError(t, err)
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, url, rdr)
	require.NoError(t, err)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if out != nil && len(raw) > 0 && resp.StatusCode < 400 {
		require.NoError(t, json.Unmarshal(raw, out), "decode body: %s", string(raw))
	}
	return resp.StatusCode
}

func TestEmbeddedWebBundleServesEntryAsset(t *testing.T) {
	if os.Getenv("TASKLINE_REQUIRE_WEB_BUNDLE") != "1" {
		t.Skip("set TASKLINE_REQUIRE_WEB_BUNDLE=1 for the release bundle gate")
	}

	base, stop := startServer(t)
	defer stop()

	indexResponse, err := http.Get(base + "/")
	require.NoError(t, err)
	defer indexResponse.Body.Close()
	indexBody, err := io.ReadAll(indexResponse.Body)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, indexResponse.StatusCode)

	indexHTML := string(indexBody)
	require.Contains(t, indexHTML, `id="root"`, "embedded index is not the production app shell")
	scriptStart := strings.Index(indexHTML, "<script")
	require.NotEqual(t, -1, scriptStart, "embedded index has no script element")
	scriptEnd := strings.Index(indexHTML[scriptStart:], ">")
	require.NotEqual(t, -1, scriptEnd, "embedded script element is unterminated")
	scriptTag := indexHTML[scriptStart : scriptStart+scriptEnd+1]
	require.Contains(t, scriptTag, `type="module"`, "embedded entry script is not a module")
	srcStart := strings.Index(scriptTag, `src="`)
	require.NotEqual(t, -1, srcStart, "embedded module script has no src")
	srcStart += len(`src="`)
	srcEnd := strings.Index(scriptTag[srcStart:], `"`)
	require.NotEqual(t, -1, srcEnd, "embedded module script src is unterminated")
	entryAsset := scriptTag[srcStart : srcStart+srcEnd]
	require.True(t, strings.HasPrefix(entryAsset, "/assets/"), "entry asset is not local: %q", entryAsset)
	require.True(t, strings.HasSuffix(entryAsset, ".js"), "entry asset is not JavaScript: %q", entryAsset)

	assetResponse, err := http.Get(base + entryAsset)
	require.NoError(t, err)
	defer assetResponse.Body.Close()
	assetBody, err := io.ReadAll(assetResponse.Body)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, assetResponse.StatusCode, "entry asset %s", entryAsset)
	require.NotEmpty(t, strings.TrimSpace(string(assetBody)), "entry asset %s is empty", entryAsset)
	require.NotContains(t, string(assetBody), "<!doctype html>", "entry asset %s fell back to index.html", entryAsset)

	contentType := assetResponse.Header.Get("Content-Type")
	mediaType, _, err := mime.ParseMediaType(contentType)
	require.NoError(t, err, "entry asset %s has invalid Content-Type %q", entryAsset, contentType)
	require.Contains(t, []string{"text/javascript", "application/javascript"}, mediaType,
		"entry asset %s has Content-Type %q", entryAsset, contentType)
}

func TestTaskHistoryTracksActorsChangesAndSurvivesDeletion(t *testing.T) {
	base, stop := startServer(t)
	defer stop()

	jsonReq(t, http.MethodPost, base+"/api/v1/projects",
		map[string]any{"name": "history"}, &project{})
	var created task
	st := jsonReqWithHeaders(t, http.MethodPost, base+"/api/v1/projects/history/tasks",
		map[string]any{
			"title": "Before", "description": "Old description",
			"type": "feature", "auto_start": true,
		},
		&created, "", map[string]string{"X-Taskline-Client": "web"})
	require.Equal(t, http.StatusCreated, st)

	st = jsonReqWithHeaders(t, http.MethodPatch, base+"/api/v1/tasks/"+created.ID,
		map[string]any{"title": "After", "description": "New description"},
		&created, "", map[string]string{"X-Taskline-Client": "cli"})
	require.Equal(t, http.StatusOK, st)

	token := registerAgent(t, base, "history-agent")
	st = jsonReqWithHeaders(t, http.MethodPost, base+"/api/v1/tasks/"+created.ID+"/claim",
		map[string]any{"lease": "1h"}, &created, token,
		map[string]string{"X-Taskline-Client": "cli"})
	require.Equal(t, http.StatusOK, st)

	var history struct {
		Events []model.TaskEvent `json:"events"`
	}
	st = jsonReq(t, http.MethodGet, base+"/api/v1/tasks/"+created.ID+"/events", nil, &history)
	require.Equal(t, http.StatusOK, st)
	require.Len(t, history.Events, 3)
	require.Equal(t, "claimed", history.Events[0].Action)
	require.Equal(t, "history-agent", history.Events[0].Actor)
	require.Equal(t, "updated", history.Events[1].Action)
	require.Equal(t, "cli", history.Events[1].Actor)
	require.Equal(t, "created", history.Events[2].Action)
	require.Equal(t, "web", history.Events[2].Actor)
	changes := history.Events[1].Details["changes"].(map[string]any)
	require.Equal(t, "Before", changes["title"].(map[string]any)["before"])
	require.Equal(t, "After", changes["title"].(map[string]any)["after"])

	st = jsonReqWithHeaders(t, http.MethodDelete, base+"/api/v1/tasks/"+created.ID,
		nil, nil, "", map[string]string{"X-Taskline-Client": "cli"})
	require.Equal(t, http.StatusOK, st)
	st = jsonReq(t, http.MethodGet, base+"/api/v1/tasks/"+created.ID+"/events", nil, &history)
	require.Equal(t, http.StatusOK, st)
	require.Len(t, history.Events, 4)
	require.Equal(t, "deleted", history.Events[0].Action)
	require.Equal(t, "cli", history.Events[0].Actor)
}

func registerAgent(t *testing.T, base, name string) string {
	t.Helper()
	var out struct {
		Agent agent  `json:"agent"`
		Token string `json:"token"`
	}
	st := jsonReq(t, "POST", base+"/api/v1/agents/register", map[string]any{"name": name}, &out)
	require.Equal(t, http.StatusCreated, st)
	require.Equal(t, name, out.Agent.Name)
	require.NotEmpty(t, out.Token)
	return out.Token
}

func TestStatusAndDuplicateRegistrationAtAPI(t *testing.T) {
	base, stop := startServer(t)
	defer stop()

	var anonymous model.ServerStatus
	st := jsonReq(t, http.MethodGet, base+"/api/v1/status", nil, &anonymous)
	require.Equal(t, http.StatusOK, st)
	require.True(t, anonymous.OK)
	require.Nil(t, anonymous.Agent)
	require.Empty(t, anonymous.ActiveTasks)

	token := registerAgent(t, base, "status-agent")
	jsonReq(t, http.MethodPost, base+"/api/v1/projects", map[string]any{"name": "status"}, &project{})
	var created task
	st = jsonReq(t, http.MethodPost, base+"/api/v1/projects/status/tasks",
		map[string]any{"title": "claimed", "type": "feature", "auto_start": true}, &created)
	require.Equal(t, http.StatusCreated, st)
	st = jsonReqWithToken(t, http.MethodPost, base+"/api/v1/tasks/"+created.ID+"/claim",
		map[string]any{"lease": "1h"}, &created, token)
	require.Equal(t, http.StatusOK, st)

	var authenticated model.ServerStatus
	st = jsonReqWithToken(t, http.MethodGet, base+"/api/v1/status", nil, &authenticated, token)
	require.Equal(t, http.StatusOK, st)
	require.NotNil(t, authenticated.Agent)
	require.Equal(t, "status-agent", authenticated.Agent.Name)
	require.Len(t, authenticated.ActiveTasks, 1)
	require.Equal(t, created.ID, authenticated.ActiveTasks[0].ID)

	st = jsonReqWithToken(t, http.MethodGet, base+"/api/v1/status", nil, nil, "invalid-token")
	require.Equal(t, http.StatusUnauthorized, st)

	status, body := jsonReqErrorWithToken(t, http.MethodPost, base+"/api/v1/agents/register",
		map[string]any{"name": "replacement"}, token)
	require.Equal(t, http.StatusConflict, status)
	require.Contains(t, body, "already registered as status-agent")
	require.Contains(t, body, "taskline status")
}

type project struct {
	ID, Name, Description string
}

type agent struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	CreatedAt int64  `json:"created_at"`
	UpdatedAt int64  `json:"updated_at"`
}

type task struct {
	ID, ProjectID, Title, Description, Type, State string
	Priority                                       int
	Labels                                         []string `json:"labels,omitempty"`
	Owner                                          string   `json:"owner"`
	ClaimedAt                                      int64    `json:"claimed_at"`
	LeaseExpiresAt                                 int64    `json:"lease_expires_at"`
	DependsOn                                      []string `json:"depends_on,omitempty"`
	Images                                         []image  `json:"images,omitempty"`
	Docs                                           []doc    `json:"docs,omitempty"`
}

type image struct {
	ID         string `json:"id"`
	TaskID     string `json:"task_id"`
	Filename   string `json:"filename"`
	MimeType   string `json:"mime_type"`
	SizeBytes  int64  `json:"size_bytes"`
	URL        string `json:"url"`
	UploadedAt int64  `json:"uploaded_at"`
}

type doc struct {
	ID        string `json:"id"`
	TaskID    string `json:"task_id"`
	Title     string `json:"title"`
	URL       string `json:"url"`
	Content   string `json:"content,omitempty"`
	CreatedAt int64  `json:"created_at"`
	UpdatedAt int64  `json:"updated_at"`
}

type taskListResp struct {
	Tasks []task `json:"tasks"`
}

type nextResp struct {
	Task *task `json:"task"`
}

func TestEndToEndHappyPath(t *testing.T) {
	base, stop := startServer(t)
	defer stop()

	// Create project.
	var p project
	st := jsonReq(t, "POST", base+"/api/v1/projects",
		map[string]any{"name": "demo", "description": "e2e"}, &p)
	require.Equal(t, http.StatusCreated, st)
	require.Equal(t, "demo", p.Name)

	// Two tasks created with auto_start=true so they're immediately
	// runnable. t2 depends on t1 but has higher priority.
	var t1, t2 task
	st = jsonReq(t, "POST", base+"/api/v1/projects/demo/tasks",
		map[string]any{"title": "first", "type": "feature", "priority": 1, "auto_start": true}, &t1)
	require.Equal(t, http.StatusCreated, st)
	require.Equal(t, "start", t1.State)
	st = jsonReq(t, "POST", base+"/api/v1/projects/demo/tasks",
		map[string]any{"title": "second", "type": "bug", "priority": 9, "auto_start": true}, &t2)
	require.Equal(t, http.StatusCreated, st)

	// Add dependency.
	st = jsonReq(t, "POST", base+"/api/v1/tasks/"+t2.ID+"/deps",
		map[string]any{"depends_on": t1.ID}, nil)
	require.Equal(t, http.StatusCreated, st)

	// Initially only t1 is runnable (t2 blocked even though prio is higher).
	var runnable taskListResp
	st = jsonReq(t, "GET", base+"/api/v1/projects/demo/tasks/runnable", nil, &runnable)
	require.Equal(t, http.StatusOK, st)
	require.Len(t, runnable.Tasks, 1)
	require.Equal(t, t1.ID, runnable.Tasks[0].ID)

	// `next` returns the same.
	var nx nextResp
	st = jsonReq(t, "GET", base+"/api/v1/projects/demo/tasks/next", nil, &nx)
	require.Equal(t, http.StatusOK, st)
	require.NotNil(t, nx.Task)
	require.Equal(t, t1.ID, nx.Task.ID)

	// Mark t1 done → t2 unblocks and outranks because of priority.
	attachTestPullRequest(t, base, t1.ID)
	var updated task
	st = jsonReq(t, "PATCH", base+"/api/v1/tasks/"+t1.ID,
		map[string]any{"state": "done"}, &updated)
	require.Equal(t, http.StatusOK, st)

	st = jsonReq(t, "GET", base+"/api/v1/projects/demo/tasks/runnable", nil, &runnable)
	require.Equal(t, http.StatusOK, st)
	require.Len(t, runnable.Tasks, 1)
	require.Equal(t, t2.ID, runnable.Tasks[0].ID)

	// State filter — t2 stayed in `start`; t1 was advanced to `done`.
	var startOnly taskListResp
	st = jsonReq(t, "GET", base+"/api/v1/projects/demo/tasks?state=start", nil, &startOnly)
	require.Equal(t, http.StatusOK, st)
	require.Len(t, startOnly.Tasks, 1)
	require.Equal(t, t2.ID, startOnly.Tasks[0].ID)

	// Description update + delete.
	st = jsonReq(t, "PATCH", base+"/api/v1/tasks/"+t2.ID,
		map[string]any{"description": "updated"}, &updated)
	require.Equal(t, http.StatusOK, st)
	require.Equal(t, "updated", updated.Description)

	st = jsonReq(t, "DELETE", base+"/api/v1/tasks/"+t2.ID, nil, nil)
	require.Equal(t, http.StatusOK, st)

	var allTasks taskListResp
	st = jsonReq(t, "GET", base+"/api/v1/projects/demo/tasks", nil, &allTasks)
	require.Equal(t, http.StatusOK, st)
	require.Len(t, allTasks.Tasks, 1)
	require.Equal(t, t1.ID, allTasks.Tasks[0].ID)
}

func TestTaskSearchEndpoint(t *testing.T) {
	base, stop := startServer(t)
	defer stop()

	var p project
	st := jsonReq(t, "POST", base+"/api/v1/projects",
		map[string]any{"name": "demo", "description": "search"}, &p)
	require.Equal(t, http.StatusCreated, st)

	var eval, docs task
	st = jsonReq(t, "POST", base+"/api/v1/projects/demo/tasks",
		map[string]any{
			"title":       "Agent capability evaluation harness",
			"description": "Tools, sandbox, and hooks coverage",
			"type":        "feature",
			"priority":    1,
			"auto_start":  true,
			"labels":      []string{"evaluation"},
		}, &eval)
	require.Equal(t, http.StatusCreated, st)
	st = jsonReq(t, "POST", base+"/api/v1/projects/demo/tasks",
		map[string]any{
			"title":       "Task search command",
			"description": "Find historical task context",
			"type":        "feature",
			"priority":    5,
			"auto_start":  true,
		}, &docs)
	require.Equal(t, http.StatusCreated, st)

	var byShortID taskListResp
	st = jsonReq(t, "GET", base+"/api/v1/projects/demo/tasks/search?q="+eval.ID[:8], nil, &byShortID)
	require.Equal(t, http.StatusOK, st)
	require.Len(t, byShortID.Tasks, 1)
	require.Equal(t, eval.ID, byShortID.Tasks[0].ID)

	var byKeyword taskListResp
	q := url.QueryEscape("historical context")
	st = jsonReq(t, "GET", base+"/api/v1/projects/demo/tasks/search?q="+q+"&limit=1", nil, &byKeyword)
	require.Equal(t, http.StatusOK, st)
	require.Len(t, byKeyword.Tasks, 1)
	require.Equal(t, docs.ID, byKeyword.Tasks[0].ID)
}

func TestDocsTaskTypeRoundTrip(t *testing.T) {
	base, stop := startServer(t)
	defer stop()

	var p project
	st := jsonReq(t, "POST", base+"/api/v1/projects",
		map[string]any{"name": "docsproj"}, &p)
	require.Equal(t, http.StatusCreated, st)

	var created task
	st = jsonReq(t, "POST", base+"/api/v1/projects/docsproj/tasks",
		map[string]any{"title": "refresh docs", "type": "docs", "auto_start": true}, &created)
	require.Equal(t, http.StatusCreated, st)
	require.Equal(t, "docs", created.Type)

	var updated task
	st = jsonReq(t, "PATCH", base+"/api/v1/tasks/"+created.ID,
		map[string]any{"type": "docs"}, &updated)
	require.Equal(t, http.StatusOK, st)
	require.Equal(t, "docs", updated.Type)

	var got task
	st = jsonReq(t, "GET", base+"/api/v1/tasks/"+created.ID, nil, &got)
	require.Equal(t, http.StatusOK, st)
	require.Equal(t, "docs", got.Type)
}

func TestImageUploadEndToEnd(t *testing.T) {
	base, stop := startServer(t)
	defer stop()

	var p project
	jsonReq(t, "POST", base+"/api/v1/projects",
		map[string]any{"name": "imgproj"}, &p)
	var tk task
	jsonReq(t, "POST", base+"/api/v1/projects/imgproj/tasks",
		map[string]any{"title": "with image", "type": "feature", "auto_start": true}, &tk)

	tmp := t.TempDir()
	fp := filepath.Join(tmp, "hello.txt")
	require.NoError(t, os.WriteFile(fp, []byte("hello world"), 0o644))

	// Multipart upload.
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	fw, err := w.CreateFormFile("file", "hello.txt")
	require.NoError(t, err)
	_, err = fw.Write([]byte("hello world"))
	require.NoError(t, err)
	require.NoError(t, w.Close())

	req, err := http.NewRequest("POST", base+"/api/v1/tasks/"+tk.ID+"/images", &body)
	require.NoError(t, err)
	req.Header.Set("Content-Type", w.FormDataContentType())
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	raw, _ := io.ReadAll(resp.Body)
	require.True(t, strings.Contains(string(raw), "hello.txt"))
	var uploaded image
	require.NoError(t, json.Unmarshal(raw, &uploaded), "decode uploaded image: %s", string(raw))
	require.NotEmpty(t, uploaded.ID)
	require.Equal(t, "/api/v1/images/"+uploaded.ID, uploaded.URL)

	// Re-fetch the task — image should be attached.
	var got task
	jsonReq(t, "GET", base+"/api/v1/tasks/"+tk.ID, nil, &got)
	require.Len(t, got.Images, 1)
	require.Equal(t, uploaded.ID, got.Images[0].ID)
	require.Equal(t, "hello.txt", got.Images[0].Filename)
	require.Equal(t, uploaded.URL, got.Images[0].URL)

	var listed taskListResp
	jsonReq(t, "GET", base+"/api/v1/projects/imgproj/tasks", nil, &listed)
	require.Len(t, listed.Tasks, 1)
	require.Len(t, listed.Tasks[0].Images, 1)
	require.Equal(t, uploaded.URL, listed.Tasks[0].Images[0].URL)

	var next nextResp
	jsonReq(t, "GET", base+"/api/v1/projects/imgproj/tasks/next", nil, &next)
	require.NotNil(t, next.Task)
	require.Len(t, next.Task.Images, 1)
	require.Equal(t, uploaded.URL, next.Task.Images[0].URL)

	var updated task
	jsonReq(t, "PATCH", base+"/api/v1/tasks/"+tk.ID, map[string]any{"priority": 3}, &updated)
	require.Len(t, updated.Images, 1)
	require.Equal(t, uploaded.URL, updated.Images[0].URL)

	// Download the image content for preview.
	resp2, err := http.Get(base + uploaded.URL)
	require.NoError(t, err)
	defer resp2.Body.Close()
	require.Equal(t, http.StatusOK, resp2.StatusCode)
	rawImage, _ := io.ReadAll(resp2.Body)
	require.Equal(t, []byte("hello world"), rawImage)
	require.Contains(t, resp2.Header.Get("Content-Disposition"), "hello.txt")

	// Delete removes the record and makes the content unavailable.
	delReq, err := http.NewRequest("DELETE", base+"/api/v1/images/"+uploaded.ID, nil)
	require.NoError(t, err)
	delResp, err := http.DefaultClient.Do(delReq)
	require.NoError(t, err)
	defer delResp.Body.Close()
	require.Equal(t, http.StatusOK, delResp.StatusCode)

	resp3, err := http.Get(base + "/api/v1/images/" + uploaded.ID)
	require.NoError(t, err)
	defer resp3.Body.Close()
	require.Equal(t, http.StatusNotFound, resp3.StatusCode)

	var afterDelete task
	jsonReq(t, "GET", base+"/api/v1/tasks/"+tk.ID, nil, &afterDelete)
	require.Empty(t, afterDelete.Images)
}

func TestDeleteDependencyEndToEnd(t *testing.T) {
	base, stop := startServer(t)
	defer stop()

	jsonReq(t, "POST", base+"/api/v1/projects",
		map[string]any{"name": "deps"}, &project{})
	var a, b task
	st := jsonReq(t, "POST", base+"/api/v1/projects/deps/tasks",
		map[string]any{"title": "a", "type": "feature", "auto_start": true}, &a)
	require.Equal(t, http.StatusCreated, st)
	st = jsonReq(t, "POST", base+"/api/v1/projects/deps/tasks",
		map[string]any{"title": "b", "type": "feature", "auto_start": true}, &b)
	require.Equal(t, http.StatusCreated, st)

	st = jsonReq(t, "POST", base+"/api/v1/tasks/"+b.ID+"/deps",
		map[string]any{"depends_on": a.ID}, nil)
	require.Equal(t, http.StatusCreated, st)

	var withDep task
	st = jsonReq(t, "GET", base+"/api/v1/tasks/"+b.ID, nil, &withDep)
	require.Equal(t, http.StatusOK, st)
	require.Equal(t, []string{a.ID}, withDep.DependsOn)

	st = jsonReq(t, "DELETE", base+"/api/v1/tasks/"+b.ID+"/deps/"+a.ID, nil, nil)
	require.Equal(t, http.StatusOK, st)

	var withoutDep task
	st = jsonReq(t, "GET", base+"/api/v1/tasks/"+b.ID, nil, &withoutDep)
	require.Equal(t, http.StatusOK, st)
	require.Empty(t, withoutDep.DependsOn)

	var runnable taskListResp
	st = jsonReq(t, "GET", base+"/api/v1/projects/deps/tasks/runnable", nil, &runnable)
	require.Equal(t, http.StatusOK, st)
	require.Len(t, runnable.Tasks, 2)
}

func TestCycleProtectionViaHTTP(t *testing.T) {
	base, stop := startServer(t)
	defer stop()
	jsonReq(t, "POST", base+"/api/v1/projects", map[string]any{"name": "cyc"}, &project{})
	var a, b task
	jsonReq(t, "POST", base+"/api/v1/projects/cyc/tasks",
		map[string]any{"title": "a", "type": "feature"}, &a)
	jsonReq(t, "POST", base+"/api/v1/projects/cyc/tasks",
		map[string]any{"title": "b", "type": "feature"}, &b)

	// b -> a is fine.
	st := jsonReq(t, "POST", base+"/api/v1/tasks/"+b.ID+"/deps",
		map[string]any{"depends_on": a.ID}, nil)
	require.Equal(t, http.StatusCreated, st)

	// a -> b would close a cycle. Service maps store.ErrConflict to 409.
	st = jsonReq(t, "POST", base+"/api/v1/tasks/"+a.ID+"/deps",
		map[string]any{"depends_on": b.ID}, nil)
	require.Equal(t, http.StatusConflict, st)
}

func TestStateTransitionAtAPI(t *testing.T) {
	base, stop := startServer(t)
	defer stop()
	jsonReq(t, "POST", base+"/api/v1/projects", map[string]any{"name": "states"}, &project{})
	var tk task
	jsonReq(t, "POST", base+"/api/v1/projects/states/tasks",
		map[string]any{"title": "x", "type": "feature"}, &tk)
	attachTestPullRequest(t, base, tk.ID)

	// Forward jump.
	st := jsonReq(t, "PATCH", base+"/api/v1/tasks/"+tk.ID,
		map[string]any{"state": "review"}, &tk)
	require.Equal(t, http.StatusOK, st)
	require.Equal(t, "review", tk.State)

	// Backward move — accepted (workflow is no longer forward-only).
	st = jsonReq(t, "PATCH", base+"/api/v1/tasks/"+tk.ID,
		map[string]any{"state": "dev"}, &tk)
	require.Equal(t, http.StatusOK, st)
	require.Equal(t, "dev", tk.State)

	// test is a first-class local verification stage before review.
	st = jsonReq(t, "PATCH", base+"/api/v1/tasks/"+tk.ID,
		map[string]any{"state": "test"}, &tk)
	require.Equal(t, http.StatusOK, st)
	require.Equal(t, "test", tk.State)

	var testOnly taskListResp
	st = jsonReq(t, "GET", base+"/api/v1/projects/states/tasks?state=test", nil, &testOnly)
	require.Equal(t, http.StatusOK, st)
	require.Len(t, testOnly.Tasks, 1)
	require.Equal(t, tk.ID, testOnly.Tasks[0].ID)

	// Retired state name — design was renamed to spec.
	st = jsonReq(t, "PATCH", base+"/api/v1/tasks/"+tk.ID,
		map[string]any{"state": "design"}, nil)
	require.Equal(t, http.StatusBadRequest, st)
}

func TestStateEntryEvidenceErrorsAtAPI(t *testing.T) {
	verifier := &mutablePullRequestVerifier{status: service.PullRequestStatus{
		State:                       service.PullRequestOpen,
		UnresolvedP0P1ReviewThreads: 1,
		CheckRollupState:            service.CheckRollupSuccess,
	}}
	base, stop := startServerWithVerifier(t, verifier)
	defer stop()
	jsonReq(t, http.MethodPost, base+"/api/v1/projects", map[string]any{"name": "evidence"}, &project{})
	var tk task
	jsonReq(t, http.MethodPost, base+"/api/v1/projects/evidence/tasks",
		map[string]any{"title": "guarded", "type": "feature", "auto_start": true}, &tk)

	status, body := jsonReqError(t, http.MethodPatch, base+"/api/v1/tasks/"+tk.ID, map[string]any{"state": "review"})
	require.Equal(t, http.StatusConflict, status)
	require.Contains(t, body, "attach a valid GitHub PR")
	require.Contains(t, body, "taskline task link "+tk.ID)

	attachTestPullRequest(t, base, tk.ID)
	status = jsonReq(t, http.MethodPatch, base+"/api/v1/tasks/"+tk.ID, map[string]any{"state": "review"}, &tk)
	require.Equal(t, http.StatusOK, status)

	status, body = jsonReqError(t, http.MethodPatch, base+"/api/v1/tasks/"+tk.ID, map[string]any{"state": "done", "force": true})
	require.Equal(t, http.StatusConflict, status)
	require.Contains(t, body, "has not been merged")
	require.Contains(t, body, "1 unresolved P0/P1 review threads")
	require.Contains(t, body, "resolve P0/P1 review comments, wait for CI, merge the PR")

	verifier.status = service.PullRequestStatus{
		State:                       service.PullRequestMerged,
		Merged:                      true,
		UnresolvedP0P1ReviewThreads: 1,
		CheckRollupState:            service.CheckRollupSuccess,
	}
	status, body = jsonReqError(t, http.MethodPatch, base+"/api/v1/tasks/"+tk.ID, map[string]any{"state": "done"})
	require.Equal(t, http.StatusConflict, status)
	require.Contains(t, body, "1 unresolved P0/P1 review threads")

	verifier.status.UnresolvedP0P1ReviewThreads = 0
	status = jsonReq(t, http.MethodPatch, base+"/api/v1/tasks/"+tk.ID, map[string]any{"state": "done"}, &tk)
	require.Equal(t, http.StatusOK, status)
	require.Equal(t, "done", tk.State)

	verifier.err = fmt.Errorf("rate limited")
	status, body = jsonReqError(t, http.MethodPatch, base+"/api/v1/tasks/"+tk.ID, map[string]any{"state": "review"})
	require.Equal(t, http.StatusServiceUnavailable, status)
	require.Contains(t, body, "state entry verification unavailable")
	require.Contains(t, body, "rate limited")
}

func TestAutoStartDefaultsToPendingAndExcludesFromRunnable(t *testing.T) {
	base, stop := startServer(t)
	defer stop()
	jsonReq(t, "POST", base+"/api/v1/projects", map[string]any{"name": "parked"}, &project{})

	// Omitted auto_start → server parks the task in `pending`.
	var parked task
	st := jsonReq(t, "POST", base+"/api/v1/projects/parked/tasks",
		map[string]any{"title": "later", "type": "feature"}, &parked)
	require.Equal(t, http.StatusCreated, st)
	require.Equal(t, "pending", parked.State)

	// Runnable list must skip it; `task next` must return null.
	var rs taskListResp
	jsonReq(t, "GET", base+"/api/v1/projects/parked/tasks/runnable", nil, &rs)
	require.Len(t, rs.Tasks, 0)
	var nx nextResp
	jsonReq(t, "GET", base+"/api/v1/projects/parked/tasks/next", nil, &nx)
	require.Nil(t, nx.Task)

	// auto_start=true short-circuits the parking lot.
	var hot task
	jsonReq(t, "POST", base+"/api/v1/projects/parked/tasks",
		map[string]any{"title": "now", "type": "feature", "auto_start": true}, &hot)
	require.Equal(t, "start", hot.State)
	jsonReq(t, "GET", base+"/api/v1/projects/parked/tasks/runnable", nil, &rs)
	require.Len(t, rs.Tasks, 1)
	require.Equal(t, hot.ID, rs.Tasks[0].ID)

	// Promoting `parked` into a runnable state unblocks it too.
	jsonReq(t, "PATCH", base+"/api/v1/tasks/"+parked.ID, map[string]any{"state": "spec"}, &parked)
	require.Equal(t, "spec", parked.State)
	jsonReq(t, "GET", base+"/api/v1/projects/parked/tasks/runnable", nil, &rs)
	require.Len(t, rs.Tasks, 2)

	// And dropping a runnable task back into pending re-parks it.
	jsonReq(t, "PATCH", base+"/api/v1/tasks/"+hot.ID, map[string]any{"state": "pending"}, &hot)
	require.Equal(t, "pending", hot.State)
	jsonReq(t, "GET", base+"/api/v1/projects/parked/tasks/runnable", nil, &rs)
	require.Len(t, rs.Tasks, 1)
	require.Equal(t, parked.ID, rs.Tasks[0].ID)
}

func TestTaskClaimLeaseAtAPI(t *testing.T) {
	base, stop := startServer(t)
	defer stop()
	agentAToken := registerAgent(t, base, "agent-a")
	agentBToken := registerAgent(t, base, "agent-b")
	jsonReq(t, "POST", base+"/api/v1/projects", map[string]any{"name": "claims"}, &project{})
	for _, title := range []string{"first", "second"} {
		var created task
		st := jsonReq(t, "POST", base+"/api/v1/projects/claims/tasks",
			map[string]any{"title": title, "type": "feature", "auto_start": true}, &created)
		require.Equal(t, http.StatusCreated, st)
	}

	st := jsonReq(t, "GET", base+"/api/v1/projects/claims/tasks/next?claim=true", nil, nil)
	require.Equal(t, http.StatusUnauthorized, st)

	var a nextResp
	st = jsonReqWithToken(t, "GET", base+"/api/v1/projects/claims/tasks/next?claim=true&lease=1h", nil, &a, agentAToken)
	require.Equal(t, http.StatusOK, st)
	require.NotNil(t, a.Task)
	require.Equal(t, "agent-a", a.Task.Owner)
	require.NotZero(t, a.Task.ClaimedAt)
	require.NotZero(t, a.Task.LeaseExpiresAt)

	var preview nextResp
	st = jsonReq(t, "GET", base+"/api/v1/projects/claims/tasks/next", nil, &preview)
	require.Equal(t, http.StatusOK, st)
	require.NotNil(t, preview.Task)
	require.NotEqual(t, a.Task.ID, preview.Task.ID)
	require.Empty(t, preview.Task.Owner)

	st = jsonReq(t, "GET", base+"/api/v1/projects/claims/tasks/next?owner=agent-a", nil, &preview)
	require.Equal(t, http.StatusOK, st)
	require.NotNil(t, preview.Task)
	require.NotEqual(t, a.Task.ID, preview.Task.ID)
	require.Empty(t, preview.Task.Owner)

	st = jsonReqWithToken(t, "GET", base+"/api/v1/projects/claims/tasks/next", nil, &preview, agentAToken)
	require.Equal(t, http.StatusOK, st)
	require.NotNil(t, preview.Task)
	require.Equal(t, a.Task.ID, preview.Task.ID)
	require.Equal(t, "agent-a", preview.Task.Owner)

	var b nextResp
	st = jsonReqWithToken(t, "GET", base+"/api/v1/projects/claims/tasks/next?claim=true&lease=1h", nil, &b, agentBToken)
	require.Equal(t, http.StatusOK, st)
	require.NotNil(t, b.Task)
	require.NotEqual(t, a.Task.ID, b.Task.ID)
	require.Equal(t, "agent-b", b.Task.Owner)

	st = jsonReqWithToken(t, "PATCH", base+"/api/v1/tasks/"+a.Task.ID,
		map[string]any{"title": "stolen"}, nil, agentBToken)
	require.Equal(t, http.StatusConflict, st)

	st = jsonReqWithToken(t, "POST", base+"/api/v1/tasks/"+a.Task.ID+"/claim",
		map[string]any{"lease": "1h"}, nil, agentBToken)
	require.Equal(t, http.StatusConflict, st)

	var heartbeat task
	st = jsonReqWithToken(t, "POST", base+"/api/v1/tasks/"+a.Task.ID+"/heartbeat",
		map[string]any{"lease": "2h"}, &heartbeat, agentAToken)
	require.Equal(t, http.StatusOK, st)
	require.Equal(t, "agent-a", heartbeat.Owner)
	require.Greater(t, heartbeat.LeaseExpiresAt, a.Task.LeaseExpiresAt)

	var updated task
	attachTestPullRequest(t, base, a.Task.ID)
	st = jsonReqWithToken(t, "PATCH", base+"/api/v1/tasks/"+a.Task.ID,
		map[string]any{"state": "done", "if_state": "start"}, &updated, agentAToken)
	require.Equal(t, http.StatusOK, st)
	require.Equal(t, "done", updated.State)

	st = jsonReqWithToken(t, "PATCH", base+"/api/v1/tasks/"+b.Task.ID,
		map[string]any{"state": "done", "if_state": "review"}, nil, agentBToken)
	require.Equal(t, http.StatusConflict, st)

	st = jsonReqWithToken(t, "POST", base+"/api/v1/tasks/"+b.Task.ID+"/release",
		map[string]any{}, nil, agentAToken)
	require.Equal(t, http.StatusConflict, st)

	var released task
	st = jsonReq(t, "POST", base+"/api/v1/tasks/"+b.Task.ID+"/release",
		map[string]any{"force": true}, &released)
	require.Equal(t, http.StatusOK, st)
	require.Empty(t, released.Owner)
	require.Zero(t, released.ClaimedAt)
	require.Zero(t, released.LeaseExpiresAt)

	var owned taskListResp
	st = jsonReq(t, "GET", base+"/api/v1/projects/claims/tasks?owner=agent-a", nil, &owned)
	require.Equal(t, http.StatusOK, st)
	require.Len(t, owned.Tasks, 1)
	require.Equal(t, a.Task.ID, owned.Tasks[0].ID)

	var unclaimed taskListResp
	st = jsonReq(t, "GET", base+"/api/v1/projects/claims/tasks?unclaimed=true", nil, &unclaimed)
	require.Equal(t, http.StatusOK, st)
	require.Len(t, unclaimed.Tasks, 1)
	require.Equal(t, b.Task.ID, unclaimed.Tasks[0].ID)
}

func TestLabelFilteredRunnableTasksAtAPI(t *testing.T) {
	base, stop := startServer(t)
	defer stop()
	agentAToken := registerAgent(t, base, "agent-a")
	agentBToken := registerAgent(t, base, "agent-b")
	jsonReq(t, "POST", base+"/api/v1/projects", map[string]any{"name": "label-filters"}, &project{})

	var backendOnly, urgentBackend, urgentFrontend, blocked task
	for _, tc := range []struct {
		out      *task
		title    string
		priority int
		labels   []string
	}{
		{out: &backendOnly, title: "backend", priority: 4, labels: []string{"Backend"}},
		{out: &urgentBackend, title: "urgent backend", priority: 9, labels: []string{"Backend", "Urgent"}},
		{out: &urgentFrontend, title: "urgent frontend", priority: 8, labels: []string{"frontend", "urgent"}},
		{out: &blocked, title: "blocked", priority: 7, labels: []string{"backend", "urgent"}},
	} {
		st := jsonReq(t, "POST", base+"/api/v1/projects/label-filters/tasks",
			map[string]any{"title": tc.title, "type": "feature", "priority": tc.priority, "auto_start": true, "labels": tc.labels}, tc.out)
		require.Equal(t, http.StatusCreated, st)
	}
	st := jsonReq(t, "POST", base+"/api/v1/tasks/"+blocked.ID+"/deps",
		map[string]any{"depends_on": backendOnly.ID}, nil)
	require.Equal(t, http.StatusCreated, st)

	var runnable taskListResp
	st = jsonReq(t, "GET", base+"/api/v1/projects/label-filters/tasks/runnable?label=backend&label=urgent", nil, &runnable)
	require.Equal(t, http.StatusOK, st)
	require.Len(t, runnable.Tasks, 1)
	require.Equal(t, urgentBackend.ID, runnable.Tasks[0].ID)

	var a nextResp
	st = jsonReqWithToken(t, "GET", base+"/api/v1/projects/label-filters/tasks/next?claim=true&label=backend&label=urgent", nil, &a, agentAToken)
	require.Equal(t, http.StatusOK, st)
	require.NotNil(t, a.Task)
	require.Equal(t, urgentBackend.ID, a.Task.ID)
	require.Equal(t, "agent-a", a.Task.Owner)

	var b nextResp
	st = jsonReqWithToken(t, "GET", base+"/api/v1/projects/label-filters/tasks/next?claim=true&label=frontend&label=urgent", nil, &b, agentBToken)
	require.Equal(t, http.StatusOK, st)
	require.NotNil(t, b.Task)
	require.Equal(t, urgentFrontend.ID, b.Task.ID)
	require.Equal(t, "agent-b", b.Task.Owner)

	var done task
	attachTestPullRequest(t, base, a.Task.ID)
	st = jsonReqWithToken(t, "PATCH", base+"/api/v1/tasks/"+a.Task.ID,
		map[string]any{"state": "done"}, &done, agentAToken)
	require.Equal(t, http.StatusOK, st)

	var withEdge task
	st = jsonReq(t, "GET", base+"/api/v1/tasks/"+blocked.ID, nil, &withEdge)
	require.Equal(t, http.StatusOK, st)
	require.Equal(t, []string{backendOnly.ID}, withEdge.DependsOn)

	st = jsonReq(t, "DELETE", base+"/api/v1/tasks/"+blocked.ID+"/deps/"+backendOnly.ID, nil, nil)
	require.Equal(t, http.StatusOK, st)
	st = jsonReq(t, "GET", base+"/api/v1/projects/label-filters/tasks/runnable?label=backend&label=urgent", nil, &runnable)
	require.Equal(t, http.StatusOK, st)
	require.Len(t, runnable.Tasks, 1)
	require.Equal(t, blocked.ID, runnable.Tasks[0].ID)
}

func TestTaskLabelsAtAPI(t *testing.T) {
	base, stop := startServer(t)
	defer stop()
	jsonReq(t, "POST", base+"/api/v1/projects", map[string]any{"name": "labels"}, &project{})

	var tk task
	st := jsonReq(t, "POST", base+"/api/v1/projects/labels/tasks",
		map[string]any{
			"title":      "with labels",
			"type":       "feature",
			"auto_start": true,
			"labels":     []string{" backend ", "UI", "backend"},
		}, &tk)
	require.Equal(t, http.StatusCreated, st)
	require.Equal(t, []string{"backend", "UI"}, tk.Labels)

	var got task
	st = jsonReq(t, "GET", base+"/api/v1/tasks/"+tk.ID, nil, &got)
	require.Equal(t, http.StatusOK, st)
	require.Equal(t, []string{"backend", "UI"}, got.Labels)

	var listed taskListResp
	st = jsonReq(t, "GET", base+"/api/v1/projects/labels/tasks", nil, &listed)
	require.Equal(t, http.StatusOK, st)
	require.Len(t, listed.Tasks, 1)
	require.Equal(t, []string{"backend", "UI"}, listed.Tasks[0].Labels)

	var next nextResp
	st = jsonReq(t, "GET", base+"/api/v1/projects/labels/tasks/next", nil, &next)
	require.Equal(t, http.StatusOK, st)
	require.NotNil(t, next.Task)
	require.Equal(t, []string{"backend", "UI"}, next.Task.Labels)

	st = jsonReq(t, "PATCH", base+"/api/v1/tasks/"+tk.ID,
		map[string]any{
			"label_ops":          map[string]any{"add": []string{"review", "backend"}, "remove": []string{"UI"}},
			"description_append": "first note",
		}, &tk)
	require.Equal(t, http.StatusOK, st)
	require.Equal(t, []string{"backend", "review"}, tk.Labels)
	require.Equal(t, "first note", tk.Description)

	st = jsonReq(t, "PATCH", base+"/api/v1/tasks/"+tk.ID,
		map[string]any{"description_append": "second note"}, &tk)
	require.Equal(t, http.StatusOK, st)
	require.Equal(t, "first note\n\nsecond note", tk.Description)

	st = jsonReq(t, "PATCH", base+"/api/v1/tasks/"+tk.ID,
		map[string]any{"labels": []string{"replace"}, "label_ops": map[string]any{"add": []string{"extra"}}}, nil)
	require.Equal(t, http.StatusBadRequest, st)

	st = jsonReq(t, "PATCH", base+"/api/v1/tasks/"+tk.ID,
		map[string]any{"description": "replace", "description_append": "append"}, nil)
	require.Equal(t, http.StatusBadRequest, st)

	var updated task
	st = jsonReq(t, "PATCH", base+"/api/v1/tasks/"+tk.ID,
		map[string]any{"labels": []string{" review ", "Frontend"}}, &updated)
	require.Equal(t, http.StatusOK, st)
	require.Equal(t, []string{"review", "Frontend"}, updated.Labels)

	st = jsonReq(t, "PATCH", base+"/api/v1/tasks/"+tk.ID,
		map[string]any{"labels": []string{}}, &updated)
	require.Equal(t, http.StatusOK, st)
	require.Empty(t, updated.Labels)

	st = jsonReq(t, "PATCH", base+"/api/v1/tasks/"+tk.ID,
		map[string]any{"labels": []string{"ok", " "}}, nil)
	require.Equal(t, http.StatusBadRequest, st)

	st = jsonReq(t, "PATCH", base+"/api/v1/tasks/"+tk.ID,
		map[string]any{"labels": []string{"ok", "bad,label"}}, nil)
	require.Equal(t, http.StatusBadRequest, st)
}

func TestTaskLinkLifecycleAtAPI(t *testing.T) {
	base, stop := startServer(t)
	defer stop()
	jsonReq(t, "POST", base+"/api/v1/projects", map[string]any{"name": "links"}, &project{})
	var tk task
	jsonReq(t, "POST", base+"/api/v1/projects/links/tasks",
		map[string]any{"title": "with links", "type": "feature", "auto_start": true}, &tk)

	// Add a link.
	var link struct {
		ID     string `json:"id"`
		TaskID string `json:"task_id"`
		URL    string `json:"url"`
		Label  string `json:"label"`
	}
	st := jsonReq(t, "POST", base+"/api/v1/tasks/"+tk.ID+"/links",
		map[string]any{"url": "https://example.com/pr/42", "label": "PR #42"}, &link)
	require.Equal(t, http.StatusCreated, st)
	require.NotEmpty(t, link.ID)
	require.Equal(t, "https://example.com/pr/42", link.URL)

	// Missing URL → 400.
	st = jsonReq(t, "POST", base+"/api/v1/tasks/"+tk.ID+"/links",
		map[string]any{"label": "no url"}, nil)
	require.Equal(t, http.StatusBadRequest, st)

	// javascript: URI must be rejected at the API boundary — it would
	// otherwise round-trip through the web's <a href=…> as a clickable
	// XSS sink.
	st = jsonReq(t, "POST", base+"/api/v1/tasks/"+tk.ID+"/links",
		map[string]any{"url": "javascript:alert(1)"}, nil)
	require.Equal(t, http.StatusBadRequest, st)

	// Link on a missing task → 404. (FK in the store, not a pre-check.)
	st = jsonReq(t, "POST", base+"/api/v1/tasks/no-such-task/links",
		map[string]any{"url": "https://x.test"}, nil)
	require.Equal(t, http.StatusNotFound, st)

	// GET task surfaces the link inline.
	resp, err := http.Get(base + "/api/v1/tasks/" + tk.ID)
	require.NoError(t, err)
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	require.Contains(t, string(raw), "https://example.com/pr/42")
	require.Contains(t, string(raw), "PR #42")

	// Delete the link.
	st = jsonReq(t, "DELETE", base+"/api/v1/links/"+link.ID, nil, nil)
	require.Equal(t, http.StatusOK, st)

	// Re-fetch — links list should be empty.
	resp2, err := http.Get(base + "/api/v1/tasks/" + tk.ID)
	require.NoError(t, err)
	defer resp2.Body.Close()
	raw2, _ := io.ReadAll(resp2.Body)
	require.NotContains(t, string(raw2), "https://example.com/pr/42")

	// Double-delete → 404.
	st = jsonReq(t, "DELETE", base+"/api/v1/links/"+link.ID, nil, nil)
	require.Equal(t, http.StatusNotFound, st)
}

func TestTaskDocLifecycleAtAPI(t *testing.T) {
	base, stop := startServer(t)
	defer stop()
	jsonReq(t, "POST", base+"/api/v1/projects", map[string]any{"name": "docs"}, &project{})
	var tk task
	jsonReq(t, "POST", base+"/api/v1/projects/docs/tasks",
		map[string]any{"title": "with docs", "type": "feature", "auto_start": true}, &tk)

	var created doc
	st := jsonReq(t, "POST", base+"/api/v1/tasks/"+tk.ID+"/docs",
		map[string]any{"title": "Spec", "content": "# Product design"}, &created)
	require.Equal(t, http.StatusCreated, st)
	require.NotEmpty(t, created.ID)
	require.Equal(t, tk.ID, created.TaskID)
	require.Equal(t, "Spec", created.Title)
	require.Equal(t, "# Product design", created.Content)
	require.Equal(t, "/api/v1/docs/"+created.ID+"/content", created.URL)

	st = jsonReq(t, "POST", base+"/api/v1/tasks/"+tk.ID+"/docs",
		map[string]any{"content": "missing title"}, nil)
	require.Equal(t, http.StatusBadRequest, st)

	st = jsonReq(t, "POST", base+"/api/v1/tasks/no-such/docs",
		map[string]any{"title": "Missing task", "content": "x"}, nil)
	require.Equal(t, http.StatusNotFound, st)

	var gotTask task
	st = jsonReq(t, "GET", base+"/api/v1/tasks/"+tk.ID, nil, &gotTask)
	require.Equal(t, http.StatusOK, st)
	require.Len(t, gotTask.Docs, 1)
	require.Equal(t, created.ID, gotTask.Docs[0].ID)
	require.Equal(t, created.URL, gotTask.Docs[0].URL)
	require.Empty(t, gotTask.Docs[0].Content)

	var listed taskListResp
	st = jsonReq(t, "GET", base+"/api/v1/projects/docs/tasks", nil, &listed)
	require.Equal(t, http.StatusOK, st)
	require.Len(t, listed.Tasks, 1)
	require.Len(t, listed.Tasks[0].Docs, 1)
	require.Equal(t, created.URL, listed.Tasks[0].Docs[0].URL)

	var next nextResp
	st = jsonReq(t, "GET", base+"/api/v1/projects/docs/tasks/next", nil, &next)
	require.Equal(t, http.StatusOK, st)
	require.NotNil(t, next.Task)
	require.Len(t, next.Task.Docs, 1)

	var fetched doc
	st = jsonReq(t, "GET", base+"/api/v1/docs/"+created.ID, nil, &fetched)
	require.Equal(t, http.StatusOK, st)
	require.Equal(t, created.ID, fetched.ID)
	require.Equal(t, "# Product design", fetched.Content)

	rawResp, err := http.Get(base + created.URL)
	require.NoError(t, err)
	defer rawResp.Body.Close()
	require.Equal(t, http.StatusOK, rawResp.StatusCode)
	require.Contains(t, rawResp.Header.Get("Content-Type"), "text/markdown")
	rawContent, _ := io.ReadAll(rawResp.Body)
	require.Equal(t, "# Product design", string(rawContent))

	var updated doc
	st = jsonReq(t, "PATCH", base+"/api/v1/docs/"+created.ID,
		map[string]any{"title": "Updated Spec", "content": "# Updated"}, &updated)
	require.Equal(t, http.StatusOK, st)
	require.Equal(t, "Updated Spec", updated.Title)
	require.Equal(t, "# Updated", updated.Content)

	rawResp2, err := http.Get(base + updated.URL)
	require.NoError(t, err)
	defer rawResp2.Body.Close()
	updatedRaw, _ := io.ReadAll(rawResp2.Body)
	require.Equal(t, "# Updated", string(updatedRaw))

	st = jsonReq(t, "DELETE", base+"/api/v1/docs/"+created.ID, nil, nil)
	require.Equal(t, http.StatusOK, st)

	st = jsonReq(t, "GET", base+"/api/v1/docs/"+created.ID, nil, nil)
	require.Equal(t, http.StatusNotFound, st)

	var afterDelete task
	st = jsonReq(t, "GET", base+"/api/v1/tasks/"+tk.ID, nil, &afterDelete)
	require.Equal(t, http.StatusOK, st)
	require.Empty(t, afterDelete.Docs)
}

// Sanity: status code for unknown project.
func TestUnknownProjectIs404(t *testing.T) {
	base, stop := startServer(t)
	defer stop()
	st := jsonReq(t, "POST", base+"/api/v1/projects/no-such/tasks",
		map[string]any{"title": "x", "type": "feature"}, nil)
	require.Equal(t, http.StatusNotFound, st)
}

func init() {
	// Quiet Hertz banner on test stdout; failures still print stack traces.
	_ = fmt.Sprintln
}
