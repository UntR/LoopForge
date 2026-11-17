package connect

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"agentflow/internal/domain"
)

func connectorTestCode() string {
	return domain.ConnectorBindCodePrefix + base64.RawURLEncoding.EncodeToString(make([]byte, 32))
}
func testBindOptions(t *testing.T, server string) BindOptions {
	t.Helper()
	return BindOptions{Server: server, Code: connectorTestCode(), StateDir: filepath.Join(t.TempDir(), "private-state"), Name: "synthetic", OS: "windows", Arch: "amd64", Agents: []domain.ConnectorAgentEnrollment{{Provider: "codex", Version: "fixture"}, {Provider: "dsh", Version: "fixture"}, {Provider: "minimax-code", Version: "fixture"}}}
}
func identityFor(input domain.ConnectorEnrollment) domain.ConnectorIdentity {
	identity := domain.ConnectorIdentity{Device: domain.ConnectorDevice{ID: "device-fixture", Name: input.Name, OS: input.OS, Arch: input.Arch, ExpiresAt: time.Now().Add(time.Hour)}}
	for _, agent := range input.Agents {
		identity.Agents = append(identity.Agents, domain.ConnectorAgent{ID: "agent-" + agent.Provider, DeviceID: identity.Device.ID, Provider: agent.Provider, Version: agent.Version})
	}
	return identity
}

func TestBindPersistsBeforeSendAndRecoversUncertainResult(t *testing.T) {
	var enrolled *domain.ConnectorEnrollment
	var options BindOptions
	registrations := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/runners/connectors/register":
			registrations++
			var input domain.ConnectorEnrollment
			if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
				t.Error(err)
				w.WriteHeader(400)
				return
			}
			state, err := loadState(options.StateDir)
			if err != nil || state.Credential != input.Credential {
				t.Error("credentials not persisted before network request")
			}
			enrolled = &input
			// Commit succeeded, but the TCP connection disappears before a response.
			conn, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			conn.Close()
		case "/runners/connectors/self":
			if enrolled == nil || r.Header.Get("Authorization") != "Bearer "+enrolled.Credential {
				w.WriteHeader(401)
				return
			}
			json.NewEncoder(w).Encode(identityFor(*enrolled))
		default:
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	options = testBindOptions(t, server.URL)
	client, err := NewClient(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Bind(context.Background(), client, options); err == nil {
		t.Fatal("expected uncertain network result")
	}
	pending, err := loadState(options.StateDir)
	if err != nil || pending.Identity != nil {
		t.Fatal("pending state lost", err)
	}
	options.Code = ""
	identity, err := Bind(context.Background(), client, options)
	if err != nil || identity.Device.ID != "device-fixture" || registrations != 1 {
		t.Fatalf("recover=%+v %v registrations=%d", identity, err, registrations)
	}
	final, err := loadState(options.StateDir)
	if err != nil || final.Identity == nil || final.Credential != pending.Credential {
		t.Fatal("recovery replaced credential", err)
	}
	data, err := os.ReadFile(filepath.Join(options.StateDir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), connectorTestCode()) {
		t.Fatal("bind code persisted")
	}
	if runtime.GOOS != "windows" {
		info, _ := os.Stat(filepath.Join(options.StateDir, "state.json"))
		if info.Mode().Perm() != 0600 {
			t.Fatal("state not private")
		}
	}
}

func TestClientRejectsUnsafeOrigins(t *testing.T) {
	for _, server := range []string{"http://nas.example", "http://192.0.2.10", "https://user:secret@example.com", "https://example.com/?token=x", "https://example.com/#x", "https://example.com/subpath", "ftp://127.0.0.1"} {
		if _, err := NewClient(server); err == nil {
			t.Fatalf("accepted %s", server)
		}
	}
	for _, server := range []string{"https://example.com", "http://127.0.0.1:1234", "http://[::1]:1234"} {
		if _, err := NewClient(server); err != nil {
			t.Fatal(err)
		}
	}
}

func TestBindDoesNotFollowRedirectOrDiscloseServerError(t *testing.T) {
	redirected := 0
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { redirected++; w.WriteHeader(200) }))
	defer target.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	options := testBindOptions(t, server.URL)
	client, _ := NewClient(server.URL)
	_, err := Bind(context.Background(), client, options)
	if err == nil || redirected != 0 || strings.Contains(err.Error(), options.Code) {
		t.Fatalf("unsafe redirect/error=%v count=%d", err, redirected)
	}
}

func TestBindRejectsOtherServerAndPreservesCredentials(t *testing.T) {
	options := testBindOptions(t, "https://one.example")
	options.Code = ""
	one, _ := NewClient(options.Server)
	if _, err := Bind(context.Background(), one, options); !errors.Is(err, ErrBindCodeRequired) {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(filepath.Join(options.StateDir, "state.json"))
	options.Server = "https://two.example"
	two, _ := NewClient(options.Server)
	if _, err := Bind(context.Background(), two, options); err == nil {
		t.Fatal("rebound state to different server")
	}
	after, _ := os.ReadFile(filepath.Join(options.StateDir, "state.json"))
	if string(before) != string(after) {
		t.Fatal("saved credentials changed")
	}
}

func TestBindRejectsSymlinkAndSharedState(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows DACL acceptance requires native Windows test host")
	}
	options := testBindOptions(t, "https://one.example")
	client, _ := NewClient(options.Server)
	target := t.TempDir()
	if err := os.Symlink(target, options.StateDir); err != nil {
		t.Fatal(err)
	}
	if _, err := Bind(context.Background(), client, options); err == nil {
		t.Fatal("accepted symlink state directory")
	}
	os.Remove(options.StateDir)
	os.Mkdir(options.StateDir, 0755)
	if _, err := Bind(context.Background(), client, options); err == nil {
		t.Fatal("accepted shared state directory")
	}
}

func TestStateLockReleasesWithoutDeletingLockInode(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "state")
	if err := secureStateDirectory(directory); err != nil {
		t.Fatal(err)
	}
	unlock, err := lockState(directory)
	if err != nil {
		t.Fatal(err)
	}
	if second, err := lockState(directory); err == nil {
		second()
		unlock()
		t.Fatal("second lock accepted")
	}
	unlock()
	third, err := lockState(directory)
	if err != nil {
		t.Fatal("lock remained after release", err)
	}
	third()
}

func TestBindDoesNotSilentlyChangeAgentSet(t *testing.T) {
	options := testBindOptions(t, "https://one.example")
	options.Code = ""
	client, _ := NewClient(options.Server)
	if _, err := Bind(context.Background(), client, options); !errors.Is(err, ErrBindCodeRequired) {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(filepath.Join(options.StateDir, "state.json"))
	options.Agents = options.Agents[:1]
	if _, err := Bind(context.Background(), client, options); err == nil {
		t.Fatal("silently accepted a different Agent set")
	}
	after, _ := os.ReadFile(filepath.Join(options.StateDir, "state.json"))
	if string(before) != string(after) {
		t.Fatal("Agent-set mismatch changed credentials")
	}
}
