//go:build linux

package codex

import (
	"strings"
	"testing"
)

func TestParseProcLocks_FindsExactOwnerAndCountsSharedLocks(t *testing.T) {
	data := strings.Join([]string{
		"1: FLOCK  ADVISORY  WRITE 4242 00:2a 12345 0 EOF",
		"2: FLOCK  ADVISORY  WRITE 4242 00:2a 12346 0 EOF",
		"3: POSIX  ADVISORY  WRITE 5252 00:2a 12345 0 EOF",
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
}

func TestPreemptCodexThreadWriter_MissingLock(t *testing.T) {
	if err := preemptCodexThreadWriter(t.TempDir(), "thread-missing"); err == nil {
		t.Fatal("preemptCodexThreadWriter() succeeded without a lock")
	}
}
