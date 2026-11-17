package repo

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"agentflow/internal/domain"
)

func connectorFixtureToken(prefix string, value byte) string {
	return prefix + base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{value}, 32))
}

func connectorFixture() domain.ConnectorEnrollment {
	return domain.ConnectorEnrollment{
		Code:       connectorFixtureToken(domain.ConnectorBindCodePrefix, 1),
		Credential: connectorFixtureToken(domain.ConnectorDeviceTokenPrefix, 2),
		Name:       "synthetic machine", OS: "windows", Arch: "amd64",
		Agents: []domain.ConnectorAgentEnrollment{
			{Provider: "codex", Version: "fixture", Credential: connectorFixtureToken(domain.ConnectorAgentTokenPrefix, 3)},
			{Provider: "dsh", Version: "fixture", Credential: connectorFixtureToken(domain.ConnectorAgentTokenPrefix, 4)},
			{Provider: "minimax-code", Version: "fixture", Credential: connectorFixtureToken(domain.ConnectorAgentTokenPrefix, 5)},
		},
	}
}

func connectorTestStore(t *testing.T) (*Store, time.Time) {
	t.Helper()
	s, err := OpenTest()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
}

func issueConnectorFixture(t *testing.T, s *Store, input domain.ConnectorEnrollment, owner int64, now time.Time) {
	t.Helper()
	if err := s.IssueConnectorCode(owner, domain.ConnectorTokenHash(input.Code), now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
}

func TestConnectorEnrollmentDurableSeparateCredentials(t *testing.T) {
	s, now := connectorTestStore(t)
	input := connectorFixture()
	issueConnectorFixture(t, s, input, 42, now)
	identity, err := s.EnrollConnector(input, "device-fixture", []string{"a1", "a2", "a3"}, now)
	if err != nil {
		t.Fatal(err)
	}
	// Recreate the domain repositories over the same database: no identity or
	// one-time code state is held in an API process's memory.
	reopened := newStore(s.DB)
	recovered, err := reopened.ConnectorIdentityByHash(domain.ConnectorTokenHash(input.Credential), now)
	if err != nil || recovered.Device.ID != identity.Device.ID || len(recovered.Agents) != 3 {
		t.Fatalf("recovery=%+v, %v", recovered, err)
	}
	for i, agent := range input.Agents {
		got, err := reopened.ConnectorAgentByHash(domain.ConnectorTokenHash(agent.Credential), now)
		if err != nil || got.ID != identity.Agents[i].ID || got.Provider != agent.Provider {
			t.Fatalf("agent=%+v %v", got, err)
		}
	}
	if _, err := s.ConnectorAgentByHash(domain.ConnectorTokenHash(input.Credential), now); !errors.Is(err, ErrConnectorCredential) {
		t.Fatal("device token became Agent token")
	}
	if _, err := s.ConnectorIdentityByHash(domain.ConnectorTokenHash(input.Agents[0].Credential), now); !errors.Is(err, ErrConnectorCredential) {
		t.Fatal("Agent token became device token")
	}
	if _, err := reopened.EnrollConnector(input, "replay", []string{"r1", "r2", "r3"}, now); !errors.Is(err, ErrConnectorCredential) {
		t.Fatalf("replayed code accepted: %v", err)
	}
	var code connectorCodeRow
	s.DB.First(&code)
	var device connectorDeviceRow
	s.DB.First(&device)
	var agent connectorAgentRow
	s.DB.First(&agent)
	serialized, _ := json.Marshal([]any{identity, code, device, agent})
	for _, raw := range []string{input.Code, input.Credential, input.Agents[0].Credential} {
		if bytes.Contains(serialized, []byte(raw)) {
			t.Fatal("raw credential persisted or returned")
		}
	}
}

func TestConnectorScopeRevocationAndExpiry(t *testing.T) {
	s, now := connectorTestStore(t)
	input := connectorFixture()
	issueConnectorFixture(t, s, input, 42, now)
	identity, err := s.EnrollConnector(input, "device", []string{"a1", "a2", "a3"}, now)
	if err != nil {
		t.Fatal(err)
	}
	other, err := s.ListConnectors(43)
	if err != nil || len(other) != 0 {
		t.Fatal("owner isolation failed")
	}
	if err := s.RevokeConnectorDevice(43, identity.Device.ID); !errors.Is(err, ErrConnectorNotFound) {
		t.Fatalf("other owner revoked device: %v", err)
	}
	if err := s.RevokeConnectorAgent(43, identity.Agents[0].ID); !errors.Is(err, ErrConnectorNotFound) {
		t.Fatalf("other owner revoked Agent: %v", err)
	}
	if err := s.RevokeConnectorAgent(42, identity.Agents[0].ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ConnectorAgentByHash(domain.ConnectorTokenHash(input.Agents[0].Credential), now); !errors.Is(err, ErrConnectorCredential) {
		t.Fatal("revoked Agent still authenticates")
	}
	if _, err := s.ConnectorAgentByHash(domain.ConnectorTokenHash(input.Agents[1].Credential), now); err != nil {
		t.Fatal("unrelated Agent revoked", err)
	}
	if _, err := s.ConnectorIdentityByHash(domain.ConnectorTokenHash(input.Credential), now.Add(31*24*time.Hour)); !errors.Is(err, ErrConnectorCredential) {
		t.Fatal("expired device accepted")
	}
	if _, err := s.ConnectorAgentByHash(domain.ConnectorTokenHash(input.Agents[1].Credential), now.Add(31*24*time.Hour)); !errors.Is(err, ErrConnectorCredential) {
		t.Fatal("Agent outlived device")
	}
	if err := s.RevokeConnectorDevice(42, identity.Device.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.RevokeConnectorDevice(42, identity.Device.ID); err != nil {
		t.Fatal("revocation not idempotent", err)
	}
	if _, err := s.ConnectorIdentityByHash(domain.ConnectorTokenHash(input.Credential), now); !errors.Is(err, ErrConnectorCredential) {
		t.Fatal("revoked device authenticates")
	}
	if _, err := s.ConnectorAgentByHash(domain.ConnectorTokenHash(input.Agents[1].Credential), now); !errors.Is(err, ErrConnectorCredential) {
		t.Fatal("device revoke left child Agent active")
	}
}

func TestConnectorEnrollmentRollbackAndExpiredCode(t *testing.T) {
	s, now := connectorTestStore(t)
	input := connectorFixture()
	issueConnectorFixture(t, s, input, 42, now)
	// Deliberately collide primary keys after the device and first Agent insert.
	if _, err := s.EnrollConnector(input, "device", []string{"same", "same", "last"}, now); err == nil {
		t.Fatal("expected transaction failure")
	}
	identities, err := s.ListConnectors(42)
	if err != nil || len(identities) != 0 {
		t.Fatal("partial identity escaped transaction")
	}
	if _, err := s.EnrollConnector(input, "device", []string{"a1", "a2", "a3"}, now); err != nil {
		t.Fatal("rollback consumed bind code", err)
	}
	input.Code = connectorFixtureToken(domain.ConnectorBindCodePrefix, 9)
	if err := s.IssueConnectorCode(42, domain.ConnectorTokenHash(input.Code), now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.EnrollConnector(input, "expired", []string{"e1", "e2", "e3"}, now); !errors.Is(err, ErrConnectorCredential) {
		t.Fatal("expired code accepted", err)
	}
}

func TestConnectorCodeConcurrentUseHasOneWinner(t *testing.T) {
	s, now := connectorTestStore(t)
	input := connectorFixture()
	issueConnectorFixture(t, s, input, 42, now)
	var group sync.WaitGroup
	results := make(chan error, 2)
	for i := 0; i < 2; i++ {
		group.Add(1)
		go func(i int) {
			defer group.Done()
			_, err := s.EnrollConnector(input, fmt.Sprint("device-", i), []string{fmt.Sprint(i, "a"), fmt.Sprint(i, "b"), fmt.Sprint(i, "c")}, now)
			results <- err
		}(i)
	}
	group.Wait()
	close(results)
	success, denied := 0, 0
	for err := range results {
		if err == nil {
			success++
		} else if errors.Is(err, ErrConnectorCredential) {
			denied++
		} else {
			t.Fatal(err)
		}
	}
	if success != 1 || denied != 1 {
		t.Fatalf("success=%d denied=%d", success, denied)
	}
}
