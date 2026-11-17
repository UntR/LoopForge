// Package connect contains portable enrollment and local CLI inventory.
// Keep it independent of the platform runtime so Windows builds do not
// inherit POSIX sandbox and process-control dependencies.
package connect

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os/exec"
	"strings"
	"time"
)

type Provider struct {
	Kind   string `json:"kind"`
	Binary string `json:"binary"`
}

var providers = []Provider{
	{Kind: "codex", Binary: "codex"},
	{Kind: "dsh", Binary: "dsh"},
	{Kind: "minimax-code", Binary: "mcode"},
}

// Providers returns a copy: callers cannot mutate the connector allowlist.
func Providers() []Provider { return append([]Provider(nil), providers...) }

type Check struct {
	Kind    string `json:"kind"`
	Binary  string `json:"binary"`
	Status  string `json:"status"`
	Version string `json:"version,omitempty"`
	Reason  string `json:"reason,omitempty"`
}

// Report deliberately makes no claim about credentials, registration or task
// execution. A successful --version probe only proves local CLI availability.
type Report struct {
	SchemaVersion int     `json:"schemaVersion"`
	OS            string  `json:"os"`
	Arch          string  `json:"arch"`
	Connected     bool    `json:"connected"`
	Checks        []Check `json:"checks"`
}

func Select(raw string) ([]Provider, error) {
	if raw == "" || strings.TrimSpace(raw) == "all" {
		return Providers(), nil
	}
	var result []Provider
	seen := map[string]bool{}
	for _, name := range strings.Split(raw, ",") {
		name = strings.TrimSpace(name)
		matched := false
		for _, provider := range providers {
			if provider.Kind == name {
				matched = true
				if !seen[name] {
					result = append(result, provider)
					seen[name] = true
				}
				break
			}
		}
		if !matched {
			return nil, errors.New("unknown agent; choose codex, dsh, minimax-code or all")
		}
	}
	return result, nil
}

// Inspect uses injectable probes so tests never invoke a model, installer or
// a user's real Agent executable. No configuration or login files are read.
func Inspect(ctx context.Context, selected []Provider, lookup func(string) (string, error), probe func(context.Context, string) (string, error)) []Check {
	result := make([]Check, 0, len(selected))
	for _, provider := range selected {
		check := Check{Kind: provider.Kind, Binary: provider.Binary, Status: "missing"}
		path, err := lookup(provider.Binary)
		if err != nil {
			check.Reason = "CLI not found on PATH; install it separately or expose the existing CLI on PATH"
			result = append(result, check)
			continue
		}
		version, err := probe(ctx, path)
		if err != nil || strings.TrimSpace(version) == "" {
			check.Status = "unusable"
			check.Reason = "version probe failed or timed out; inspect the local CLI separately"
		} else {
			check.Status = "available"
			check.Version = strings.TrimSpace(version)
		}
		result = append(result, check)
	}
	return result
}

// Version only executes the fixed --version argument, never task text or a
// shell command. Bound memory and duration even if a broken CLI is noisy.
func Version(ctx context.Context, binary string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, "--version")
	var output limitedOutput
	cmd.Stdout = &output
	cmd.Stderr = io.Discard
	if err := cmd.Run(); err != nil {
		return "", err
	}
	return output.String(), nil
}

type limitedOutput struct{ bytes.Buffer }

func (output *limitedOutput) Write(p []byte) (int, error) {
	n := len(p)
	if left := 4096 - output.Len(); left > 0 {
		if len(p) > left {
			p = p[:left]
		}
		_, _ = output.Buffer.Write(p)
	}
	return n, nil
}
