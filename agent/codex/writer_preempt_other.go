//go:build !linux

package codex

import (
	"fmt"
	"runtime"
)

func preemptCodexThreadWriter(_, _ string) error {
	return fmt.Errorf("codex writer preemption is unsupported on %s", runtime.GOOS)
}
