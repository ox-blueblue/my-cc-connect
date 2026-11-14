//go:build linux

package codex

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestCodexSession_ResumePreemptsActiveWriterBeforeExec(t *testing.T) {
	workDir := t.TempDir()
	codexHome := t.TempDir()
	lockDir := filepath.Join(codexHome, "thread-writer-locks")
	if err := os.MkdirAll(lockDir, 0o755); err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(lockDir, "thread-exec.lock")
	if err := os.WriteFile(lockPath, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	writer := startWriterLockHelper(t, lockPath)

	binDir := filepath.Join(workDir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFakeCodexScript(t, binDir,
		"#!/bin/sh\n"+
			"printf '%s\\n' '{\"type\":\"thread.started\",\"thread_id\":\"thread-exec\"}'\n"+
			"printf '%s\\n' '{\"type\":\"turn.completed\"}'\n",
		"[Console]::Out.WriteLine('{\"type\":\"thread.started\",\"thread_id\":\"thread-exec\"}')\n"+
			"[Console]::Out.WriteLine('{\"type\":\"turn.completed\"}')\n")
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	cs, err := newCodexSession(context.Background(), "codex", nil, workDir, "", "", "", "thread-exec", "", []string{"CODEX_HOME=" + codexHome}, "", "", "")
	if err != nil {
		t.Fatalf("newCodexSession: %v", err)
	}
	defer cs.Close()

	if err := cs.Send("resume", "", nil, nil); err != nil {
		t.Fatalf("Send: %v", err)
	}
	waitForDoneResult(t, cs.Events())
	waitForWriterHelperExit(t, writer)
}
