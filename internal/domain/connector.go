package domain

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strings"
	"time"
)

const ConnectorDeviceTokenPrefix = "lfd_"
const ConnectorAgentTokenPrefix = "lfa_"
const ConnectorBindCodePrefix = "lfbind_"

// Connector identities are distinct from workflow role definitions and legacy
// Runner credentials. Enrollment alone never advertises task readiness.
type ConnectorDevice struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	OS        string    `json:"os"`
	Arch      string    `json:"arch"`
	Revoked   bool      `json:"revoked"`
	ExpiresAt time.Time `json:"expiresAt"`
	CreatedAt time.Time `json:"createdAt"`
}

type ConnectorAgent struct {
	ID        string    `json:"id"`
	DeviceID  string    `json:"deviceId"`
	Provider  string    `json:"provider"`
	Version   string    `json:"version"`
	Revoked   bool      `json:"revoked"`
	CreatedAt time.Time `json:"createdAt"`
}

type ConnectorIdentity struct {
	Device ConnectorDevice  `json:"device"`
	Agents []ConnectorAgent `json:"agents"`
}

type ConnectorAgentEnrollment struct {
	Provider   string `json:"provider"`
	Version    string `json:"version"`
	Credential string `json:"credential"`
}

type ConnectorEnrollment struct {
	Code       string                     `json:"code"`
	Name       string                     `json:"name"`
	OS         string                     `json:"os"`
	Arch       string                     `json:"arch"`
	Credential string                     `json:"credential"`
	Agents     []ConnectorAgentEnrollment `json:"agents"`
}

func ConnectorTokenHash(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

func ValidConnectorToken(raw, prefix string) bool {
	if !strings.HasPrefix(raw, prefix) {
		return false
	}
	decoded, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(raw, prefix))
	return err == nil && len(decoded) == 32 && prefix+base64.RawURLEncoding.EncodeToString(decoded) == raw
}

func ValidateConnectorEnrollment(input ConnectorEnrollment) error {
	invalid := errors.New("invalid connector enrollment")
	if !ValidConnectorToken(input.Code, ConnectorBindCodePrefix) || !ValidConnectorToken(input.Credential, ConnectorDeviceTokenPrefix) {
		return invalid
	}
	if strings.TrimSpace(input.Name) == "" || len(input.Name) > 128 || strings.ContainsAny(input.Name, "\r\n\x00") {
		return invalid
	}
	switch input.OS {
	case "linux", "darwin", "windows":
	default:
		return invalid
	}
	if input.Arch != "amd64" && input.Arch != "arm64" {
		return invalid
	}
	if len(input.Agents) < 1 || len(input.Agents) > 3 {
		return invalid
	}
	seen := map[string]bool{}
	for _, agent := range input.Agents {
		switch agent.Provider {
		case "codex", "dsh", "minimax-code":
		default:
			return invalid
		}
		if seen[agent.Provider] || len(agent.Version) > 256 || strings.ContainsAny(agent.Version, "\r\n\x00") || !ValidConnectorToken(agent.Credential, ConnectorAgentTokenPrefix) {
			return invalid
		}
		if seen[agent.Credential] {
			return invalid
		}
		seen[agent.Provider], seen[agent.Credential] = true, true
	}
	return nil
}
