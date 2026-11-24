package main

import (
	"bytes"
	"os/exec"
	"testing"
)

// runChild executes a command with the given environment and returns its
// trimmed stdout (test helper for run injection).
func runChild(t *testing.T, script string, env []string) string {
	t.Helper()
	cmd := exec.Command(script)
	cmd.Env = env
	var buf bytes.Buffer
	cmd.Stdout = &buf
	if err := cmd.Run(); err != nil {
		t.Fatalf("child run: %v", err)
	}
	return buf.String()
}
