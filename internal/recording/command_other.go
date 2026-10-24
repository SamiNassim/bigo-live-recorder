//go:build !windows && !darwin && !linux

package recording

import "os/exec"

func configureCommand(cmd *exec.Cmd) {}
