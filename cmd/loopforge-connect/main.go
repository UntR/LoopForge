package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"agentflow/internal/connect"
	"agentflow/internal/domain"
)

func run(args []string, out, errorOut io.Writer) int {
	return runWithInput(args, os.Stdin, out, errorOut)
}

func runWithInput(args []string, input io.Reader, out, errorOut io.Writer) int {
	if len(args) > 0 && args[0] == "bind" {
		return runBind(args[1:], input, out, errorOut)
	}
	if len(args) == 0 || args[0] != "doctor" {
		fmt.Fprintln(errorOut, "Usage: loopforge-connect doctor [--agents all|codex,dsh,minimax-code]")
		fmt.Fprintln(errorOut, "Use loopforge-connect bind --server https://your-server to register existing local Agents. This build does not execute tasks.")
		return 2
	}
	flags := flag.NewFlagSet("doctor", flag.ContinueOnError)
	flags.SetOutput(errorOut)
	agents := flags.String("agents", "all", "comma-separated local Agent kinds")
	if err := flags.Parse(args[1:]); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(errorOut, "doctor does not accept commands or prompts")
		return 2
	}
	selected, err := connect.Select(*agents)
	if err != nil {
		fmt.Fprintln(errorOut, err)
		return 2
	}
	report := connect.Report{
		SchemaVersion: 1, OS: runtime.GOOS, Arch: runtime.GOARCH,
		Checks: connect.Inspect(context.Background(), selected, exec.LookPath, connect.Version),
	}
	encoder := json.NewEncoder(out)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(report); err != nil {
		fmt.Fprintln(errorOut, "cannot write inventory")
		return 1
	}
	for _, check := range report.Checks {
		if check.Status != "available" {
			return 1
		}
	}
	return 0
}

func runBind(args []string, input io.Reader, out, errorOut io.Writer) int {
	flags := flag.NewFlagSet("bind", flag.ContinueOnError)
	flags.SetOutput(errorOut)
	server := flags.String("server", "", "HTTPS server origin")
	agents := flags.String("agents", "all", "Agent kinds to register")
	stateDir := flags.String("state-dir", "", "dedicated credential directory (default ~/.loopforge-connect)")
	name := flags.String("name", "", "machine display name (default hostname)")
	if flags.Parse(args) != nil || flags.NArg() != 0 || *server == "" {
		fmt.Fprintln(errorOut, "Usage: loopforge-connect bind --server https://your-server [--agents all|codex,dsh,minimax-code]")
		return 2
	}
	client, err := connect.NewClient(*server)
	if err != nil {
		fmt.Fprintln(errorOut, err)
		return 2
	}
	selected, err := connect.Select(*agents)
	if err != nil {
		fmt.Fprintln(errorOut, err)
		return 2
	}
	checks := connect.Inspect(context.Background(), selected, exec.LookPath, connect.Version)
	var available []domain.ConnectorAgentEnrollment
	for _, check := range checks {
		if check.Status == "available" {
			version := strings.SplitN(check.Version, "\n", 2)[0]
			if len(version) > 256 {
				version = version[:256]
			}
			available = append(available, domain.ConnectorAgentEnrollment{Provider: check.Kind, Version: strings.TrimSpace(version)})
		} else if *agents != "all" {
			fmt.Fprintln(errorOut, "Selected Agent is unavailable:", check.Kind)
			return 1
		}
	}
	if len(available) == 0 {
		fmt.Fprintln(errorOut, "No supported Agent CLI is available; no software was installed.")
		return 1
	}
	if *stateDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			fmt.Fprintln(errorOut, "Cannot locate credential directory")
			return 1
		}
		*stateDir = filepath.Join(home, ".loopforge-connect")
	}
	if *name == "" {
		*name, _ = os.Hostname()
	}
	options := connect.BindOptions{Server: *server, StateDir: *stateDir, Name: *name, OS: runtime.GOOS, Arch: runtime.GOARCH, Agents: available}
	identity, err := connect.Bind(context.Background(), client, options)
	if errors.Is(err, connect.ErrBindCodeRequired) {
		fmt.Fprint(errorOut, "Paste the one-time connector code (not an Agent API key): ")
		line, readErr := bufio.NewReader(io.LimitReader(input, 256)).ReadString('\n')
		if readErr != nil && readErr != io.EOF {
			fmt.Fprintln(errorOut, "Cannot read bind code")
			return 1
		}
		options.Code = strings.TrimSpace(line)
		if options.Code == "" {
			fmt.Fprintln(errorOut, "No code supplied; local pending credentials were retained.")
			return 1
		}
		identity, err = connect.Bind(context.Background(), client, options)
	}
	if err != nil {
		fmt.Fprintln(errorOut, err)
		return 1
	}
	if err := json.NewEncoder(out).Encode(identity); err != nil {
		fmt.Fprintln(errorOut, "Cannot write registration result")
		return 1
	}
	fmt.Fprintln(errorOut, "Registered independent identities. Task execution is not included in this development build.")
	return 0
}

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }
