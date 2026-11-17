package main

import (
	"agentflow/internal/domain"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestRejectTaskOrIncompleteBindingInput(t *testing.T) {
	for _, args := range [][]string{nil, {"bind"}, {"doctor", "--agents", "shell"}, {"doctor", "run-this-command"}} {
		var out, errors bytes.Buffer
		if code := run(args, &out, &errors); code != 2 || out.Len() != 0 {
			t.Fatalf("run(%v) = %d, output %s", args, code, out.String())
		}
	}
}

func TestBindCommandWithSyntheticCLIs(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX CLI fixture; native Windows acceptance is separate")
	}
	bin := t.TempDir()
	for _, name := range []string{"codex", "dsh", "mcode"} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\n[ \"$#\" = 1 ] && [ \"$1\" = --version ] || exit 97\nprintf '%s\\n' 'synthetic-cli-version'\n"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin)
	code := domain.ConnectorBindCodePrefix + base64.RawURLEncoding.EncodeToString(make([]byte, 32))
	var registrations atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/runners/connectors/register" {
			w.WriteHeader(401)
			return
		}
		var input domain.ConnectorEnrollment
		if json.NewDecoder(r.Body).Decode(&input) != nil || input.Code != code {
			w.WriteHeader(400)
			return
		}
		registrations.Add(1)
		identity := domain.ConnectorIdentity{Device: domain.ConnectorDevice{ID: "device-cli-fixture", Name: input.Name, OS: input.OS, Arch: input.Arch, ExpiresAt: time.Now().Add(time.Hour)}}
		for _, agent := range input.Agents {
			identity.Agents = append(identity.Agents, domain.ConnectorAgent{ID: "agent-" + agent.Provider, DeviceID: identity.Device.ID, Provider: agent.Provider})
		}
		w.WriteHeader(201)
		json.NewEncoder(w).Encode(identity)
	}))
	defer server.Close()
	var out, errorOut bytes.Buffer
	exit := runWithInput([]string{"bind", "--server", server.URL, "--state-dir", filepath.Join(t.TempDir(), "state"), "--name", "synthetic-cli-device"}, strings.NewReader(code+"\n"), &out, &errorOut)
	if exit != 0 || registrations.Load() != 1 {
		t.Fatalf("bind command failed: exit=%d, diagnostic=%s", exit, errorOut.String())
	}
	var identity domain.ConnectorIdentity
	if json.Unmarshal(out.Bytes(), &identity) != nil || len(identity.Agents) != 3 {
		t.Fatal("command did not return three public identities")
	}
	for _, secret := range []string{code, domain.ConnectorAgentTokenPrefix, domain.ConnectorDeviceTokenPrefix} {
		if strings.Contains(out.String()+errorOut.String(), secret) {
			t.Fatal("command disclosed credential material")
		}
	}
}
