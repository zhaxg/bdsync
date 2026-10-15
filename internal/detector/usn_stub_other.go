//go:build !windows

package detector

import (
	"context"
	"errors"

	"bdsync/internal/index"
)

// RunUsnSelftest -- 非 Windows 无 usn 后端（design §4.10：Linux 默认 poll）。
func RunUsnSelftest(context.Context, string, *index.Store) error {
	return errors.New("usn 后端仅 Windows；Linux 用 poll（selftest --backend poll）")
}

// RunUsnCycleStub -- 非 Windows 占位（永远回退 poll）。
func RunUsnCycle(context.Context, string, *index.Store, []string) (*ScanResult, bool, error) {
	return &ScanResult{Backend: "usn"}, true, errors.New("usn 后端仅 Windows")
}
