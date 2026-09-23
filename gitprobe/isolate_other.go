//go:build !unix

package gitprobe

import "os/exec"

// isolate is a no-op where process sessions aren't available; the probe then
// relies on cmd.WaitDelay alone to stop waiting on a timed-out git.
func isolate(cmd *exec.Cmd) {}
