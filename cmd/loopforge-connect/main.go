package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"

	"agentflow/internal/connect"
)

func run(args []string, out, errorOut io.Writer) int {
	if len(args) == 0 || args[0] != "doctor" {
		fmt.Fprintln(errorOut, "Usage: loopforge-connect doctor [--agents all|codex,dsh,minimax-code]")
		fmt.Fprintln(errorOut, "Local CLI inventory only. This development command does not bind, install or run Agent tasks.")
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

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }
