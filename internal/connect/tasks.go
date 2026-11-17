package connect

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"agentflow/internal/domain"
)

// AgentSession carries one identity, never the parent device credential.
// It provides protocol operations only; it does not launch an Agent or claim
// that same-user native child processes are isolated from connector secrets.
type AgentSession struct {
	client     *Client
	credential string
	agent      domain.ConnectorAgent
}

type TaskObservation struct {
	Task domain.ConnectorTask
	// RunUntil uses the client's monotonic clock after subtracting the entire
	// request round-trip and a safety margin from the server's remaining lease.
	// Never persist/reuse this as execution authority after a process restart.
	RunUntil time.Time
}

func (observation TaskObservation) MayRun() bool {
	return observation.Task.RunAllowed && !observation.RunUntil.IsZero() && time.Now().Before(observation.RunUntil)
}

func OpenAgentSession(ctx context.Context, client *Client, directory, provider string) (*AgentSession, error) {
	if _, err := Select(provider); err != nil || provider == "" || provider == "all" || strings.Contains(provider, ",") {
		return nil, errors.New("choose one supported Agent identity")
	}
	if err := secureStateDirectory(directory); err != nil {
		return nil, err
	}
	unlock, err := lockState(directory)
	if err != nil {
		return nil, err
	}
	defer unlock()
	state, err := loadState(directory)
	if err != nil {
		return nil, err
	}
	if state.Server != client.server || state.Identity == nil || validateState(state) != nil {
		return nil, errors.New("complete binding to this server before opening an Agent session")
	}
	var credential string
	var saved domain.ConnectorAgent
	for _, agent := range state.Agents {
		if agent.Provider == provider {
			credential = agent.Credential
		}
	}
	for _, agent := range state.Identity.Agents {
		if agent.Provider == provider {
			saved = agent
		}
	}
	if credential == "" || saved.ID == "" || saved.Revoked {
		return nil, errors.New("Agent identity is unavailable in this profile")
	}
	var current domain.ConnectorAgent
	if _, err := client.request(ctx, "GET", "/runners/connectors/agent", credential, nil, &current); err != nil {
		return nil, err
	}
	if current.ID != saved.ID || current.DeviceID != state.Identity.Device.ID || current.Provider != provider || current.Revoked {
		return nil, errors.New("server Agent identity does not match the bound profile")
	}
	return &AgentSession{client: client, credential: credential, agent: current}, nil
}

func (session *AgentSession) Identity() domain.ConnectorAgent { return session.agent }
func NewTaskLeaseToken() (string, error) {
	return randomConnectorToken(domain.ConnectorLeaseTokenPrefix)
}

// The caller must persist this fresh random proof before sending Claim. Reuse
// the same proof to reconcile an uncertain response, not to start another job.
func (session *AgentSession) Claim(ctx context.Context, leaseToken string) (*domain.ConnectorTask, error) {
	if !domain.ValidConnectorToken(leaseToken, domain.ConnectorLeaseTokenPrefix) {
		return nil, errors.New("invalid task lease proof")
	}
	body, _ := json.Marshal(struct {
		LeaseToken string `json:"leaseToken"`
	}{leaseToken})
	var response struct {
		Task *domain.ConnectorTask `json:"task"`
	}
	if _, err := session.client.requestBounded(ctx, "POST", "/runners/connector-tasks/claim", session.credential, bytes.NewReader(body), &response, 8<<20); err != nil {
		return nil, err
	}
	if response.Task != nil {
		if err := session.validateTask(response.Task); err != nil {
			return nil, err
		}
	}
	return response.Task, nil
}
func (session *AgentSession) Start(ctx context.Context, id string, proof domain.ConnectorTaskLease) (TaskObservation, error) {
	return session.action(ctx, id, "start", proof, nil)
}
func (session *AgentSession) Renew(ctx context.Context, id string, proof domain.ConnectorTaskLease) (TaskObservation, error) {
	return session.action(ctx, id, "renew", proof, nil)
}
func (session *AgentSession) Control(ctx context.Context, id string, proof domain.ConnectorTaskLease) (TaskObservation, error) {
	return session.action(ctx, id, "control", proof, nil)
}
func (session *AgentSession) Complete(ctx context.Context, id string, proof domain.ConnectorTaskLease, result domain.ConnectorTaskResult) (TaskObservation, error) {
	normalized, err := domain.NormalizeConnectorTaskResult(result)
	if err != nil {
		return TaskObservation{}, err
	}
	observation, err := session.action(ctx, id, "complete", proof, &normalized)
	if err != nil {
		return observation, err
	}
	if observation.Task.Result == nil || observation.Task.Status != normalized.Status {
		return TaskObservation{}, errors.New("server did not confirm the submitted terminal result")
	}
	got, _ := json.Marshal(observation.Task.Result)
	expected, _ := json.Marshal(normalized)
	if string(got) != string(expected) {
		return TaskObservation{}, errors.New("server terminal result differs from submitted output")
	}
	return observation, nil
}

func validConnectorTaskID(id string) bool {
	if !strings.HasPrefix(id, "ctask-") || len(id) > 64 {
		return false
	}
	for _, r := range id {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
			return false
		}
	}
	return len(id) > 6
}
func (session *AgentSession) action(ctx context.Context, id, action string, proof domain.ConnectorTaskLease, result *domain.ConnectorTaskResult) (TaskObservation, error) {
	var empty TaskObservation
	if !validConnectorTaskID(id) || proof.Attempt < 1 || !domain.ValidConnectorToken(proof.LeaseToken, domain.ConnectorLeaseTokenPrefix) {
		return empty, errors.New("invalid fenced task operation")
	}
	var body []byte
	if result == nil {
		body, _ = json.Marshal(proof)
	} else {
		body, _ = json.Marshal(domain.ConnectorTaskCompletion{Attempt: proof.Attempt, LeaseToken: proof.LeaseToken, Result: *result})
	}
	var task domain.ConnectorTask
	before := time.Now()
	_, err := session.client.requestBounded(ctx, "POST", "/runners/connector-tasks/"+id+"/"+action, session.credential, bytes.NewReader(body), &task, 8<<20)
	after := time.Now()
	if err != nil {
		return empty, err
	}
	if task.ID != id || task.Attempt != proof.Attempt {
		return empty, errors.New("server returned a different task attempt")
	}
	if err := session.validateTask(&task); err != nil {
		return empty, err
	}
	observation := TaskObservation{Task: task}
	remaining := time.Duration(task.LeaseRemainingMillis)*time.Millisecond - after.Sub(before) - 2*time.Second
	if task.RunAllowed && remaining > 0 {
		observation.RunUntil = after.Add(remaining)
	}
	return observation, nil
}
func (session *AgentSession) validateTask(task *domain.ConnectorTask) error {
	invalid := errors.New("server returned an invalid or differently scoped task")
	if !validConnectorTaskID(task.ID) || task.AgentID != session.agent.ID || task.Provider != session.agent.Provider || task.Attempt < 1 || task.LeaseRemainingMillis < 0 || task.LeaseRemainingMillis > 45000 {
		return invalid
	}
	switch task.Status {
	case domain.ConnectorTaskClaimed, domain.ConnectorTaskRunning, domain.ConnectorTaskCancelRequested, domain.ConnectorTaskRecoveryRequired, domain.ConnectorTaskSucceeded, domain.ConnectorTaskFailed, domain.ConnectorTaskCanceled:
	default:
		return invalid
	}
	if task.RunAllowed && (task.Status != domain.ConnectorTaskRunning || task.CancelRequested || task.LeaseRemainingMillis == 0 || task.LeaseUntil == nil || task.DeadlineAt == nil) {
		return invalid
	}
	if task.Input != nil {
		if task.Input.AgentID != session.agent.ID {
			return invalid
		}
		normalized, err := domain.NormalizeConnectorTaskRequest(*task.Input)
		if err != nil {
			return invalid
		}
		task.Input = &normalized
	}
	if task.Status == domain.ConnectorTaskClaimed && task.Input == nil {
		return invalid
	}
	if task.Result != nil {
		normalized, err := domain.NormalizeConnectorTaskResult(*task.Result)
		if err != nil || normalized.Status != task.Status {
			return invalid
		}
		task.Result = &normalized
	}
	return nil
}
