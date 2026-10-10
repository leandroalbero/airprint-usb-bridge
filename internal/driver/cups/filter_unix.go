//go:build unix

package cups

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

// ownProcessGroup puts the filter in a process group of its own and kills the whole group on
// timeout or cancel: wrapper filters (shell scripts, foomatic-rip) start gs and friends, which
// would otherwise keep running after the filter itself is killed.
func ownProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); !errors.Is(err, syscall.ESRCH) {
			return err
		}
		return os.ErrProcessDone
	}
}
