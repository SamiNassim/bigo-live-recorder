//go:build darwin || linux

package recording

import (
	"os/exec"
	"syscall"
)

// The app owns graceful shutdown; a terminal interrupt must not simultaneously
// terminate the muxer before its stdin has drained.
func configureCommand(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}
