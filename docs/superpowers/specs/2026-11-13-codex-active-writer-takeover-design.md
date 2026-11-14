# Codex Active Writer Takeover

## Intent

切换飞书中的 Codex agent 时，如果目标线程仍被本地 Codex CLI 持有，`thread/resume` 会返回 `already has an active writer (code -32600)`。恢复流程应安全地抢占该线程后再试一次。

## Scope and constraints

- 只处理 Codex app-server 的已有线程恢复。
- 不增加未被当前 Codex 协议支持的 `takeover` 或 `force` 参数。
- 不直接删除 writer 锁文件，避免留下活跃进程继续写入或制造双 writer。
- 仅在系统能确认目标锁由 Codex 进程持有，且该进程没有持有其他线程锁时终止该进程；无法确认或进程共享多个线程锁时保留原始错误。
- 新线程、非 Codex 后端和非 active-writer 错误保持现有行为。

## Approach

采用“按目标锁定位并终止旧 Codex writer，再重试一次 `thread/resume`”。Linux 通过目标锁的设备/inode 在 `/proc/locks` 中查找持有者，并校验进程命令行和实际可执行文件是 Codex；同时枚举该 PID 持有的所有锁，只有目标锁是唯一锁时才允许继续。记录 `/proc/<pid>/stat` 的进程启动时间并使用稳定的 Linux pidfd，先冻结进程，再重新确认锁归属和进程身份，避免 PID 复用或进程取得其他会话锁。随后发送 `SIGTERM` 并在有限时间内等待锁释放；仍未释放时再次冻结并复核后才发送 `SIGKILL`。接管动作只重试一次，避免无限重试。其他平台返回未支持接管，由现有恢复错误处理负责。

## Verification

新增回归测试覆盖 active-writer 错误的接管重试、普通恢复错误不触发接管，以及共享多个线程锁的进程不会被终止。运行 Codex agent 单元测试、core 测试和完整 `go test ./...`。
