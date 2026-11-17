package repo

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"agentflow/internal/domain"
	"gorm.io/gorm"
)

func taskFixture(t *testing.T) (*Store, time.Time, domain.ConnectorIdentity, []string) {
	t.Helper()
	s, now := connectorTestStore(t)
	input := connectorFixture()
	issueConnectorFixture(t, s, input, 42, now)
	identity, err := s.EnrollConnector(input, "task-device", []string{"task-agent-a", "task-agent-b", "task-agent-c"}, now)
	if err != nil {
		t.Fatal(err)
	}
	hashes := []string{}
	for _, agent := range input.Agents {
		hashes = append(hashes, domain.ConnectorTokenHash(agent.Credential))
	}
	return s, now, identity, hashes
}
func typedTask(agent, key string) domain.ConnectorTaskRequest {
	return domain.ConnectorTaskRequest{SchemaVersion: 1, IdempotencyKey: key, AgentID: agent, Kind: "prompt", Prompt: "synthetic task", Files: []domain.ConnectorTaskFile{{Path: "input.txt", Content: "synthetic input"}}}
}
func submitTypedTask(t *testing.T, s *Store, id, agent string, now time.Time) domain.ConnectorTask {
	t.Helper()
	task, created, err := s.SubmitConnectorTask(42, id, typedTask(agent, id), now)
	if err != nil || !created {
		t.Fatalf("submit=%+v %v", task, err)
	}
	return task
}
func taskLease(value byte) string {
	return connectorFixtureToken(domain.ConnectorLeaseTokenPrefix, value)
}
func claimTypedTask(t *testing.T, s *Store, hash, token string, now time.Time) domain.ConnectorTask {
	t.Helper()
	task, err := s.ClaimConnectorTask(hash, token, now)
	if err != nil || task == nil {
		t.Fatalf("claim=%+v %v", task, err)
	}
	return *task
}
func actTypedTask(t *testing.T, s *Store, hash, id, action string, proof domain.ConnectorTaskLease, result *domain.ConnectorTaskResult, now time.Time) domain.ConnectorTask {
	t.Helper()
	task, err := s.ConnectorTaskAction(hash, id, action, proof, result, now)
	if err != nil {
		t.Fatalf("%s=%+v %v", action, task, err)
	}
	return task
}

func TestConnectorTasksIdempotencyScopeAndPersistence(t *testing.T) {
	s, now, identity, _ := taskFixture(t)
	agent := identity.Agents[0].ID
	input := typedTask(agent, "CaseSensitive")
	first, created, err := s.SubmitConnectorTask(42, "task-one", input, now)
	if err != nil || !created {
		t.Fatal(err)
	}
	retry, created, err := s.SubmitConnectorTask(42, "different-id", input, now.Add(time.Second))
	if err != nil || created || retry.ID != first.ID {
		t.Fatal("idempotency failed", err)
	}
	input.Prompt = "different input"
	if _, _, err := s.SubmitConnectorTask(42, "conflict", input, now); !errors.Is(err, ErrConnectorTaskConflict) {
		t.Fatal("same key changed content", err)
	}
	input = typedTask(agent, "casesensitive")
	if _, created, err := s.SubmitConnectorTask(42, "case-distinct", input, now); err != nil || !created {
		t.Fatal("key hashing lost case sensitivity", err)
	}
	if _, _, err := s.SubmitConnectorTask(43, "other-owner", typedTask(agent, "other-owner"), now); !errors.Is(err, ErrConnectorTaskNotFound) {
		t.Fatal("other owner submitted to Agent", err)
	}
	if _, err := s.ConnectorTask(43, first.ID, now); !errors.Is(err, ErrConnectorTaskNotFound) {
		t.Fatal("other owner read input", err)
	}
	reopened := newStore(s.DB)
	loaded, err := reopened.ConnectorTask(42, first.ID, now)
	if err != nil || loaded.Input == nil || loaded.Input.TimeoutSeconds != 300 {
		t.Fatal("task did not persist", err)
	}
	list, err := s.ConnectorTaskList(42, now)
	if err != nil || len(list) != 2 || list[0].Input != nil || list[0].Result != nil {
		t.Fatal("list did not remain metadata only", err)
	}
	if err := s.RevokeConnectorAgent(42, agent); err != nil {
		t.Fatal(err)
	}
	if retry, _, err := s.SubmitConnectorTask(42, "after-revoke", typedTask(agent, "CaseSensitive"), now); err != nil || retry.ID != first.ID {
		t.Fatal("revoke broke idempotent retrieval", err)
	}
	if _, _, err := s.SubmitConnectorTask(42, "new-after-revoke", typedTask(agent, "new-key"), now); !errors.Is(err, ErrConnectorTaskNotFound) {
		t.Fatal("new task bypassed revoke", err)
	}
}

func TestConnectorTasksFencedClaimStartAndCompletion(t *testing.T) {
	s, now, identity, hashes := taskFixture(t)
	submitTypedTask(t, s, "task-a1", identity.Agents[0].ID, now)
	submitTypedTask(t, s, "task-a2", identity.Agents[0].ID, now.Add(time.Millisecond))
	submitTypedTask(t, s, "task-b", identity.Agents[1].ID, now)
	claimed := claimTypedTask(t, s, hashes[0], taskLease(20), now)
	if claimed.ID != "task-a1" || claimed.Status != domain.ConnectorTaskClaimed || claimed.Input == nil {
		t.Fatal(claimed)
	}
	repeat := claimTypedTask(t, s, hashes[0], taskLease(20), now)
	if repeat.ID != claimed.ID || repeat.Attempt != claimed.Attempt {
		t.Fatal("lost response claimed twice")
	}
	if _, err := s.ClaimConnectorTask(hashes[0], taskLease(21), now); !errors.Is(err, ErrConnectorAgentBusy) {
		t.Fatal("same Agent received overlapping tasks", err)
	}
	other := claimTypedTask(t, s, hashes[1], taskLease(22), now)
	if other.ID != "task-b" {
		t.Fatal("Agent claimed another identity's task")
	}
	proof := domain.ConnectorTaskLease{Attempt: claimed.Attempt, LeaseToken: taskLease(20)}
	if _, err := s.ConnectorTaskAction(hashes[1], claimed.ID, "start", proof, nil, now); !errors.Is(err, ErrConnectorTaskNotFound) {
		t.Fatal("cross-Agent mutation accepted", err)
	}
	wrong := proof
	wrong.Attempt++
	if _, err := s.ConnectorTaskAction(hashes[0], claimed.ID, "start", wrong, nil, now); !errors.Is(err, ErrConnectorTaskLease) {
		t.Fatal("wrong attempt accepted", err)
	}
	running := actTypedTask(t, s, hashes[0], claimed.ID, "start", proof, nil, now)
	if running.Status != domain.ConnectorTaskRunning || running.DeadlineAt == nil {
		t.Fatal(running)
	}
	restarted := actTypedTask(t, s, hashes[0], claimed.ID, "start", proof, nil, now.Add(time.Second))
	if !restarted.DeadlineAt.Equal(*running.DeadlineAt) {
		t.Fatal("replayed start extended deadline")
	}
	renewed := actTypedTask(t, s, hashes[0], claimed.ID, "renew", proof, nil, now.Add(20*time.Second))
	if !renewed.LeaseUntil.After(*running.LeaseUntil) {
		t.Fatal("lease did not renew")
	}
	result := domain.ConnectorTaskResult{Status: domain.ConnectorTaskSucceeded, Text: "synthetic output", Files: []domain.ConnectorTaskFile{{Path: "result.txt", Content: "report"}}, Quiesced: true}
	done := actTypedTask(t, s, hashes[0], claimed.ID, "complete", proof, &result, now.Add(21*time.Second))
	if done.Result == nil || done.Status != domain.ConnectorTaskSucceeded {
		t.Fatal(done)
	}
	actTypedTask(t, s, hashes[0], claimed.ID, "complete", proof, &result, now.Add(22*time.Second))
	result.Text = "different output"
	if _, err := s.ConnectorTaskAction(hashes[0], claimed.ID, "complete", proof, &result, now.Add(22*time.Second)); !errors.Is(err, ErrConnectorTaskLease) {
		t.Fatal("duplicate completion rewrote result", err)
	}
	canceled, err := s.CancelConnectorTask(42, claimed.ID, now.Add(23*time.Second))
	if err != nil || canceled.Status != domain.ConnectorTaskSucceeded {
		t.Fatal("cancel rewrote committed success", err)
	}
	if old := claimTypedTask(t, s, hashes[0], taskLease(20), now.Add(24*time.Second)); old.ID != claimed.ID || !domain.ConnectorTaskTerminal(old.Status) {
		t.Fatal("reused lease nonce claimed another task")
	}
	if next := claimTypedTask(t, s, hashes[0], taskLease(23), now.Add(24*time.Second)); next.ID != "task-a2" {
		t.Fatal(next)
	}
	var stored connectorTaskRow
	s.DB.Where("id = ?", claimed.ID).Take(&stored)
	raw, _ := json.Marshal(stored)
	if strings.Contains(string(raw), proof.LeaseToken) {
		t.Fatal("plaintext lease proof persisted")
	}
}

func TestConnectorTasksExpiredStartIsNeverBlindlyRerun(t *testing.T) {
	s, now, identity, hashes := taskFixture(t)
	agent := identity.Agents[0].ID
	submitTypedTask(t, s, "task-one", agent, now)
	submitTypedTask(t, s, "task-next", agent, now.Add(time.Millisecond))
	first := claimTypedTask(t, s, hashes[0], taskLease(30), now)
	oldProof := domain.ConnectorTaskLease{Attempt: first.Attempt, LeaseToken: taskLease(30)}
	queued, err := s.ConnectorTask(42, first.ID, now.Add(31*time.Second))
	if err != nil || queued.Status != domain.ConnectorTaskQueued {
		t.Fatal("never-started claim not requeued", err)
	}
	if _, err := s.ConnectorTaskAction(hashes[0], first.ID, "start", oldProof, nil, now.Add(31*time.Second)); !errors.Is(err, ErrConnectorTaskLease) {
		t.Fatal("stale start authorized", err)
	}
	second := claimTypedTask(t, s, hashes[0], taskLease(31), now.Add(31*time.Second))
	if second.ID != first.ID || second.Attempt != first.Attempt+1 {
		t.Fatal("claim attempt did not advance")
	}
	proof := domain.ConnectorTaskLease{Attempt: second.Attempt, LeaseToken: taskLease(31)}
	actTypedTask(t, s, hashes[0], second.ID, "start", proof, nil, now.Add(31*time.Second))
	unknown, err := s.ConnectorTask(42, second.ID, now.Add(77*time.Second))
	if err != nil || unknown.Status != domain.ConnectorTaskRecoveryRequired {
		t.Fatal("running lease loss was not explicit", err)
	}
	if _, err := s.ClaimConnectorTask(hashes[0], taskLease(32), now.Add(77*time.Second)); !errors.Is(err, ErrConnectorAgentBusy) {
		t.Fatal("unknown execution allowed next task", err)
	}
	if _, err := s.ConnectorTaskAction(hashes[0], second.ID, "start", proof, nil, now.Add(77*time.Second)); !errors.Is(err, ErrConnectorTaskLease) {
		t.Fatal("expired run resumed", err)
	}
	// A late acknowledgement may settle the same attempt after it has stopped;
	// it does not renew the lease or authorize continuing execution.
	result := domain.ConnectorTaskResult{Status: domain.ConnectorTaskFailed, Text: "synthetic worker stopped after loss", Quiesced: true}
	actTypedTask(t, s, hashes[0], second.ID, "complete", proof, &result, now.Add(78*time.Second))
	if next := claimTypedTask(t, s, hashes[0], taskLease(33), now.Add(79*time.Second)); next.ID != "task-next" {
		t.Fatal(next)
	}
}

func TestConnectorTasksCancellationWaitsForStop(t *testing.T) {
	s, now, identity, hashes := taskFixture(t)
	agent := identity.Agents[0].ID
	submitTypedTask(t, s, "never-claimed", agent, now)
	if got, err := s.CancelConnectorTask(42, "never-claimed", now); err != nil || got.Status != domain.ConnectorTaskCanceled {
		t.Fatal(got, err)
	}
	submitTypedTask(t, s, "in-flight", agent, now)
	claim := claimTypedTask(t, s, hashes[0], taskLease(40), now)
	proof := domain.ConnectorTaskLease{Attempt: claim.Attempt, LeaseToken: taskLease(40)}
	actTypedTask(t, s, hashes[0], claim.ID, "start", proof, nil, now)
	requested, err := s.CancelConnectorTask(42, claim.ID, now.Add(time.Second))
	if err != nil || requested.Status != domain.ConnectorTaskCancelRequested {
		t.Fatal("premature cancellation", err)
	}
	bad := domain.ConnectorTaskResult{Status: domain.ConnectorTaskSucceeded, Text: "late success", Quiesced: true}
	if _, err := s.ConnectorTaskAction(hashes[0], claim.ID, "complete", proof, &bad, now.Add(2*time.Second)); !errors.Is(err, ErrConnectorTaskLease) {
		t.Fatal("success overrode committed cancellation", err)
	}
	stop := actTypedTask(t, s, hashes[0], claim.ID, "renew", proof, nil, now.Add(2*time.Second))
	if stop.Status != domain.ConnectorTaskCancelRequested {
		t.Fatal("renew lost cancel signal")
	}
	bad = domain.ConnectorTaskResult{Status: domain.ConnectorTaskCanceled, Quiesced: false}
	if _, err := s.ConnectorTaskAction(hashes[0], claim.ID, "complete", proof, &bad, now.Add(3*time.Second)); err == nil {
		t.Fatal("unconfirmed process stop accepted")
	}
	unknown, err := s.ConnectorTask(42, claim.ID, now.Add(46*time.Second))
	if err != nil || unknown.Status != domain.ConnectorTaskRecoveryRequired || !unknown.CancelRequested {
		t.Fatal("lost canceled execution was hidden", err)
	}
	bad.Quiesced = true
	done := actTypedTask(t, s, hashes[0], claim.ID, "complete", proof, &bad, now.Add(47*time.Second))
	if done.Status != domain.ConnectorTaskCanceled {
		t.Fatal(done)
	}
}

func TestConnectorTasksRevocationAndDeadline(t *testing.T) {
	s, now, identity, hashes := taskFixture(t)
	input := typedTask(identity.Agents[0].ID, "short")
	input.TimeoutSeconds = 10
	if _, _, err := s.SubmitConnectorTask(42, "short", input, now); err != nil {
		t.Fatal(err)
	}
	claim := claimTypedTask(t, s, hashes[0], taskLease(50), now)
	proof := domain.ConnectorTaskLease{Attempt: claim.Attempt, LeaseToken: taskLease(50)}
	running := actTypedTask(t, s, hashes[0], claim.ID, "start", proof, nil, now)
	renewed := actTypedTask(t, s, hashes[0], claim.ID, "renew", proof, nil, now.Add(5*time.Second))
	if !renewed.LeaseUntil.Equal(*running.DeadlineAt) {
		t.Fatal("renew exceeded absolute task deadline")
	}
	got, err := s.ConnectorTask(42, claim.ID, now.Add(11*time.Second))
	if err != nil || got.Status != domain.ConnectorTaskRecoveryRequired {
		t.Fatal("deadline not enforced", err)
	}
	if err := s.RevokeConnectorDevice(42, identity.Device.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimConnectorTask(hashes[0], taskLease(51), now); !errors.Is(err, ErrConnectorCredential) {
		t.Fatal("revoked credential claimed work", err)
	}
	if _, err := s.ConnectorTaskAction(hashes[0], claim.ID, "control", proof, nil, now); !errors.Is(err, ErrConnectorCredential) {
		t.Fatal("revoked credential controlled task", err)
	}
}

func TestConnectorTasksConcurrentClaimHasOneWinner(t *testing.T) {
	s, now, identity, hashes := taskFixture(t)
	submitTypedTask(t, s, "race", identity.Agents[0].ID, now)
	var group sync.WaitGroup
	results := make(chan error, 2)
	for i := 0; i < 2; i++ {
		group.Add(1)
		go func(i int) {
			defer group.Done()
			_, err := s.ClaimConnectorTask(hashes[0], taskLease(byte(60+i)), now)
			results <- err
		}(i)
	}
	group.Wait()
	close(results)
	won, busy := 0, 0
	for err := range results {
		if err == nil {
			won++
		} else if errors.Is(err, ErrConnectorAgentBusy) {
			busy++
		} else {
			t.Fatal(err)
		}
	}
	if won != 1 || busy != 1 {
		t.Fatalf("won=%d busy=%d", won, busy)
	}
}

func TestConnectorTasksConcurrentSubmissionKeepsOneRequest(t *testing.T) {
	s, now, identity, _ := taskFixture(t)
	var group sync.WaitGroup
	tasks := make(chan string, 2)
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		group.Add(1)
		go func(i int) {
			defer group.Done()
			task, _, err := s.SubmitConnectorTask(42, fmt.Sprint("task-", i), typedTask(identity.Agents[0].ID, "same-request"), now)
			tasks <- task.ID
			errs <- err
		}(i)
	}
	group.Wait()
	close(tasks)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	first := ""
	for id := range tasks {
		if first == "" {
			first = id
		} else if id != first {
			t.Fatal("duplicate durable request")
		}
	}
}

func TestConnectorTaskCancelBeforeStartAndAtomicAudit(t *testing.T) {
	s, now, identity, hashes := taskFixture(t)
	submitTypedTask(t, s, "before-start", identity.Agents[0].ID, now)
	claim := claimTypedTask(t, s, hashes[0], taskLease(70), now)
	proof := domain.ConnectorTaskLease{Attempt: claim.Attempt, LeaseToken: taskLease(70)}
	if claim.RunAllowed {
		t.Fatal("claim authorized execution before start")
	}
	if _, err := s.CancelConnectorTask(42, claim.ID, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	control := actTypedTask(t, s, hashes[0], claim.ID, "control", proof, nil, now.Add(2*time.Second))
	if control.Status != domain.ConnectorTaskCancelRequested || control.RunAllowed {
		t.Fatal("never-started cancellation lost its state")
	}
	result := domain.ConnectorTaskResult{Status: domain.ConnectorTaskCanceled, Quiesced: true}
	actTypedTask(t, s, hashes[0], claim.ID, "complete", proof, &result, now.Add(3*time.Second))
	actTypedTask(t, s, hashes[0], claim.ID, "complete", proof, &result, now.Add(4*time.Second))
	events, err := s.ConnectorTaskEvents(42, claim.ID, 0)
	if err != nil || len(events) != 4 {
		t.Fatalf("events=%+v %v", events, err)
	}
	for i, want := range []string{"submitted", "claimed", "cancel_requested", "complete"} {
		if events[i].Action != want {
			t.Fatal("wrong event order", events)
		}
	}
	if events[0].OwnerID != 42 || events[1].AgentID != identity.Agents[0].ID || events[3].LeaseUntil == nil {
		t.Fatal("audit actor or lease context missing")
	}
	if _, err := s.ConnectorTaskEvents(43, claim.ID, 0); !errors.Is(err, ErrConnectorTaskNotFound) {
		t.Fatal("another owner read audit")
	}
	last, err := s.ConnectorTaskEvents(42, claim.ID, events[2].ID)
	if err != nil || len(last) != 1 {
		t.Fatal("event cursor failed", err)
	}
	serialized, _ := json.Marshal(events)
	for _, secret := range []string{"synthetic input", "synthetic task", proof.LeaseToken} {
		if strings.Contains(string(serialized), secret) {
			t.Fatal("audit included task content or credentials")
		}
	}
}

func TestConnectorTaskStateFailureRollsBackItsAudit(t *testing.T) {
	s, now, identity, hashes := taskFixture(t)
	submitTypedTask(t, s, "atomic", identity.Agents[0].ID, now)
	claim := claimTypedTask(t, s, hashes[0], taskLease(71), now)
	proof := domain.ConnectorTaskLease{Attempt: claim.Attempt, LeaseToken: taskLease(71)}
	injected := false
	if err := s.DB.Callback().Update().Before("gorm:update").Register("fixture:connector-start-failure", func(tx *gorm.DB) {
		if values, ok := tx.Statement.Dest.(map[string]any); ok && tx.Statement.Table == "connector_tasks" && values["status"] == domain.ConnectorTaskRunning {
			injected = true
			tx.AddError(errors.New("synthetic state-write failure"))
		}
	}); err != nil {
		t.Fatal(err)
	}
	_, err := s.ConnectorTaskAction(hashes[0], claim.ID, "start", proof, nil, now)
	s.DB.Callback().Update().Remove("fixture:connector-start-failure")
	if err == nil || !injected {
		t.Fatal("state failure was not exercised")
	}
	events, err := s.ConnectorTaskEvents(42, claim.ID, 0)
	if err != nil || len(events) != 2 {
		t.Fatal("orphan audit survived rolled-back state", err)
	}
	state, err := s.ConnectorTask(42, claim.ID, now)
	if err != nil || state.Status != domain.ConnectorTaskClaimed {
		t.Fatal("failed start changed state", err)
	}
	started := actTypedTask(t, s, hashes[0], claim.ID, "start", proof, nil, now)
	if !started.RunAllowed || started.LeaseRemainingMillis != 45000 {
		t.Fatal("successful retry did not grant a bounded lease")
	}
}

func TestConnectorTaskStorageFailureIsNotCredentialRevocation(t *testing.T) {
	s, now, identity, hashes := taskFixture(t)
	submitTypedTask(t, s, "storage", identity.Agents[0].ID, now)
	injected := false
	if err := s.DB.Callback().Query().Before("gorm:query").Register("fixture:connector-query-failure", func(tx *gorm.DB) {
		if tx.Statement.Table == "connector_agents" {
			injected = true
			tx.AddError(errors.New("synthetic database query outage"))
		}
	}); err != nil {
		t.Fatal(err)
	}
	_, err := s.ClaimConnectorTask(hashes[0], taskLease(72), now)
	s.DB.Callback().Query().Remove("fixture:connector-query-failure")
	if err == nil || !injected || errors.Is(err, ErrConnectorCredential) {
		t.Fatal("storage failure was misreported as invalid credentials", err)
	}
	if claim := claimTypedTask(t, s, hashes[0], taskLease(72), now); claim.ID != "storage" {
		t.Fatal("failed request consumed task")
	}
}
