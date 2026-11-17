package domain

import (
	"errors"
	"path"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const ConnectorLeaseTokenPrefix = "lfl_"
const (
	ConnectorTaskQueued           = "queued"
	ConnectorTaskClaimed          = "claimed"
	ConnectorTaskRunning          = "running"
	ConnectorTaskCancelRequested  = "cancel_requested"
	ConnectorTaskRecoveryRequired = "recovery_required"
	ConnectorTaskSucceeded        = "succeeded"
	ConnectorTaskFailed           = "failed"
	ConnectorTaskCanceled         = "canceled"
)

type ConnectorTaskFile struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

// Only a typed prompt and explicitly supplied text inputs cross this boundary.
// No remote argv, shell, binary path, local directory or write grant is accepted.
type ConnectorTaskRequest struct {
	SchemaVersion  int                 `json:"schemaVersion"`
	IdempotencyKey string              `json:"idempotencyKey"`
	AgentID        string              `json:"agentId"`
	Kind           string              `json:"kind"`
	Prompt         string              `json:"prompt"`
	Files          []ConnectorTaskFile `json:"files"`
	TimeoutSeconds int                 `json:"timeoutSeconds"`
}

type ConnectorTaskResult struct {
	Status   string              `json:"status"`
	Text     string              `json:"text"`
	Files    []ConnectorTaskFile `json:"files"`
	Quiesced bool                `json:"quiesced"`
}

type ConnectorTask struct {
	ID                   string                `json:"id"`
	AgentID              string                `json:"agentId"`
	Provider             string                `json:"provider"`
	Status               string                `json:"status"`
	Attempt              int                   `json:"attempt"`
	RunAllowed           bool                  `json:"runAllowed"`
	LeaseRemainingMillis int64                 `json:"leaseRemainingMillis"`
	CancelRequested      bool                  `json:"cancelRequested"`
	RecoveryReason       string                `json:"recoveryReason,omitempty"`
	LeaseUntil           *time.Time            `json:"leaseUntil,omitempty"`
	DeadlineAt           *time.Time            `json:"deadlineAt,omitempty"`
	Input                *ConnectorTaskRequest `json:"input,omitempty"`
	Result               *ConnectorTaskResult  `json:"result,omitempty"`
	CreatedAt            time.Time             `json:"createdAt"`
	UpdatedAt            time.Time             `json:"updatedAt"`
}

type ConnectorTaskLease struct {
	Attempt    int    `json:"attempt"`
	LeaseToken string `json:"leaseToken"`
}

type ConnectorTaskCompletion struct {
	Attempt    int                 `json:"attempt"`
	LeaseToken string              `json:"leaseToken"`
	Result     ConnectorTaskResult `json:"result"`
}

func ConnectorTaskTerminal(status string) bool {
	return status == ConnectorTaskSucceeded || status == ConnectorTaskFailed || status == ConnectorTaskCanceled
}

func NormalizeConnectorTaskRequest(input ConnectorTaskRequest) (ConnectorTaskRequest, error) {
	invalid := errors.New("invalid typed connector task")
	if input.SchemaVersion != 1 || input.Kind != "prompt" || len(input.AgentID) < 1 || len(input.AgentID) > 64 || len(input.IdempotencyKey) < 1 || len(input.IdempotencyKey) > 128 {
		return input, invalid
	}
	for _, r := range input.IdempotencyKey {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("._:-", r)) {
			return input, invalid
		}
	}
	if strings.TrimSpace(input.Prompt) == "" || len(input.Prompt) > 32<<10 || !utf8.ValidString(input.Prompt) || strings.ContainsRune(input.Prompt, 0) {
		return input, invalid
	}
	if input.TimeoutSeconds == 0 {
		input.TimeoutSeconds = 300
	}
	if input.TimeoutSeconds < 10 || input.TimeoutSeconds > 1800 {
		return input, invalid
	}
	files, err := normalizeConnectorFiles(input.Files, 64<<10, 256<<10)
	if err != nil {
		return input, err
	}
	input.Files = files
	return input, nil
}

func NormalizeConnectorTaskResult(result ConnectorTaskResult) (ConnectorTaskResult, error) {
	if !result.Quiesced || !ConnectorTaskTerminal(result.Status) || len(result.Text) > 128<<10 || !utf8.ValidString(result.Text) || strings.ContainsRune(result.Text, 0) {
		return result, errors.New("invalid or unquiesced connector task result")
	}
	files, err := normalizeConnectorFiles(result.Files, 256<<10, 512<<10)
	if err != nil {
		return result, err
	}
	result.Files = files
	return result, nil
}

func normalizeConnectorFiles(files []ConnectorTaskFile, perFile, total int) ([]ConnectorTaskFile, error) {
	invalid := errors.New("invalid connector task files")
	if len(files) > 64 {
		return nil, invalid
	}
	out := append([]ConnectorTaskFile{}, files...)
	seen := map[string]bool{}
	for _, file := range out {
		if !connectorTaskPath(file.Path) || len(file.Content) > perFile || !utf8.ValidString(file.Content) || strings.ContainsRune(file.Content, 0) {
			return nil, invalid
		}
		total -= len(file.Content)
		if total < 0 {
			return nil, invalid
		}
		key := strings.Map(func(r rune) rune {
			smallest := r
			for next := unicode.SimpleFold(r); next != r; next = unicode.SimpleFold(next) {
				if next < smallest {
					smallest = next
				}
			}
			return smallest
		}, file.Path)
		if seen[key] {
			return nil, invalid
		}
		for other := range seen {
			if strings.HasPrefix(key, other+"/") || strings.HasPrefix(other, key+"/") {
				return nil, invalid
			}
		}
		seen[key] = true
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

func connectorTaskPath(name string) bool {
	if name == "" || len(name) > 512 || !utf8.ValidString(name) || path.Clean(name) != name || strings.HasPrefix(name, "/") || name == "." || name == ".." || strings.HasPrefix(name, "../") {
		return false
	}
	for _, r := range name {
		if r < 32 || r == 127 || strings.ContainsRune(`\:*?"<>|`, r) {
			return false
		}
	}
	for _, part := range strings.Split(name, "/") {
		if strings.HasSuffix(part, ".") || strings.HasSuffix(part, " ") {
			return false
		}
		base := strings.ToUpper(strings.TrimSpace(strings.SplitN(part, ".", 2)[0]))
		runes := []rune(base)
		if base == "CON" || base == "PRN" || base == "AUX" || base == "NUL" || (len(runes) == 4 && (strings.HasPrefix(base, "COM") || strings.HasPrefix(base, "LPT")) && strings.ContainsRune("123456789¹²³", runes[3])) {
			return false
		}
	}
	return true
}

type ConnectorTaskEvent struct {
	ID         int64      `json:"id"`
	TaskID     string     `json:"taskId"`
	Attempt    int        `json:"attempt"`
	Action     string     `json:"action"`
	FromStatus string     `json:"fromStatus"`
	ToStatus   string     `json:"toStatus"`
	AgentID    string     `json:"agentId,omitempty"`
	OwnerID    int64      `json:"ownerId,omitempty"`
	LeaseUntil *time.Time `json:"leaseUntil,omitempty"`
	DeadlineAt *time.Time `json:"deadlineAt,omitempty"`
	CreatedAt  time.Time  `json:"createdAt"`
}
