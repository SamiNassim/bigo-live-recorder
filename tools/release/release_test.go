package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func testRepository(t *testing.T) (string, config) {
	t.Helper()
	root := t.TempDir()
	for _, args := range [][]string{{"init", "-b", "master"}, {"config", "user.name", "Release Test"}, {"config", "user.email", "release@example.invalid"}} {
		if _, err := git(root, args...); err != nil {
			t.Fatal(err)
		}
	}
	c := config{Name: "Demo Recorder", Version: "0.1.0", Out: "build"}
	b, _ := json.Marshal(c)
	if err := os.WriteFile(filepath.Join(root, "mygo.json"), b, 0644); err != nil {
		t.Fatal(err)
	}
	commitFile(t, root, "README.md", "Demo", "feat: first feature", "A description for the first release.")
	return root, c
}

func commitFile(t *testing.T, root, name, content, subject, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := git(root, "add", "."); err != nil {
		t.Fatal(err)
	}
	if _, err := git(root, "commit", "-m", subject, "-m", body); err != nil {
		t.Fatal(err)
	}
}

func TestPreparationCreatesImmutableLocalTagsAndUpdatesVersion(t *testing.T) {
	root, c := testRepository(t)
	if err := prepare(root, c, "v0.1.0"); err != nil {
		t.Fatal(err)
	}
	before, _ := git(root, "rev-parse", "refs/tags/v0.1.0")
	if err := prepare(root, c, "v0.1.0"); err == nil {
		t.Fatal("existing release tag was overwritten")
	}
	after, _ := git(root, "rev-parse", "refs/tags/v0.1.0")
	if before != after {
		t.Fatal("release tag moved")
	}
	if err := prepare(root, c, "v0.2.0"); err != nil {
		t.Fatal(err)
	}
	updated, err := readConfig(root)
	if err != nil || updated.Version != "0.2.0" {
		t.Fatal("release version was not updated", updated, err)
	}
	if status, err := git(root, "status", "--porcelain"); err != nil || status != "" {
		t.Fatal("release preparation left an uncommitted version", status, err)
	}
	if err := prepare(root, updated, "v0.1.1"); err == nil {
		t.Fatal("release version went backwards")
	}
}

func TestPreparationRejectsDirtyTreesAndInvalidTags(t *testing.T) {
	root, c := testRepository(t)
	for _, tag := range []string{"0.1.0", "v01.1.0", "v0.1", "v0.1.0;command", "--help"} {
		if err := prepare(root, c, tag); err == nil {
			t.Fatal("invalid release tag was accepted", tag)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "draft.txt"), []byte("unreviewed"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := prepare(root, c, "v0.1.1"); err == nil {
		t.Fatal("release preparation committed unreviewed files")
	}
	unchanged, _ := readConfig(root)
	if unchanged.Version != c.Version {
		t.Fatal("rejected preparation changed the version")
	}
}

func TestNotesUseDirectCommitsAndBodiesSincePreviousRelease(t *testing.T) {
	root, _ := testRepository(t)
	if _, err := git(root, "tag", "v0.1.0"); err != nil {
		t.Fatal(err)
	}
	commitFile(t, root, "feature.txt", "feature", "feat(recording): another feature", "Its useful description.\n\nAn additional paragraph.")
	commitFile(t, root, "fix.txt", "fix", "fix: recover interrupted streams", "Resume recording after the connection returns.")
	commitFile(t, root, "version.txt", "version", "chore(release): v0.1.1", "Set the application version.")
	if _, err := git(root, "tag", "v0.1.1"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GITHUB_SERVER_URL", "https://github.com")
	t.Setenv("GITHUB_REPOSITORY", "example/demo-recorder")
	notes, err := releaseNotes(root, "v0.1.1", "v0.1.0")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"## Features", "## Fixes", "another feature", "Its useful description.", "An additional paragraph.", "Resume recording", "/compare/v0.1.0...v0.1.1"} {
		if !strings.Contains(notes, want) {
			t.Fatal("direct commit or description missing from notes", want)
		}
	}
	for _, absent := range []string{"first feature", "chore(release)", "Release Test", "release@example.invalid"} {
		if strings.Contains(notes, absent) {
			t.Fatal("notes included an old commit, release bookkeeping, or author metadata")
		}
	}
	first, err := releaseNotes(root, "v0.1.0", "")
	if err != nil || !strings.Contains(first, "first feature") {
		t.Fatal("first release could not generate notes", err)
	}
	if strings.HasPrefix(first, "# ") || strings.HasPrefix(notes, "# ") {
		t.Fatal("release notes repeat GitHub's release title")
	}
}

func TestWindowsReleaseContainsOnlyThePortableExecutable(t *testing.T) {
	root, c := testRepository(t)
	dir := filepath.Join(root, c.Out, "windows-amd64")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{c.Name + ".exe", c.Name + " Setup 0.1.0 amd64.exe", "CREDITS.md", "LICENSE", "config.json", "data.json", "private.log"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("synthetic fixture"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	out := t.TempDir()
	if err := packageAssets(root, c, "windows/amd64", out); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(out)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	if !slices.Equal(names, []string{"demo-recorder-0.1.0-windows-amd64.exe"}) {
		t.Fatal("Windows release included an installer, archive, or unrelated file", names)
	}
	if b, err := os.ReadFile(filepath.Join(out, names[0])); err != nil || string(b) != "synthetic fixture" {
		t.Fatal("portable executable was not copied correctly", err)
	}
	if err := checksums(out); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(out, "SHA256SUMS.txt"))
	if err != nil || len(strings.Split(strings.TrimSpace(string(b)), "\n")) != 1 || strings.Contains(string(b), "SHA256SUMS.txt") {
		t.Fatal("checksum list is incomplete or hashes itself", err)
	}
}
