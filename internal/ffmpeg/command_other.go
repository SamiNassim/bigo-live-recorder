//go:build !windows

package ffmpeg

import "os/exec"

func configureCommand(*exec.Cmd) {}
