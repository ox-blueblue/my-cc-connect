//go:build linux

package codex

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	procLocksPath            = "/proc/locks"
	writerGracePeriod        = 750 * time.Millisecond
	writerPollInterval       = 20 * time.Millisecond
	writerForcedKillWaitTime = 750 * time.Millisecond
)

type procLock struct {
	device string
	inode  uint64
	pid    int
}

func parseProcLocks(data []byte) []procLock {
	var locks []procLock
	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 7 {
			continue
		}
		switch fields[1] {
		case "FLOCK", "POSIX", "OFDLCK":
		default:
			continue
		}
		pid, err := strconv.Atoi(fields[4])
		if err != nil || pid <= 0 {
			continue
		}
		inode, err := strconv.ParseUint(fields[6], 10, 64)
		if err != nil {
			continue
		}
		locks = append(locks, procLock{device: fields[5], inode: inode, pid: pid})
	}
	return locks
}

func findProcLockOwner(locks []procLock, device string, inode uint64) (int, bool) {
	for _, lock := range locks {
		if lock.device == device && lock.inode == inode {
			return lock.pid, true
		}
	}
	return 0, false
}

func countProcLocksForPID(locks []procLock, pid int) int {
	count := 0
	for _, lock := range locks {
		if lock.pid == pid {
			count++
		}
	}
	return count
}

func isCodexProcessCmdline(cmdline []byte) bool {
	args := strings.Split(string(cmdline), "\x00")
	if len(args) == 0 || strings.TrimSpace(args[0]) == "" {
		return false
	}
	name := strings.ToLower(filepath.Base(strings.TrimSpace(args[0])))
	return strings.Contains(name, "codex")
}

func preemptCodexThreadWriter(codexHome, threadID string) error {
	if strings.TrimSpace(threadID) == "" || strings.ContainsAny(threadID, `/\`) {
		return fmt.Errorf("codex writer preemption: invalid thread id")
	}
	lockPath := filepath.Join(resolveCodexHomeDir(codexHome), "thread-writer-locks", threadID+".lock")
	owner, err := inspectWriterLock(lockPath)
	if err != nil {
		return err
	}
	if err := validateWriterOwner(owner); err != nil {
		return err
	}

	// Re-read the lock immediately before signaling to avoid killing a PID
	// that has been reused or a writer that changed between inspection steps.
	confirmed, err := inspectWriterLock(lockPath)
	if err != nil {
		return err
	}
	if confirmed != owner {
		return fmt.Errorf("codex writer preemption: target writer changed before termination")
	}

	process, err := os.FindProcess(owner.pid)
	if err != nil {
		return fmt.Errorf("codex writer preemption: find pid %d: %w", owner.pid, err)
	}
	if err := process.Signal(syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("codex writer preemption: stop pid %d: %w", owner.pid, err)
	}
	if released, err := waitForWriterRelease(owner, writerGracePeriod); err != nil {
		return err
	} else if released {
		return nil
	}

	confirmed, err = inspectWriterLock(lockPath)
	if err != nil {
		return err
	}
	if confirmed != owner {
		return fmt.Errorf("codex writer preemption: target writer changed before force termination")
	}
	if err := process.Signal(syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("codex writer preemption: force stop pid %d: %w", owner.pid, err)
	}
	if released, err := waitForWriterRelease(owner, writerForcedKillWaitTime); err != nil {
		return err
	} else if !released {
		return fmt.Errorf("codex writer preemption: pid %d still owns thread lock", owner.pid)
	}
	return nil
}

func inspectWriterLock(lockPath string) (procLock, error) {
	info, err := os.Stat(lockPath)
	if err != nil {
		return procLock{}, fmt.Errorf("codex writer preemption: stat lock %s: %w", lockPath, err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return procLock{}, fmt.Errorf("codex writer preemption: unsupported lock metadata for %s", lockPath)
	}
	data, err := os.ReadFile(procLocksPath)
	if err != nil {
		return procLock{}, fmt.Errorf("codex writer preemption: read %s: %w", procLocksPath, err)
	}
	locks := parseProcLocks(data)
	device := linuxLockDevice(stat.Dev)
	pid, ok := findProcLockOwner(locks, device, stat.Ino)
	if !ok {
		return procLock{}, fmt.Errorf("codex writer preemption: no active owner for %s", lockPath)
	}
	if countProcLocksForPID(locks, pid) != 1 {
		return procLock{}, fmt.Errorf("codex writer preemption: pid %d owns multiple thread locks", pid)
	}
	cmdline, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil {
		return procLock{}, fmt.Errorf("codex writer preemption: read pid %d command line: %w", pid, err)
	}
	if !isCodexProcessCmdline(cmdline) {
		return procLock{}, fmt.Errorf("codex writer preemption: pid %d is not a Codex process", pid)
	}
	return procLock{device: device, inode: stat.Ino, pid: pid}, nil
}

func validateWriterOwner(owner procLock) error {
	if owner.pid <= 0 || owner.device == "" || owner.inode == 0 {
		return fmt.Errorf("codex writer preemption: invalid writer owner")
	}
	return nil
}

func waitForWriterRelease(owner procLock, timeout time.Duration) (bool, error) {
	deadline := time.Now().Add(timeout)
	for {
		data, err := os.ReadFile(procLocksPath)
		if err != nil {
			return false, fmt.Errorf("codex writer preemption: read %s while waiting: %w", procLocksPath, err)
		}
		locks := parseProcLocks(data)
		pid, found := findProcLockOwner(locks, owner.device, owner.inode)
		if !found {
			return true, nil
		}
		if pid != owner.pid || countProcLocksForPID(locks, pid) != 1 {
			return false, fmt.Errorf("codex writer preemption: target lock ownership changed while waiting")
		}
		if time.Now().After(deadline) {
			return false, nil
		}
		time.Sleep(writerPollInterval)
	}
}

func linuxLockDevice(dev uint64) string {
	major := (dev >> 8) & 0xfff
	minor := (dev & 0xff) | ((dev >> 12) & 0xfff00)
	return fmt.Sprintf("%x:%x", major, minor)
}
