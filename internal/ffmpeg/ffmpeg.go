// Package ffmpeg discovers and installs the app's only runtime dependency.
// Installs are per-user and do not modify PATH or require elevated privileges.
package ffmpeg

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

func ManagedPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	name := "ffmpeg"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	return filepath.Join(dir, "BIGO Live Recorder", "bin", name), nil
}

// Find uses an explicit selection when present. Automatic discovery prefers
// PATH, then the private installation and common portable/package-manager paths.
func Find(configured string) (string, error) {
	candidates := []string{configured}
	if configured == "" {
		managed, _ := ManagedPath()
		candidates = []string{"ffmpeg", managed}
		if runtime.GOOS == "darwin" {
			candidates = append(candidates, "/opt/homebrew/bin/ffmpeg", "/usr/local/bin/ffmpeg")
		}
		if executable, err := os.Executable(); err == nil {
			candidates = append(candidates, filepath.Join(filepath.Dir(executable), "ffmpeg"), filepath.Join(filepath.Dir(executable), "ffmpeg.exe"))
		}
	}
	for _, candidate := range candidates {
		if candidate == "" {
			continue
		}
		if path, err := exec.LookPath(candidate); err == nil {
			return path, nil
		}
	}
	return "", errors.New("FFmpeg was not found; install it from Settings")
}

// Check verifies that a discovered executable really starts, not just that a
// file with the right name exists. Run it off the UI thread.
func Check(ctx context.Context, configured string) (string, error) {
	path, err := Find(configured)
	if err != nil {
		return "", err
	}
	if err = probe(ctx, path); err != nil {
		return "", err
	}
	return path, nil
}

func probe(parent context.Context, path string) error {
	ctx, cancel := context.WithTimeout(parent, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, "-version")
	configureCommand(cmd)
	out, err := cmd.Output()
	if err != nil || !strings.HasPrefix(string(out), "ffmpeg version ") {
		return errors.New("FFmpeg could not start; choose a compatible executable or install it from Settings")
	}
	return nil
}
