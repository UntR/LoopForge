package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agentflow/internal/connect"
	"agentflow/internal/domain"
	"agentflow/internal/engine"
	"agentflow/internal/repo"
	"agentflow/internal/runtime"
	"agentflow/internal/tracker"
)

func TestConnectorEnrollmentHTTPBoundary(t *testing.T) {
	t.Setenv("AGENTFLOW_AUTH_REQUIRED", "true")
	t.Setenv("AGENTFLOW_LOCAL_LOGIN_USER", "")
	s, err := repo.OpenTest()
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, user := range []domain.User{{ID: 42, LoginName: "fixture-owner"}, {ID: 43, LoginName: "fixture-other"}} {
		if err := s.PutUser(user); err != nil {
			t.Fatal(err)
		}
		if err := s.PutAPIToken(domain.APIToken{ID: user.LoginName, UserID: user.ID, Name: "synthetic", TokenHash: domain.ConnectorTokenHash(user.LoginName)}); err != nil {
			t.Fatal(err)
		}
	}
	handler := New(s, engine.New(s, runtime.NewManager(t.TempDir())), tracker.NewService(), "", "")
	server := httptest.NewServer(handler)
	defer server.Close()
	owner := map[string]string{"Authorization": "Bearer fixture-owner"}
	other := map[string]string{"Authorization": "Bearer fixture-other"}
	rec := requestWithHeaders(t, handler, http.MethodPost, "/api/connectors/bind-codes", nil, nil)
	if rec.Code != 401 {
		t.Fatalf("unauthenticated code issue=%d", rec.Code)
	}
	rec = requestWithHeaders(t, handler, http.MethodPost, "/api/connectors/bind-codes", nil, owner)
	if rec.Code != 201 {
		t.Fatalf("issue=%d %s", rec.Code, rec.Body.String())
	}
	var issued struct {
		Code string `json:"code"`
	}
	json.Unmarshal(rec.Body.Bytes(), &issued)
	if !domain.ValidConnectorToken(issued.Code, domain.ConnectorBindCodePrefix) || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("invalid issuance")
	}
	client, err := connect.NewClient(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	options := connect.BindOptions{Server: server.URL, Code: issued.Code, StateDir: filepath.Join(t.TempDir(), "connector"), Name: "fixture-device", OS: "windows", Arch: "amd64", Agents: []domain.ConnectorAgentEnrollment{{Provider: "codex", Version: "fixture"}, {Provider: "dsh", Version: "fixture"}, {Provider: "minimax-code", Version: "fixture"}}}
	identity, err := connect.Bind(context.Background(), client, options)
	if err != nil || len(identity.Agents) != 3 {
		t.Fatalf("enrollment=%+v %v", identity, err)
	}
	var private struct {
		Credential string                            `json:"credential"`
		Agents     []domain.ConnectorAgentEnrollment `json:"agents"`
	}
	privateBytes, err := os.ReadFile(filepath.Join(options.StateDir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(privateBytes, &private); err != nil {
		t.Fatal(err)
	}
	deviceHeader := map[string]string{"Authorization": "Bearer " + private.Credential}
	for _, agent := range private.Agents {
		rec = requestWithHeaders(t, handler, http.MethodGet, "/runners/connectors/agent", nil, map[string]string{"Authorization": "Bearer " + agent.Credential})
		var verified domain.ConnectorAgent
		json.Unmarshal(rec.Body.Bytes(), &verified)
		if rec.Code != 200 || verified.Provider != agent.Provider {
			t.Fatal("Agent credential did not identify its own Agent", rec.Code)
		}
		rec = requestWithHeaders(t, handler, http.MethodGet, "/runners/connectors/self", nil, map[string]string{"Authorization": "Bearer " + agent.Credential})
		if rec.Code != 401 {
			t.Fatal("Agent credential authenticated as device")
		}
	}
	rec = requestWithHeaders(t, handler, http.MethodGet, "/runners/connectors/agent", nil, deviceHeader)
	if rec.Code != 401 {
		t.Fatal("device credential authenticated as Agent")
	}
	rec = requestWithHeaders(t, handler, http.MethodGet, "/api/connectors", nil, deviceHeader)
	if rec.Code != 401 {
		t.Fatal("device credential authenticated user management")
	}
	options.Code = ""
	recovered, err := connect.Bind(context.Background(), client, options)
	if err != nil || recovered.Device.ID != identity.Device.ID {
		t.Fatal("recovery failed", err)
	}
	rec = requestWithHeaders(t, handler, http.MethodGet, "/api/connectors", nil, owner)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), identity.Device.ID) || strings.Contains(rec.Body.String(), "credential") || strings.Contains(rec.Body.String(), "TokenHash") {
		t.Fatalf("list=%d %s", rec.Code, rec.Body.String())
	}
	rec = requestWithHeaders(t, handler, http.MethodGet, "/api/connectors", nil, other)
	if rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != "[]" {
		t.Fatal("other user saw connector")
	}
	rec = requestWithHeaders(t, handler, http.MethodPost, "/api/connectors/agents/"+identity.Agents[0].ID+"/revoke", nil, other)
	if rec.Code != 404 {
		t.Fatal("other user revoked Agent", rec.Code)
	}
	rec = requestWithHeaders(t, handler, http.MethodPost, "/api/connectors/agents/"+identity.Agents[0].ID+"/revoke", nil, owner)
	if rec.Code != 204 {
		t.Fatal("owner could not revoke Agent", rec.Code)
	}
	for _, agent := range private.Agents {
		rec = requestWithHeaders(t, handler, http.MethodGet, "/runners/connectors/agent", nil, map[string]string{"Authorization": "Bearer " + agent.Credential})
		want := 200
		if agent.Provider == identity.Agents[0].Provider {
			want = 401
		}
		if rec.Code != want {
			t.Fatal("per-Agent revocation crossed identity boundary", agent.Provider, rec.Code)
		}
	}
	rec = requestWithHeaders(t, handler, http.MethodPost, "/api/connectors/devices/"+identity.Device.ID+"/revoke", nil, owner)
	if rec.Code != 204 {
		t.Fatal("owner could not revoke device", rec.Code)
	}
	for _, agent := range private.Agents {
		rec = requestWithHeaders(t, handler, http.MethodGet, "/runners/connectors/agent", nil, map[string]string{"Authorization": "Bearer " + agent.Credential})
		if rec.Code != 401 {
			t.Fatal("parent revocation did not invalidate Agent")
		}
	}
	if _, err := connect.Bind(context.Background(), client, options); err == nil {
		t.Fatal("revoked device recovered")
	}
	for _, path := range []string{"/runners/connectors/self", "/runners/connectors/agent"} {
		rec = requestWithHeaders(t, handler, http.MethodGet, path, nil, nil)
		if rec.Code != 401 {
			t.Fatal("machine endpoint omitted its own auth", path, rec.Code)
		}
	}
}

func TestConnectorEnrollmentRejectsUnknownAndOversizedPayload(t *testing.T) {
	handler := newRunnerTestServer(t)
	for _, payload := range []string{`{"credential":"private-secret","unknown":true}`, `{} {}`, `{"name":"` + strings.Repeat("x", 17<<10) + `"}`} {
		request := httptest.NewRequest(http.MethodPost, "/runners/connectors/register", strings.NewReader(payload))
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		if recorder.Code != 400 || strings.Contains(recorder.Body.String(), "private-secret") {
			t.Fatalf("unsafe response %d %s", recorder.Code, recorder.Body.String())
		}
	}
}
