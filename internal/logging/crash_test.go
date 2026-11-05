package logging

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestCrashOutputHelper(t *testing.T) {
	if os.Getenv("BIGO_CRASH_TEST_HELPER") != "1" {
		return
	}
	previous, err := CaptureCrashes(os.Getenv("BIGO_CRASH_TEST_DIR"))
	if err != nil {
		t.Fatal(err)
	}
	if previous != "" {
		fmt.Fprintln(os.Stdout, filepath.Base(previous))
	}
	if message := os.Getenv("BIGO_CRASH_TEST_MESSAGE"); message != "" {
		go func() { panic(message) }()
		select {}
	}
}

func runCrashHelper(t *testing.T, dir, message string) string {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(executable, "-test.run=^TestCrashOutputHelper$")
	cmd.Env = append(os.Environ(), "BIGO_CRASH_TEST_HELPER=1", "BIGO_CRASH_TEST_DIR="+dir, "BIGO_CRASH_TEST_MESSAGE="+message, "GOTRACEBACK=single")
	output, err := cmd.CombinedOutput()
	if message == "" {
		if err != nil {
			t.Fatalf("clean helper failed: %v: %s", err, output)
		}
	} else {
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 2 || !strings.Contains(string(output), "panic: "+message) {
			t.Fatalf("panic was hidden or changed: %v: %s", err, output)
		}
	}
	return string(output)
}

func TestCrashReportsCaptureBackgroundPanicsAndKeepBoundedHistory(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, CrashLogName)
	assertReport := func(path, message string) {
		t.Helper()
		data, err := os.ReadFile(path)
		if err != nil || !strings.Contains(string(data), "panic: "+message) || !strings.Contains(string(data), "goroutine ") {
			t.Fatalf("missing runtime panic trace: %v: %s", err, data)
		}
		if info, err := os.Stat(path); err != nil || runtime.GOOS != "windows" && info.Mode().Perm() != 0600 {
			t.Fatal("crash report permissions", info, err)
		}
	}
	runCrashHelper(t, dir, "synthetic first failure")
	assertReport(path, "synthetic first failure")
	if output := runCrashHelper(t, dir, ""); !strings.Contains(output, CrashLogName+".1") {
		t.Fatal("the previous report was not announced", output)
	}
	assertReport(path+".1", "synthetic first failure")
	if info, err := os.Stat(path); err != nil || info.Size() != 0 {
		t.Fatal("a normal start wrote a false crash report", info, err)
	}
	// Healthy restarts must not evict the last actual crash from the archive.
	if output := runCrashHelper(t, dir, ""); strings.Contains(output, CrashLogName+".1") {
		t.Fatal("a normal exit was reported as a new crash", output)
	}
	assertReport(path+".1", "synthetic first failure")
	for _, message := range []string{"synthetic second failure", "synthetic third failure", "synthetic fourth failure"} {
		runCrashHelper(t, dir, message)
	}
	assertReport(path, "synthetic fourth failure")
	assertReport(path+".1", "synthetic third failure")
	assertReport(path+".2", "synthetic second failure")
	if files, err := filepath.Glob(path + "*"); err != nil || len(files) != 3 {
		t.Fatal("crash reports were not rotated", files, err)
	}
}

func TestCrashReportsRejectNonregularPathsAndPreserveReportOnRotationFailure(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, CrashLogName)
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := CaptureCrashes(dir); err == nil {
		t.Fatal("accepted a nonregular crash report")
	}
	dir = t.TempDir()
	path = filepath.Join(dir, CrashLogName)
	if err := os.WriteFile(path, []byte("saved diagnostic"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path+".2", 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path+".2", "keep"), []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := CaptureCrashes(dir); err == nil {
		t.Fatal("ignored an archive rotation error")
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "saved diagnostic" {
		t.Fatal("lost the previous report after rotation failed", string(data), err)
	}
}
