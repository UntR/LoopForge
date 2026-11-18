package connect

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"agentflow/internal/domain"
)

type taskRoundTrip func(*http.Request) (*http.Response, error)

func (transport taskRoundTrip) RoundTrip(request *http.Request) (*http.Response, error) {
	return transport(request)
}
func sessionTaskFixture(t *testing.T, transform func(*domain.ConnectorTask)) (*AgentSession, domain.ConnectorTaskLease) {
	t.Helper()
	lease := domain.ConnectorLeaseTokenPrefix + base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32))
	credential := domain.ConnectorAgentTokenPrefix + base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{8}, 32))
	proof := domain.ConnectorTaskLease{Attempt: 1, LeaseToken: lease}
	serverClock := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	until, deadline := serverClock.Add(45*time.Second), serverClock.Add(5*time.Minute)
	client := &Client{server: "https://fixture.example", http: &http.Client{Transport: taskRoundTrip(func(request *http.Request) (*http.Response, error) {
		if request.URL.RawQuery != "" || strings.Contains(request.URL.String(), lease) || request.Header.Get("Authorization") != "Bearer "+credential {
			t.Error("lease proof escaped JSON or Agent identity changed")
		}
		var received domain.ConnectorTaskLease
		if err := json.NewDecoder(request.Body).Decode(&received); err != nil || received.Attempt != proof.Attempt || received.LeaseToken != lease {
			t.Error("invalid fenced request")
		}
		task := domain.ConnectorTask{ID: "ctask-fixture", AgentID: "agent-fixture", Provider: "minimax-code", Status: domain.ConnectorTaskRunning, Attempt: 1, RunAllowed: true, LeaseRemainingMillis: 45000, LeaseUntil: &until, DeadlineAt: &deadline}
		if transform != nil {
			transform(&task)
		}
		body, _ := json.Marshal(task)
		return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(body)), Header: make(http.Header)}, nil
	})}}
	return &AgentSession{client: client, credential: credential, agent: domain.ConnectorAgent{ID: "agent-fixture", Provider: "minimax-code"}}, proof
}

func TestConnectorTaskObservationUsesMonotonicConservativeLease(t *testing.T) {
	session, proof := sessionTaskFixture(t, nil)
	observation, err := session.Start(context.Background(), "ctask-fixture", proof)
	if err != nil || !observation.MayRun() {
		t.Fatal("server/host wall-clock skew incorrectly determined execution permission", err)
	}
	remaining := time.Until(observation.RunUntil)
	if remaining <= 0 || remaining > 43*time.Second {
		t.Fatal("round trip and safety margin not subtracted", remaining)
	}
	session, proof = sessionTaskFixture(t, func(task *domain.ConnectorTask) { task.LeaseRemainingMillis = 100 })
	short, err := session.Renew(context.Background(), "ctask-fixture", proof)
	if err != nil || short.MayRun() {
		t.Fatal("insufficient safe lease allowed work", err)
	}
	session, proof = sessionTaskFixture(t, func(task *domain.ConnectorTask) {
		task.Status = domain.ConnectorTaskCancelRequested
		task.RunAllowed = false
		task.CancelRequested = true
	})
	stop, err := session.Control(context.Background(), "ctask-fixture", proof)
	if err != nil || stop.MayRun() {
		t.Fatal("cancel did not invalidate run window", err)
	}
}

func TestConnectorTaskSDKRejectsWrongIdentityAttemptAndUnsafeInput(t *testing.T) {
	for _, mutate := range []func(*domain.ConnectorTask){
		func(task *domain.ConnectorTask) { task.AgentID = "another-agent" }, func(task *domain.ConnectorTask) { task.Attempt = 2 }, func(task *domain.ConnectorTask) { task.LeaseRemainingMillis = 1 << 62 }, func(task *domain.ConnectorTask) { task.Status = domain.ConnectorTaskClaimed },
		func(task *domain.ConnectorTask) {
			task.Input = &domain.ConnectorTaskRequest{SchemaVersion: 1, AgentID: "agent-fixture", IdempotencyKey: "fixture", Kind: "prompt", Prompt: "fixture", Files: []domain.ConnectorTaskFile{{Path: "../outside"}}}
		},
	} {
		session, proof := sessionTaskFixture(t, mutate)
		if observation, err := session.Start(context.Background(), "ctask-fixture", proof); err == nil || observation.MayRun() {
			t.Fatal("accepted malformed or differently scoped task")
		}
	}
	session, proof := sessionTaskFixture(t, nil)
	if _, err := session.Control(context.Background(), "../outside", proof); err == nil {
		t.Fatal("accepted task path injection")
	}
	if _, err := session.Claim(context.Background(), "not-a-random-lease"); err == nil {
		t.Fatal("accepted malformed claim proof")
	}
}

func TestNewConnectorTaskLeaseIsIndependent(t *testing.T) {
	one, err := NewTaskLeaseToken()
	if err != nil {
		t.Fatal(err)
	}
	two, err := NewTaskLeaseToken()
	if err != nil || one == two || !domain.ValidConnectorToken(one, domain.ConnectorLeaseTokenPrefix) {
		t.Fatal("lease generation failed", err)
	}
}

func TestConnectorTaskStatusErrorsAreTypedAndDoNotEchoServerBody(t *testing.T) {
	session, proof := sessionTaskFixture(t, nil)
	session.client.http.Transport = taskRoundTrip(func(request *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 401, Body: io.NopCloser(strings.NewReader("private-server-diagnostic")), Header: make(http.Header)}, nil
	})
	_, err := session.Control(context.Background(), "ctask-fixture", proof)
	var status *RequestStatusError
	if !errors.As(err, &status) || status.StatusCode != 401 || strings.Contains(err.Error(), "private-server-diagnostic") {
		t.Fatal("invalid typed/redacted status error", err)
	}
}
