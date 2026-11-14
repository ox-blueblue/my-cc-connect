# Codex Active Writer Takeover Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 当 Codex app-server 恢复的线程被旧 Codex CLI 持有 writer 锁时，安全终止该目标 writer 并只重试一次恢复。

**Architecture:** 在 `thread/resume` 失败且错误明确表示 active writer 时，调用 Codex writer 抢占器。Linux 抢占器根据目标线程锁的设备/inode 从 `/proc/locks` 找到持有 PID，校验命令行是 Codex，并确认该 PID 只持有目标线程锁后才终止并等待锁释放；无法安全定位或进程共享其他线程锁时返回原错误。恢复路径通过内部可注入回调测试接管和重试，其他错误不触发接管。

**Tech Stack:** Go 1.25、标准库 `os`/`syscall`/`filepath`/`time`、现有 `golang.org/x/sys/unix` 的 Linux pidfd 支持、现有 Codex app-server JSON-RPC 测试夹具。

**Spec:** `docs/superpowers/specs/2026-11-13-codex-active-writer-takeover-design.md`

## Global Constraints

- 只处理 Codex app-server 的已有线程恢复。
- 不增加当前 Codex 协议不支持的 `takeover`、`force` 或 `preempt` RPC 参数。
- 不直接删除 writer 锁文件。
- 只有确认目标锁由 Codex 进程唯一持有时才终止；同一进程持有其他线程锁、无法确认持有者或发生竞态时保留原始错误。
- 抢占最多触发一次，避免无限重试。
- 遵循最小修订量原则，不改飞书命令、其他 agent、配置格式或公共接口。

## Review Focus

- active writer 错误：只终止目标线程的 Codex writer，然后重新发送一次 `thread/resume`。
- 非 active writer 的 RPC 错误：原样返回，不终止进程、不发送第二次恢复请求。
- 锁文件不存在、锁持有者无法解析或进程不是 Codex：原样返回，不删除文件、不误杀进程。
- 同一 PID 持有多个线程锁：原样返回，不终止该 PID，确保其他会话不受影响。
- 抢占前后锁归属发生变化：重新校验并放弃终止，确保不会误杀新 writer。
- 旧 writer 终止后未及时退出：等待有限时长后失败，不能无限阻塞恢复。
- 新线程启动和已有恢复成功路径：请求参数和权限校验保持现状。

### Task 1: Add failing resume takeover tests

**Files:**
- Modify: `agent/codex/appserver_session_test.go`
- Modify: `agent/codex/appserver_session.go` (test seam only after the failing test)

**Interfaces:**
- Produces an internal `appServerSession.preemptWriter func(threadID string) error` seam, defaulting to the platform helper, so the resume test can model process takeover without killing a real process.

- [x] **Step 1: Write the failing test**

  Add `TestAppServerSession_ResumeTakesOverActiveWriter`: the first `thread/resume` response returns code `-32600` and message containing `already has an active writer`; assert the injected preempt callback receives the requested thread ID, a second `thread/resume` is sent, and the successful response completes `ensureThread`.

- [x] **Step 2: Run the focused test and verify it fails**

  Run `go test ./agent/codex -run TestAppServerSession_ResumeTakesOverActiveWriter -count=1`.
  Expected: FAIL because current `ensureThread` returns the first RPC error and never calls the callback or retries.

- [x] **Step 3: Add the non-regression test**

  Add `TestAppServerSession_ResumeDoesNotTakeOverOtherError`: return a non-active-writer RPC error, assert the callback is not called and `ensureThread` returns that error after one request.

### Task 2: Implement safe writer-owner discovery and termination

**Files:**
- Create: `agent/codex/writer_preempt_linux.go`
- Create: `agent/codex/writer_preempt_other.go`
- Create: `agent/codex/writer_preempt_test.go`

**Interfaces:**
- Produces `preemptCodexThreadWriter(codexHome, threadID string) error` for app-server recovery.
- Produces Linux-only parsing helpers that map a lock file's device/inode to a PID from `/proc/locks` and validate the PID command line contains Codex.

- [x] **Step 1: Write failing helper tests**

  Add Linux parser tests using representative `/proc/locks` lines for matching device/inode, unrelated locks, malformed lines, a non-Codex command line, and one PID holding multiple thread locks. Keep OS-specific tests behind the Linux build tag; test the non-Linux helper returns an unsupported error without touching files.

- [x] **Step 2: Run helper tests and verify the intended failure**

  Run `go test ./agent/codex -run 'Test(ProcLock|PreemptCodexThreadWriter)' -count=1`.
  Expected: FAIL because the helper and parser are not implemented.

- [x] **Step 3: Implement Linux `preemptCodexThreadWriter`**

  Resolve the effective Codex home with the same explicit/env/work-directory precedence as the spawned Codex process, form `thread-writer-locks/<threadID>.lock`, stat it, read `/proc/locks`, and locate the exact device/inode owner. Enumerate every lock owned by that PID; continue only when the target lock is the sole lock. Read `/proc/<pid>/cmdline` and require the executable name to be exactly Codex before signaling. Record the process start time from `/proc/<pid>/stat`, open a Linux pidfd, freeze the process, and re-read lock ownership and identity while it cannot acquire another lock. Request `SIGTERM` and wait for bounded lock release; if it remains, freeze and revalidate identity/ownership before `SIGKILL`. Return contextual errors and never remove the lock file.

- [x] **Step 4: Implement non-Linux fallback**

  Define the same function on non-Linux platforms and return a typed unsupported error, preserving the original resume error at the caller.

- [x] **Step 5: Run helper tests and verify they pass**

  Run `go test ./agent/codex -run 'Test(ProcLock|PreemptCodexThreadWriter)' -count=1`.
  Expected: PASS with no process or lock-file side effects outside temporary test fixtures.

### Task 3: Wire one-shot takeover into thread resume

**Files:**
- Modify: `agent/codex/appserver_session.go`
- Modify: `agent/codex/appserver_session_test.go`

**Interfaces:**
- Consumes `preemptCodexThreadWriter(codexHome, threadID string) error`.
- Uses `appServerSession.preemptWriter`, defaulting to the helper in `newAppServerSession` and overrideable only by package tests.

- [x] **Step 1: Implement active-writer detection and retry**

  In `ensureThread`, detect only the active-writer message/code from the failed `thread/resume`; invoke `preemptWriter(resumeID)` once, then issue the same request again. Return the original error if preemption is unsupported or fails, and do not retry any other error.

- [x] **Step 2: Run the focused resume tests**

  Run `go test ./agent/codex -run 'TestAppServerSession_(Resume|.*Writer)' -count=1`.
  Expected: PASS for takeover and non-takeover cases.

- [x] **Step 3: Run package and repository verification**

  Run `go test ./agent/codex ./core` and then `go test ./...`.
  Expected: exit status 0 with no new failures. Run `gofmt` on changed Go files and `git diff --check` before any commit.

## Self-review

- The spec's active-writer, safety, bounded retry, compatibility, and verification requirements each map to Tasks 1–3.
- No production code is required before the failing tests in Task 1 and Task 2.
- The only new production seam is private to `appServerSession`; no public interface or configuration is added.
- The plan intentionally leaves deployment, branch merge, and Feishu behavior unchanged because this task only fixes Codex thread ownership during resume.
