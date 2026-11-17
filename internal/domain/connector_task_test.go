package domain

import (
	"strings"
	"testing"
)

func TestConnectorTaskContractCanonicalAndBounded(t *testing.T) {
	input := ConnectorTaskRequest{SchemaVersion: 1, IdempotencyKey: "Request:A-1", AgentID: "agent-fixture", Kind: "prompt", Prompt: "summarize only the supplied input", Files: []ConnectorTaskFile{{Path: "笔记/说明.txt", Content: "合成输入"}, {Path: "a.txt", Content: "fixture"}}}
	got, err := NormalizeConnectorTaskRequest(input)
	if err != nil || got.TimeoutSeconds != 300 || got.Files[0].Path != "a.txt" {
		t.Fatalf("normalize=%+v %v", got, err)
	}
	if input.Files[0].Path != "笔记/说明.txt" {
		t.Fatal("normalization mutated caller input")
	}
	for _, mutate := range []func(*ConnectorTaskRequest){
		func(v *ConnectorTaskRequest) { v.SchemaVersion = 0 }, func(v *ConnectorTaskRequest) { v.Kind = "shell" }, func(v *ConnectorTaskRequest) { v.Prompt = " " }, func(v *ConnectorTaskRequest) { v.Prompt = strings.Repeat("x", 32<<10+1) }, func(v *ConnectorTaskRequest) { v.Prompt = "x\x00y" }, func(v *ConnectorTaskRequest) { v.IdempotencyKey = "key with spaces" }, func(v *ConnectorTaskRequest) { v.TimeoutSeconds = 9 }, func(v *ConnectorTaskRequest) { v.TimeoutSeconds = 1801 },
	} {
		copy := input
		mutate(&copy)
		if _, err := NormalizeConnectorTaskRequest(copy); err == nil {
			t.Fatal("accepted invalid typed task", copy.Kind)
		}
	}
}

func TestConnectorTaskFilesRejectPortableEscapesAndCollisions(t *testing.T) {
	for _, name := range []string{"../outside", "/absolute", "a/../b", "a//b", "a/./b", `C:\private`, "a:b", "a\\b", "trailing.", "trailing ", "NUL", "con.txt", "CON .txt", "COM1.log", "COM¹.log", "LPT³", "a\nfile", ""} {
		if _, err := normalizeConnectorFiles([]ConnectorTaskFile{{Path: name, Content: "fixture"}}, 1024, 1024); err == nil {
			t.Fatalf("accepted nonportable path %q", name)
		}
	}
	for _, files := range [][]ConnectorTaskFile{
		{{Path: "A.txt"}, {Path: "a.TXT"}}, {{Path: "Σ"}, {Path: "ς"}}, {{Path: "folder"}, {Path: "folder/file"}}, {{Path: "folder/file"}, {Path: "folder"}}, {{Path: "a", Content: "bad\x00input"}}, {{Path: "a", Content: string([]byte{255})}},
	} {
		if _, err := normalizeConnectorFiles(files, 1024, 1024); err == nil {
			t.Fatal("accepted ambiguous or binary file set")
		}
	}
	files := []ConnectorTaskFile{}
	for _, name := range []string{"a", "b", "c", "d", "e"} {
		files = append(files, ConnectorTaskFile{Path: name, Content: strings.Repeat("x", 64<<10)})
	}
	if _, err := normalizeConnectorFiles(files, 64<<10, 256<<10); err == nil {
		t.Fatal("accepted excessive aggregate input")
	}
}

func TestConnectorTaskResultRequiresQuiescence(t *testing.T) {
	for _, result := range []ConnectorTaskResult{{Status: ConnectorTaskSucceeded}, {Status: ConnectorTaskCanceled}, {Status: "running", Quiesced: true}, {Status: ConnectorTaskSucceeded, Quiesced: true, Text: strings.Repeat("x", 128<<10+1)}} {
		if _, err := NormalizeConnectorTaskResult(result); err == nil {
			t.Fatal("accepted unconfirmed or unbounded result")
		}
	}
	if _, err := NormalizeConnectorTaskResult(ConnectorTaskResult{Status: ConnectorTaskCanceled, Quiesced: true}); err != nil {
		t.Fatal(err)
	}
}
