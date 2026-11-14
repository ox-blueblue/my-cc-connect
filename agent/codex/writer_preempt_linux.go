//go:build linux

package codex

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const (
	procLocksPath            = "/proc/locks"
	writerGracePeriod        = 750 * time.Millisecond
	writerPollInterval       = 20 * time.Millisecond
	writerForcedKillWaitTime = 750 * time.Millisecond
)

var preemptManagedDaemonThread = preemptCodexThreadViaDaemon
var managedCodexDaemonDetector = isManagedCodexDaemon

type procLock struct {
	device string
	inode  uint64
	pid    int
	start  uint64
	count  int
}

func parseProcLocks(data []byte) []procLock {
	var locks []procLock
	scanner := bufio.NewScanner(bytes.NewReader(data))
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
		deviceParts := strings.Split(fields[5], ":")
		if len(deviceParts) != 3 {
			continue
		}
		inode, err := strconv.ParseUint(deviceParts[2], 10, 64)
		if err != nil {
			continue
		}
		locks = append(locks, procLock{
			device: deviceParts[0] + ":" + deviceParts[1],
			inode:  inode,
			pid:    pid,
		})
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
	return isCodexExecutablePath(args[0])
}

func isCodexExecutablePath(path string) bool {
	name := strings.ToLower(filepath.Base(strings.TrimSpace(path)))
	return name == "codex" || name == "codex.exe"
}

func isCodexProcess(pid int, cmdline []byte) bool {
	if !isCodexProcessCmdline(cmdline) {
		return false
	}
	executable, err := os.Readlink(fmt.Sprintf("/proc/%d/exe", pid))
	return err == nil && isCodexExecutablePath(executable)
}

func isManagedCodexDaemon(pid int, cmdline []byte) bool {
	if !isCodexProcess(pid, cmdline) {
		return false
	}
	for _, arg := range strings.Split(string(cmdline), "\x00") {
		if arg == "--managed-daemon" {
			return true
		}
	}
	return false
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
	if owner.count != 1 {
		cmdline, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", owner.pid))
		if err != nil {
			return fmt.Errorf("codex writer preemption: read pid %d command line: %w", owner.pid, err)
		}
		if !managedCodexDaemonDetector(owner.pid, cmdline) {
			return fmt.Errorf("codex writer preemption: pid %d owns multiple thread locks", owner.pid)
		}
		confirmed, err := inspectWriterLock(lockPath)
		if err != nil {
			return err
		}
		if confirmed != owner {
			return fmt.Errorf("codex writer preemption: target writer changed before daemon takeover")
		}
		return preemptManagedDaemonThread(codexHome, threadID)
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

	pidfd, err := unix.PidfdOpen(owner.pid, 0)
	if err != nil {
		return fmt.Errorf("codex writer preemption: open stable pid handle %d: %w", owner.pid, err)
	}
	defer unix.Close(pidfd)
	start, err := readProcessStartTime(owner.pid)
	if err != nil {
		return fmt.Errorf("codex writer preemption: read pid %d identity: %w", owner.pid, err)
	}
	if start != owner.start {
		return fmt.Errorf("codex writer preemption: target writer changed before freeze")
	}
	if err := unix.PidfdSendSignal(pidfd, unix.SIGSTOP, nil, 0); err != nil && !errors.Is(err, unix.ESRCH) {
		return fmt.Errorf("codex writer preemption: freeze pid %d: %w", owner.pid, err)
	}

	// Keep the verified process stopped while checking ownership one final time.
	// This prevents it from acquiring another thread lock between validation and
	// termination, which would make a process-level kill affect another session.
	confirmed, err = inspectWriterLock(lockPath)
	if err != nil {
		_ = unix.PidfdSendSignal(pidfd, unix.SIGCONT, nil, 0)
		return err
	}
	if confirmed != owner {
		_ = unix.PidfdSendSignal(pidfd, unix.SIGCONT, nil, 0)
		return fmt.Errorf("codex writer preemption: target writer changed before force termination")
	}
	if err := unix.PidfdSendSignal(pidfd, unix.SIGTERM, nil, 0); err != nil && !errors.Is(err, unix.ESRCH) {
		_ = unix.PidfdSendSignal(pidfd, unix.SIGCONT, nil, 0)
		return fmt.Errorf("codex writer preemption: request graceful stop for pid %d: %w", owner.pid, err)
	}
	if err := unix.PidfdSendSignal(pidfd, unix.SIGCONT, nil, 0); err != nil && !errors.Is(err, unix.ESRCH) {
		return fmt.Errorf("codex writer preemption: resume pid %d for graceful stop: %w", owner.pid, err)
	}
	if released, err := waitForWriterRelease(owner, writerGracePeriod); err != nil {
		return err
	} else if released {
		return nil
	}
	if err := unix.PidfdSendSignal(pidfd, unix.SIGSTOP, nil, 0); err != nil && !errors.Is(err, unix.ESRCH) {
		return fmt.Errorf("codex writer preemption: refreeze pid %d: %w", owner.pid, err)
	}
	// Re-check process identity and lock ownership after the graceful-stop
	// window, while the process is stopped again.
	confirmed, err = inspectWriterLock(lockPath)
	if err != nil {
		_ = unix.PidfdSendSignal(pidfd, unix.SIGCONT, nil, 0)
		return err
	}
	if confirmed != owner {
		_ = unix.PidfdSendSignal(pidfd, unix.SIGCONT, nil, 0)
		return fmt.Errorf("codex writer preemption: target writer changed before force termination")
	}
	if err := unix.PidfdSendSignal(pidfd, unix.SIGKILL, nil, 0); err != nil && !errors.Is(err, unix.ESRCH) {
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
	lockCount := countProcLocksForPID(locks, pid)
	cmdline, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil {
		return procLock{}, fmt.Errorf("codex writer preemption: read pid %d command line: %w", pid, err)
	}
	if !isCodexProcess(pid, cmdline) {
		return procLock{}, fmt.Errorf("codex writer preemption: pid %d is not a Codex process", pid)
	}
	start, err := readProcessStartTime(pid)
	if err != nil {
		return procLock{}, fmt.Errorf("codex writer preemption: read pid %d identity: %w", pid, err)
	}
	return procLock{device: device, inode: stat.Ino, pid: pid, start: start, count: lockCount}, nil
}

func readProcessStartTime(pid int) (uint64, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0, err
	}
	end := strings.LastIndexByte(string(data), ')')
	if end < 0 {
		return 0, fmt.Errorf("malformed process stat")
	}
	fields := strings.Fields(string(data)[end+1:])
	if len(fields) <= 19 {
		return 0, fmt.Errorf("malformed process stat")
	}
	start, err := strconv.ParseUint(fields[19], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse process start time: %w", err)
	}
	return start, nil
}

func validateWriterOwner(owner procLock) error {
	if owner.pid <= 0 || owner.device == "" || owner.inode == 0 || owner.start == 0 {
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
		start, err := readProcessStartTime(pid)
		if err != nil {
			return false, fmt.Errorf("codex writer preemption: read pid %d identity while waiting: %w", pid, err)
		}
		if start != owner.start {
			return false, fmt.Errorf("codex writer preemption: target process changed while waiting")
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
	return fmt.Sprintf("%02x:%02x", major, minor)
}
