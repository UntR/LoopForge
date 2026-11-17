package httpapi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentflow/internal/connect"
	"agentflow/internal/domain"
	"agentflow/internal/engine"
	"agentflow/internal/repo"
	"agentflow/internal/runtime"
	"agentflow/internal/tracker"
)

type typedTaskHTTPFixture struct {
	http         http.Handler
	client       *connect.Client
	owner, other map[string]string
	identity     domain.ConnectorIdentity
	private      connect.State
	directory    string
}

func newTypedTaskHTTPFixture(t *testing.T) typedTaskHTTPFixture {
	t.Helper()
	t.Setenv("AGENTFLOW_AUTH_REQUIRED", "true")
	t.Setenv("AGENTFLOW_LOCAL_LOGIN_USER", "")
	s, err := repo.OpenTest()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	for _, user := range []domain.User{{ID: 42, LoginName: "task-owner"}, {ID: 43, LoginName: "task-other"}} {
		if err := s.PutUser(user); err != nil {
			t.Fatal(err)
		}
		if err := s.PutAPIToken(domain.APIToken{ID: user.LoginName, UserID: user.ID, Name: "synthetic task credential", TokenHash: domain.ConnectorTokenHash(user.LoginName)}); err != nil {
			t.Fatal(err)
		}
	}
	h := New(s, engine.New(s, runtime.NewManager(t.TempDir())), tracker.NewService(), "", "")
	server := httptest.NewServer(h)
	t.Cleanup(server.Close)
	owner := map[string]string{"Authorization": "Bearer task-owner"}
	other := map[string]string{"Authorization": "Bearer task-other"}
	issued := requestWithHeaders(t, h, "POST", "/api/connectors/bind-codes", nil, owner)
	if issued.Code != 201 {
		t.Fatal("cannot issue fixture code", issued.Code)
	}
	var code struct {
		Code string `json:"code"`
	}
	json.Unmarshal(issued.Body.Bytes(), &code)
	client, _ := connect.NewClient(server.URL)
	directory := filepath.Join(t.TempDir(), "profile")
	identity, err := connect.Bind(context.Background(), client, connect.BindOptions{Server: server.URL, Code: code.Code, StateDir: directory, Name: "synthetic task machine", OS: "linux", Arch: "amd64", Agents: []domain.ConnectorAgentEnrollment{{Provider: "codex", Version: "fixture"}, {Provider: "dsh", Version: "fixture"}}})
	if err != nil {
		t.Fatal(err)
	}
	var private connect.State
	body, err := os.ReadFile(filepath.Join(directory, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(body, &private); err != nil {
		t.Fatal(err)
	}
	return typedTaskHTTPFixture{http: h, client: client, owner: owner, other: other, identity: identity, private: private, directory: directory}
}
func submitHTTPTask(t *testing.T, fixture typedTaskHTTPFixture, key string) domain.ConnectorTask {
	t.Helper()
	input := domain.ConnectorTaskRequest{SchemaVersion: 1, IdempotencyKey: key, AgentID: fixture.identity.Agents[0].ID, Kind: "prompt", Prompt: "synthetic report only", Files: []domain.ConnectorTaskFile{{Path: "a.txt", Content: strings.Repeat("x", 48<<10)}, {Path: "b.txt", Content: strings.Repeat("y", 48<<10)}}}
	rec := requestWithHeaders(t, fixture.http, "POST", "/api/connector-tasks", input, fixture.owner)
	if rec.Code != 201 {
		t.Fatalf("submit=%d %s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("input response can be cached")
	}
	var task domain.ConnectorTask
	if err := json.Unmarshal(rec.Body.Bytes(), &task); err != nil {
		t.Fatal(err)
	}
	return task
}

func TestConnectorTasksHTTPAndScopedSDK(t *testing.T) {
	fixture := newTypedTaskHTTPFixture(t)
	ctx := context.Background()
	session, err := connect.OpenAgentSession(ctx, fixture.client, fixture.directory, "codex")
	if err != nil {
		t.Fatal(err)
	}
	otherAgent, err := connect.OpenAgentSession(ctx, fixture.client, fixture.directory, "dsh")
	if err != nil {
		t.Fatal(err)
	}
	task := submitHTTPTask(t, fixture, "request-one")
	retry := requestWithHeaders(t, fixture.http, "POST", "/api/connector-tasks", *task.Input, fixture.owner)
	var retried domain.ConnectorTask
	json.Unmarshal(retry.Body.Bytes(), &retried)
	if retry.Code != 200 || retried.ID != task.ID {
		t.Fatal("HTTP retry created another task")
	}
	changed := *task.Input
	changed.Prompt = "different synthetic content"
	if got := requestWithHeaders(t, fixture.http, "POST", "/api/connector-tasks", changed, fixture.owner); got.Code != 409 {
		t.Fatal("HTTP idempotency conflict not surfaced", got.Code)
	}
	if got := requestWithHeaders(t, fixture.http, "GET", "/api/connector-tasks/"+task.ID, nil, fixture.other); got.Code != 404 {
		t.Fatal("another owner read the task", got.Code)
	}
	if got := requestWithHeaders(t, fixture.http, "POST", "/api/connector-tasks", *task.Input, fixture.other); got.Code != 404 {
		t.Fatal("another owner submitted to Agent", got.Code)
	}
	if got := requestWithHeaders(t, fixture.http, "POST", "/api/connector-tasks/"+task.ID+"/cancel", nil, fixture.other); got.Code != 404 {
		t.Fatal("another owner canceled task", got.Code)
	}
	lease, err := connect.NewTaskLeaseToken()
	if err != nil {
		t.Fatal(err)
	}
	claim, err := session.Claim(ctx, lease)
	if err != nil || claim == nil || claim.ID != task.ID || claim.RunAllowed || claim.Input == nil || len(claim.Input.Files[0].Content) != 48<<10 {
		t.Fatal("bounded input claim failed", err)
	}
	repeat, err := session.Claim(ctx, lease)
	if err != nil || repeat.ID != task.ID {
		t.Fatal("lost claim response not recoverable", err)
	}
	proof := domain.ConnectorTaskLease{Attempt: claim.Attempt, LeaseToken: lease}
	if _, err := otherAgent.Control(ctx, task.ID, proof); err == nil {
		t.Fatal("sibling Agent controlled task")
	}
	start, err := session.Start(ctx, task.ID, proof)
	if err != nil || !start.MayRun() {
		t.Fatal("start did not return a conservative live lease", err)
	}
	if start.RunUntil.Sub(time.Now()) > 45e9 {
		t.Fatal("client run window exceeds lease")
	}
	result := domain.ConnectorTaskResult{Status: domain.ConnectorTaskSucceeded, Text: "synthetic report", Files: []domain.ConnectorTaskFile{{Path: "report.txt", Content: "synthetic artifact"}}, Quiesced: true}
	done, err := session.Complete(ctx, task.ID, proof, result)
	if err != nil || done.Task.Status != domain.ConnectorTaskSucceeded || done.MayRun() {
		t.Fatal("completion failed", err)
	}
	if _, err := session.Complete(ctx, task.ID, proof, result); err != nil {
		t.Fatal("completion retry not idempotent", err)
	}
	eventsResponse := requestWithHeaders(t, fixture.http, "GET", "/api/connector-tasks/"+task.ID+"/events", nil, fixture.owner)
	var events []domain.ConnectorTaskEvent
	if json.Unmarshal(eventsResponse.Body.Bytes(), &events) != nil || eventsResponse.Code != 200 || len(events) != 4 {
		t.Fatal("task audit did not persist state transitions")
	}
	if strings.Contains(eventsResponse.Body.String(), lease) || strings.Contains(eventsResponse.Body.String(), "synthetic report") {
		t.Fatal("task audit leaked payload")
	}
	if hidden := requestWithHeaders(t, fixture.http, "GET", "/api/connector-tasks/"+task.ID+"/events", nil, fixture.other); hidden.Code != 404 {
		t.Fatal("another owner read audit")
	}
	if invalid := requestWithHeaders(t, fixture.http, "GET", "/api/connector-tasks/"+task.ID+"/events?after=invalid", nil, fixture.owner); invalid.Code != 400 {
		t.Fatal("invalid event cursor accepted")
	}
	list := requestWithHeaders(t, fixture.http, "GET", "/api/connector-tasks", nil, fixture.owner)
	if list.Code != 200 || strings.Contains(list.Body.String(), "synthetic report only") || strings.Contains(list.Body.String(), "synthetic artifact") {
		t.Fatal("task list disclosed input/output payload")
	}
	for _, header := range []map[string]string{{"Authorization": "Bearer " + fixture.private.Credential}, {"Authorization": "Bearer " + fixture.private.Agents[0].Credential}} {
		if got := requestWithHeaders(t, fixture.http, "POST", "/api/connector-tasks", *task.Input, header); got.Code != 401 {
			t.Fatal("machine credential became a user task credential", got.Code)
		}
	}
	if got := requestWithHeaders(t, fixture.http, "POST", "/runners/connector-tasks/claim", map[string]string{"leaseToken": lease}, map[string]string{"Authorization": "Bearer " + fixture.private.Credential}); got.Code != 401 {
		t.Fatal("device credential claimed Agent work", got.Code)
	}
}

func TestConnectorTasksHTTPCancelAndStrictPayload(t *testing.T) {
	fixture := newTypedTaskHTTPFixture(t)
	ctx := context.Background()
	session, err := connect.OpenAgentSession(ctx, fixture.client, fixture.directory, "codex")
	if err != nil {
		t.Fatal(err)
	}
	task := submitHTTPTask(t, fixture, "cancel-request")
	lease, _ := connect.NewTaskLeaseToken()
	claim, err := session.Claim(ctx, lease)
	if err != nil || claim == nil {
		t.Fatal(err)
	}
	proof := domain.ConnectorTaskLease{Attempt: claim.Attempt, LeaseToken: lease}
	if _, err := session.Start(ctx, task.ID, proof); err != nil {
		t.Fatal(err)
	}
	cancel := requestWithHeaders(t, fixture.http, "POST", "/api/connector-tasks/"+task.ID+"/cancel", nil, fixture.owner)
	var requested domain.ConnectorTask
	json.Unmarshal(cancel.Body.Bytes(), &requested)
	if cancel.Code != 200 || requested.Status != domain.ConnectorTaskCancelRequested || requested.RunAllowed {
		t.Fatal("cancellation was prematurely final or still authorized")
	}
	control, err := session.Control(ctx, task.ID, proof)
	if err != nil || control.MayRun() || !control.Task.CancelRequested {
		t.Fatal("cancel not delivered", err)
	}
	if _, err := session.Complete(ctx, task.ID, proof, domain.ConnectorTaskResult{Status: domain.ConnectorTaskSucceeded, Quiesced: true}); err == nil {
		t.Fatal("late success replaced cancellation")
	}
	bad := domain.ConnectorTaskCompletion{Attempt: proof.Attempt, LeaseToken: proof.LeaseToken, Result: domain.ConnectorTaskResult{Status: domain.ConnectorTaskCanceled}}
	header := map[string]string{"Authorization": "Bearer " + fixture.private.Agents[0].Credential}
	if got := requestWithHeaders(t, fixture.http, "POST", "/runners/connector-tasks/"+task.ID+"/complete", bad, header); got.Code != 400 {
		t.Fatal("API accepted unconfirmed process stop", got.Code)
	}
	done, err := session.Complete(ctx, task.ID, proof, domain.ConnectorTaskResult{Status: domain.ConnectorTaskCanceled, Quiesced: true})
	if err != nil || done.Task.Status != domain.ConnectorTaskCanceled {
		t.Fatal(err)
	}
	for _, payload := range []string{`{"command":"private-input-must-not-echo"}`, `{} {}`, `{"prompt":"` + strings.Repeat("x", 2<<20) + `"}`} {
		req := httptest.NewRequest("POST", "/api/connector-tasks", strings.NewReader(payload))
		req.Header.Set("Authorization", "Bearer task-owner")
		rec := httptest.NewRecorder()
		fixture.http.ServeHTTP(rec, req)
		if rec.Code != 400 || strings.Contains(rec.Body.String(), "private-input-must-not-echo") {
			t.Fatal("invalid payload accepted or echoed", rec.Code)
		}
	}
}

type taskUnreadBody struct{ read bool }

func (body *taskUnreadBody) Read(_ []byte) (int, error) { body.read = true; return 0, io.EOF }
func (body *taskUnreadBody) Close() error               { return nil }
func TestConnectorTasksRejectUnknownIdentityBeforePayload(t *testing.T) {
	fixture := newTypedTaskHTTPFixture(t)
	credential := domain.ConnectorAgentTokenPrefix + base64.RawURLEncoding.EncodeToString(make([]byte, 32))
	for _, path := range []string{"/runners/connector-tasks/claim", "/runners/connector-tasks/ctask-fixture/complete"} {
		body := &taskUnreadBody{}
		request := httptest.NewRequest("POST", path, body)
		request.Header.Set("Authorization", "Bearer "+credential)
		recorder := httptest.NewRecorder()
		fixture.http.ServeHTTP(recorder, request)
		if recorder.Code != 401 || body.read {
			t.Fatal("unknown Agent reached payload processing", recorder.Code, body.read)
		}
	}
}
