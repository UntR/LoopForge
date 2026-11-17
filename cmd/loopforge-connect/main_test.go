package main

import (
	"bytes"
	"testing"
)

func TestRejectTaskOrUnsupportedBindingInput(t *testing.T) {
	for _, args := range [][]string{nil, {"bind"}, {"doctor", "--agents", "shell"}, {"doctor", "run-this-command"}} {
		var out, errors bytes.Buffer
		if code := run(args, &out, &errors); code != 2 || out.Len() != 0 {
			t.Fatalf("run(%v) = %d, output %s", args, code, out.String())
		}
	}
}
