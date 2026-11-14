//go:build linux

package codex

import (
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestParseProcLocks_FindsExactOwnerAndCountsSharedLocks(t *testing.T) {
	data := strings.Join([]string{
		"1: FLOCK  ADVISORY  WRITE 4242 00:2a:12345 0 EOF",
		"2: FLOCK  ADVISORY  WRITE 4242 00:2a:12346 0 EOF",
		"3: POSIX  ADVISORY  WRITE 5252 00:2a:12345 0 EOF",
		"malformed lock line",
	}, "\n")

	locks := parseProcLocks([]byte(data))
	pid, ok := findProcLockOwner(locks, "00:2a", 12345)
	if !ok || pid != 4242 {
		t.Fatalf("owner = %d, found = %v, want pid 4242", pid, ok)
	}
	if got := countProcLocksForPID(locks, 4242); got != 2 {
		t.Fatalf("lock count for pid 4242 = %d, want 2", got)
	}
	if got := countProcLocksForPID(locks, 5252); got != 1 {
		t.Fatalf("lock count for pid 5252 = %d, want 1", got)
	}
}

func TestIsCodexProcessCmdline(t *testing.T) {
	if !isCodexProcessCmdline([]byte("/home/zj/.local/bin/codex\x00app-server\x00")) {
		t.Fatal("Codex command line was rejected")
	}
	if isCodexProcessCmdline([]byte("/usr/bin/python3\x00codex-helper\x00")) {
		t.Fatal("non-Codex command line was accepted")
	}
	if isCodexProcessCmdline([]byte("/usr/bin/not-codex\x00app-server\x00")) {
		t.Fatal("not-codex command line was accepted")
	}
}

func TestIsCodexExecutablePath(t *testing.T) {
	if !isCodexExecutablePath("/home/zj/.local/bin/codex") {
		t.Fatal("Codex executable path was rejected")
	}
	if isCodexExecutablePath("/tmp/not-codex") {
		t.Fatal("not-Codex executable path was accepted")
	}
}

func TestPreemptCodexThreadWriter_MissingLock(t *testing.T) {
	if err := preemptCodexThreadWriter(t.TempDir(), "thread-missing"); err == nil {
		t.Fatal("preemptCodexThreadWriter() succeeded without a lock")
	}
}

func TestPreemptCodexThreadWriter_KillsExclusiveWriter(t *testing.T) {
	home := t.TempDir()
	lockDir := filepath.Join(home, "thread-writer-locks")
	if err := os.MkdirAll(lockDir, 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(lockDir, "thread-exclusive.lock")
	if err := os.WriteFile(target, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := startWriterLockHelper(t, target)

	if err := preemptCodexThreadWriter(home, "thread-exclusive"); err != nil {
		t.Fatalf("preemptCodexThreadWriter() error = %v", err)
	}
	waitForWriterHelperExit(t, cmd)
}

func TestPreemptCodexThreadWriter_DoesNotKillSharedWriter(t *testing.T) {
	home := t.TempDir()
	lockDir := filepath.Join(home, "thread-writer-locks")
	if err := os.MkdirAll(lockDir, 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(lockDir, "thread-shared.lock")
	other := filepath.Join(lockDir, "other.lock")
	for _, path := range []string{target, other} {
		if err := os.WriteFile(path, nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cmd := startWriterLockHelper(t, target, other)

	err := preemptCodexThreadWriter(home, "thread-shared")
	if err == nil || !strings.Contains(err.Error(), "multiple thread locks") {
		t.Fatalf("preemptCodexThreadWriter() error = %v, want multiple-lock refusal", err)
	}
	if err := cmd.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatalf("shared writer was terminated: %v", err)
	}
	_ = cmd.Process.Kill()
	waitForWriterHelperExit(t, cmd)
}

func TestWriterPreemptHelper(t *testing.T) {
	if os.Getenv("CC_WRITER_HELPER") != "1" {
		return
	}
	files := make([]*os.File, 0)
	for _, path := range strings.Split(os.Getenv("CC_WRITER_LOCKS"), "|") {
		file, err := os.OpenFile(path, os.O_RDWR, 0)
		if err != nil {
			os.Exit(20)
		}
		if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX); err != nil {
			os.Exit(21)
		}
		files = append(files, file)
	}
	if err := os.WriteFile(os.Getenv("CC_WRITER_READY"), []byte("ready"), 0o600); err != nil {
		os.Exit(22)
	}
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGTERM)
	<-signals
}

func startWriterLockHelper(t *testing.T, locks ...string) *exec.Cmd {
	t.Helper()
	dir := t.TempDir()
	binary, err := os.ReadFile(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	helper := filepath.Join(dir, "codex")
	if err := os.WriteFile(helper, binary, 0o755); err != nil {
		t.Fatal(err)
	}
	ready := filepath.Join(dir, "ready")
	cmd := exec.Command(helper, "-test.run=^TestWriterPreemptHelper$")
	cmd.Args[0] = "codex"
	cmd.Env = append(os.Environ(),
		"CC_WRITER_HELPER=1",
		"CC_WRITER_READY="+ready,
		"CC_WRITER_LOCKS="+strings.Join(locks, "|"),
	)
	cmd.Stdin = strings.NewReader("")
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		_ = cmd.Wait()
	})
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Stat(ready); err == nil {
			return cmd
		}
		if time.Now().After(deadline) {
			t.Fatalf("writer helper did not acquire locks")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func waitForWriterHelperExit(t *testing.T, cmd *exec.Cmd) {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("writer helper did not exit")
	}
}
