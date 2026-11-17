package connect

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestSelectAgents(t *testing.T) {
	for _, raw := range []string{"", "all", "codex,dsh,minimax-code"} {
		got, err := Select(raw)
		if err != nil || len(got) != 3 {
			t.Fatalf("Select(%q) = %v, %v", raw, got, err)
		}
	}
	got, err := Select("minimax-code,codex,minimax-code")
	if err != nil || len(got) != 2 || got[0].Binary != "mcode" {
		t.Fatalf("deduplicated selection = %v, %v", got, err)
	}
	for _, raw := range []string{"terminal", "mmx", "codex,", "codex;echo unsafe"} {
		if _, err := Select(raw); err == nil {
			t.Fatalf("accepted unrecognized agent %q", raw)
		}
	}
	items := Providers()
	items[0].Kind = "mutated"
	if Providers()[0].Kind != "codex" {
		t.Fatal("caller mutated allowlist")
	}
}

func TestInspectDoesNotProbeMissingAgentsOrExposeErrors(t *testing.T) {
	var probed []string
	checks := Inspect(context.Background(), Providers(), func(binary string) (string, error) {
		if binary == "dsh" {
			return "", errors.New("private lookup details")
		}
		return binary, nil
	}, func(_ context.Context, binary string) (string, error) {
		probed = append(probed, binary)
		if binary == "mcode" {
			return "", errors.New("private diagnostic")
		}
		return " codex test-version\n", nil
	})
	if len(probed) != 2 || checks[0].Status != "available" || checks[0].Version != "codex test-version" || checks[1].Status != "missing" || checks[2].Status != "unusable" {
		t.Fatalf("unexpected inventory: %+v, probes %v", checks, probed)
	}
	for _, check := range checks {
		if strings.Contains(check.Reason, "private") {
			t.Fatal("probe error disclosed")
		}
	}
}

func TestInspectEmptyVersionIsNotAvailability(t *testing.T) {
	checks := Inspect(context.Background(), Providers()[:1], func(string) (string, error) { return "fixture", nil }, func(context.Context, string) (string, error) { return " \n", nil })
	if checks[0].Status != "unusable" {
		t.Fatal(checks)
	}
}

func TestVersionOutputIsBoundedWithoutFailingWriter(t *testing.T) {
	var output limitedOutput
	data := []byte(strings.Repeat("x", 8192))
	for i := 0; i < 3; i++ {
		if n, err := output.Write(data); n != len(data) || err != nil {
			t.Fatalf("write = %d, %v", n, err)
		}
	}
	if output.Len() != 4096 {
		t.Fatalf("retained %d bytes", output.Len())
	}
}
