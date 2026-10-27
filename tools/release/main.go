// Command release prepares local tags and packages draft-release assets.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

var tagPattern = regexp.MustCompile(`^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)

type config struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Out     string `json:"out"`
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "release:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) < 2 {
		return errors.New("usage: release prepare TAG | check TAG | notes TAG [PREVIOUS_TAG] | assets PLATFORM OUTPUT_DIR | checksums DIR")
	}
	root, err := os.Getwd()
	if err != nil {
		return err
	}
	c, err := readConfig(root)
	if err != nil {
		return err
	}
	switch {
	case args[0] == "prepare" && len(args) == 2:
		return prepare(root, c, args[1])
	case args[0] == "check" && len(args) == 2:
		version, err := versionOf(args[1])
		if err != nil {
			return err
		}
		if version != c.Version {
			return fmt.Errorf("tag %s does not match mygo.json version %s", args[1], c.Version)
		}
		return nil
	case args[0] == "notes" && (len(args) == 2 || len(args) == 3):
		previous := ""
		if len(args) == 3 {
			previous = args[2]
		}
		notes, err := releaseNotes(root, args[1], previous)
		if err == nil {
			_, err = fmt.Print(notes)
		}
		return err
	case args[0] == "assets" && len(args) == 3:
		return packageAssets(root, c, args[1], args[2])
	case args[0] == "checksums" && len(args) == 2:
		return checksums(args[1])
	default:
		return errors.New("unknown command or arguments")
	}
}

func readConfig(root string) (config, error) {
	b, err := os.ReadFile(filepath.Join(root, "mygo.json"))
	if err != nil {
		return config{}, err
	}
	var c config
	if err := json.Unmarshal(b, &c); err != nil {
		return c, err
	}
	if c.Name == "" || strings.ContainsAny(c.Name, `/\\`) || c.Out == "" {
		return c, errors.New("mygo.json must define a valid name and output directory")
	}
	_, err = versionOf("v" + c.Version)
	return c, err
}

func versionOf(tag string) (string, error) {
	if !tagPattern.MatchString(tag) {
		return "", errors.New("use a stable version tag such as v0.1.0")
	}
	return strings.TrimPrefix(tag, "v"), nil
}

func git(root string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %w\n%s", args[0], err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

func prepare(root string, c config, tag string) error {
	version, err := versionOf(tag)
	if err != nil {
		return err
	}
	status, err := git(root, "status", "--porcelain")
	if err != nil {
		return err
	}
	if status != "" {
		return errors.New("review and commit outstanding changes first, including an untracked README; no files were changed")
	}
	branch, err := git(root, "branch", "--show-current")
	if err != nil {
		return err
	}
	if branch != "master" {
		return errors.New("prepare releases from master")
	}
	if _, err := git(root, "rev-parse", "--verify", "refs/tags/"+tag); err == nil {
		return fmt.Errorf("tag %s already exists; existing release tags are never moved", tag)
	}
	for i, current := range strings.Split(c.Version, ".") {
		old, err := strconv.ParseUint(current, 10, 64)
		if err != nil {
			return err
		}
		next, err := strconv.ParseUint(strings.Split(version, ".")[i], 10, 64)
		if err != nil {
			return err
		}
		if next < old {
			return errors.New("release version cannot go backwards")
		}
		if next > old {
			break
		}
	}
	if version != c.Version {
		name := filepath.Join(root, "mygo.json")
		b, err := os.ReadFile(name)
		if err != nil {
			return err
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(b, &fields); err != nil {
			return err
		}
		fields["version"], _ = json.Marshal(version)
		b, err = json.MarshalIndent(fields, "", "  ")
		if err != nil {
			return err
		}
		if err := os.WriteFile(name, append(b, '\n'), 0644); err != nil {
			return err
		}
		if _, err := git(root, "add", "--", "mygo.json"); err != nil {
			return err
		}
		if _, err := git(root, "commit", "-m", "chore(release): "+tag, "-m", "Set the application version for the release."); err != nil {
			return err
		}
	}
	if _, err := git(root, "tag", "-a", tag, "-m", tag); err != nil {
		return err
	}
	fmt.Printf("Prepared %s locally. Start its draft-release workflow with:\n\n  git push --atomic origin master %s\n", tag, tag)
	return nil
}
