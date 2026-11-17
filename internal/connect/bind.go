package connect

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"agentflow/internal/domain"
)

// State is private local credential material, never a CLI result or log.
// Pending credentials are saved before registration so an uncertain network
// result can be recovered with device authentication rather than a second bind.
type State struct {
	SchemaVersion int                               `json:"schemaVersion"`
	Server        string                            `json:"server"`
	Name          string                            `json:"name"`
	OS            string                            `json:"os"`
	Arch          string                            `json:"arch"`
	Credential    string                            `json:"credential"`
	Agents        []domain.ConnectorAgentEnrollment `json:"agents"`
	Identity      *domain.ConnectorIdentity         `json:"identity,omitempty"`
}

var ErrBindCodeRequired = errors.New("enter a current one-time connector code from the server; pending credentials were retained")

type BindOptions struct {
	Server, Code, StateDir, Name, OS, Arch string
	Agents                                 []domain.ConnectorAgentEnrollment
}

// RequestStatusError exposes only an HTTP status, never a remote error body.
// Workers can distinguish revocation from transient failures without logging
// credentials, prompts, filesystem paths or a server's arbitrary diagnostics.
type RequestStatusError struct{ StatusCode int }

func (err *RequestStatusError) Error() string {
	return fmt.Sprintf("connector request rejected (HTTP %d)", err.StatusCode)
}

type Client struct {
	server string
	http   *http.Client
}

func NewClient(server string) (*Client, error) {
	parsed, err := url.Parse(server)
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || (parsed.Path != "" && parsed.Path != "/") {
		return nil, errors.New("server must be an HTTPS origin without credentials, query or subpath")
	}
	address := net.ParseIP(parsed.Hostname())
	if parsed.Scheme != "https" && !(parsed.Scheme == "http" && address != nil && address.IsLoopback()) {
		return nil, errors.New("HTTPS is required except for a literal loopback development address")
	}
	return &Client{server: strings.TrimRight(server, "/"), http: &http.Client{
		Timeout:       15 * time.Second,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse },
	}}, nil
}

func randomConnectorToken(prefix string) (string, error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return prefix + base64.RawURLEncoding.EncodeToString(raw[:]), nil
}

// Bind uses an existing local Agent installation and the user's explicitly
// supplied code. It never starts an Agent, reads its login or opens a port.
func Bind(ctx context.Context, client *Client, options BindOptions) (domain.ConnectorIdentity, error) {
	var empty domain.ConnectorIdentity
	if options.Server != "" && strings.TrimRight(options.Server, "/") != client.server {
		return empty, errors.New("server mismatch")
	}
	if err := secureStateDirectory(options.StateDir); err != nil {
		return empty, err
	}
	lock, err := lockState(options.StateDir)
	if err != nil {
		return empty, err
	}
	defer lock()
	state, err := loadState(options.StateDir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return empty, err
	}
	if state != nil {
		if state.Server != client.server {
			return empty, errors.New("state belongs to a different server; choose a separate state directory")
		}
		if err := validateState(state); err != nil {
			return empty, err
		}
		if options.OS != state.OS || options.Arch != state.Arch {
			return empty, errors.New("saved enrollment belongs to a different platform; use a new dedicated profile")
		}
		if len(options.Agents) > 0 {
			kinds := map[string]bool{}
			for _, agent := range state.Agents {
				kinds[agent.Provider] = true
			}
			if len(kinds) != len(options.Agents) {
				return empty, errors.New("saved enrollment has a different Agent set; adding Agents requires a new dedicated profile in this development build")
			}
			for _, agent := range options.Agents {
				if !kinds[agent.Provider] {
					return empty, errors.New("saved enrollment has a different Agent set; use a separate dedicated profile")
				}
			}
		}
		identity, status, err := client.identity(ctx, state.Credential)
		if err == nil {
			if err := validateBoundIdentity(*state, identity); err != nil {
				return empty, err
			}
			state.Identity = &identity
			if err := saveState(options.StateDir, state); err != nil {
				return empty, err
			}
			return identity, nil
		}
		if state.Identity != nil || status != http.StatusUnauthorized {
			return empty, errors.New("cannot verify saved enrollment; credentials are unchanged; check server, revocation or expiry")
		}
	} else {
		credential, err := randomConnectorToken(domain.ConnectorDeviceTokenPrefix)
		if err != nil {
			return empty, err
		}
		state = &State{SchemaVersion: 1, Server: client.server, Name: options.Name, OS: options.OS, Arch: options.Arch, Credential: credential}
		for _, agent := range options.Agents {
			agent.Credential, err = randomConnectorToken(domain.ConnectorAgentTokenPrefix)
			if err != nil {
				return empty, err
			}
			state.Agents = append(state.Agents, agent)
		}
		if err := validateState(state); err != nil {
			return empty, err
		}
		if err := saveState(options.StateDir, state); err != nil {
			return empty, err
		}
	}
	if !domain.ValidConnectorToken(options.Code, domain.ConnectorBindCodePrefix) {
		return empty, ErrBindCodeRequired
	}
	input := domain.ConnectorEnrollment{Code: options.Code, Name: state.Name, OS: state.OS, Arch: state.Arch, Credential: state.Credential, Agents: state.Agents}
	body, err := json.Marshal(input)
	if err != nil {
		return empty, errors.New("cannot encode enrollment")
	}
	var identity domain.ConnectorIdentity
	_, err = client.request(ctx, http.MethodPost, "/runners/connectors/register", "", bytes.NewReader(body), &identity)
	if err != nil {
		return empty, errors.New("enrollment was not confirmed; rerun bind with the same state directory to recover before requesting a new code")
	}
	if err := validateBoundIdentity(*state, identity); err != nil {
		return empty, err
	}
	state.Identity = &identity
	if err := saveState(options.StateDir, state); err != nil {
		return empty, err
	}
	return identity, nil
}

func validateState(state *State) error {
	// Validation reuses the server's strict contract without persisting a code.
	fakeCode := domain.ConnectorBindCodePrefix + base64.RawURLEncoding.EncodeToString(make([]byte, 32))
	if state.SchemaVersion != 1 || domain.ValidateConnectorEnrollment(domain.ConnectorEnrollment{Code: fakeCode, Name: state.Name, OS: state.OS, Arch: state.Arch, Credential: state.Credential, Agents: state.Agents}) != nil {
		return errors.New("invalid local connector state; refusing to replace credentials")
	}
	return nil
}

func validateBoundIdentity(state State, identity domain.ConnectorIdentity) error {
	invalid := errors.New("server returned an inconsistent enrollment; local credentials were retained")
	if identity.Device.ID == "" || len(identity.Device.ID) > 64 || identity.Device.Revoked || identity.Device.Name != state.Name || identity.Device.OS != state.OS || identity.Device.Arch != state.Arch || !identity.Device.ExpiresAt.After(time.Now()) || len(identity.Agents) != len(state.Agents) {
		return invalid
	}
	if state.Identity != nil && identity.Device.ID != state.Identity.Device.ID {
		return invalid
	}
	seen := map[string]bool{}
	ids := map[string]bool{}
	for _, agent := range identity.Agents {
		if agent.DeviceID != identity.Device.ID || agent.ID == "" || len(agent.ID) > 64 || seen[agent.Provider] || ids[agent.ID] {
			return invalid
		}
		found := false
		for _, expected := range state.Agents {
			if expected.Provider == agent.Provider {
				found = true
				break
			}
		}
		if !found {
			return invalid
		}
		if state.Identity != nil {
			matched := false
			for _, saved := range state.Identity.Agents {
				if saved.Provider == agent.Provider && saved.ID == agent.ID {
					matched = true
					break
				}
			}
			if !matched {
				return invalid
			}
		}
		seen[agent.Provider], ids[agent.ID] = true, true
	}
	return nil
}

func (client *Client) identity(ctx context.Context, credential string) (domain.ConnectorIdentity, int, error) {
	var identity domain.ConnectorIdentity
	status, err := client.request(ctx, http.MethodGet, "/runners/connectors/self", credential, nil, &identity)
	return identity, status, err
}

func (client *Client) request(ctx context.Context, method, path, token string, body io.Reader, result any) (int, error) {
	return client.requestBounded(ctx, method, path, token, body, result, 64<<10)
}

func (client *Client) requestBounded(ctx context.Context, method, path, token string, body io.Reader, result any, maximum int64) (int, error) {
	request, err := http.NewRequestWithContext(ctx, method, client.server+path, body)
	if err != nil {
		return 0, errors.New("cannot create connector request")
	}
	request.Header.Set("Content-Type", "application/json")
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response, err := client.http.Do(request)
	if err != nil {
		return 0, errors.New("connector network request failed")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK && response.StatusCode != http.StatusCreated {
		return response.StatusCode, &RequestStatusError{StatusCode: response.StatusCode}
	}
	limited := &io.LimitedReader{R: response.Body, N: maximum + 1}
	data, err := io.ReadAll(limited)
	if err != nil || int64(len(data)) > maximum || json.Unmarshal(data, result) != nil {
		return response.StatusCode, errors.New("invalid connector response")
	}
	return response.StatusCode, nil
}

func loadState(directory string) (*State, error) {
	path := filepath.Join(directory, "state.json")
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > 64<<10 {
		return nil, errors.New("state is not a bounded regular file")
	}
	if err := checkStateFile(path, info); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, errors.New("cannot read connector state")
	}
	var state State
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, errors.New("invalid connector state")
	}
	return &state, nil
}

func saveState(directory string, state *State) error {
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return errors.New("cannot encode connector state")
	}
	file, err := os.CreateTemp(directory, ".state-")
	if err != nil {
		return errors.New("cannot create private connector state")
	}
	name := file.Name()
	defer os.Remove(name)
	if _, err = file.Write(data); err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err != nil || closeErr != nil {
		return errors.New("cannot persist connector state")
	}
	if err := os.Rename(name, filepath.Join(directory, "state.json")); err != nil {
		return errors.New("cannot commit connector state")
	}
	return nil
}
