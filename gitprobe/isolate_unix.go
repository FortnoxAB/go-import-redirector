//go:build unix

package gitprobe

import (
	"os/exec"
	"syscall"
)

// isolate runs cmd in a session of its own. Without a controlling terminal,
// ssh can't prompt for a host key or passphrase and fails instead; and when
// the probe times out the whole process group is killed, not just git, so an
// ssh child stuck in connect can't keep the probe (and its slot) waiting.
func isolate(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Cancel = func() error {
		// Setsid makes git the group leader, so its pid is the group id.
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
