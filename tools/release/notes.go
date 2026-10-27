package main

import (
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
)

var conventional = regexp.MustCompile(`^([a-z]+)(?:\([^)]*\))?!?:\s*(.+)$`)

type change struct{ hash, subject, body string }

func releaseNotes(root, tag, previous string) (string, error) {
	if _, err := versionOf(tag); err != nil {
		return "", err
	}
	ref := "refs/tags/" + tag
	if _, err := git(root, "rev-parse", "--verify", ref+"^{commit}"); err != nil {
		return "", err
	}
	if previous != "" {
		if _, err := versionOf(previous); err != nil {
			return "", err
		}
		before := "refs/tags/" + previous
		if _, err := git(root, "merge-base", "--is-ancestor", before, ref); err != nil {
			return "", fmt.Errorf("previous release must be an ancestor of the new tag: %w", err)
		}
		ref = before + ".." + ref
	}
	cmd := exec.Command("git", "log", "--reverse", "--no-merges", "--format=%H%x00%s%x00%b%x00", ref)
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	fields := strings.Split(string(out), "\x00")
	var changes []change
	for i := 0; i+2 < len(fields); i += 3 {
		hash := strings.TrimSpace(fields[i])
		if len(hash) >= 7 {
			changes = append(changes, change{hash, fields[i+1], fields[i+2]})
		}
	}
	repo := strings.TrimRight(os.Getenv("GITHUB_SERVER_URL"), "/") + "/" + os.Getenv("GITHUB_REPOSITORY")
	if os.Getenv("GITHUB_REPOSITORY") == "" {
		repo = ""
	}
	return formatNotes(changes, tag, previous, repo), nil
}

func formatNotes(changes []change, tag, previous, repo string) string {
	groups := map[string][]change{}
	for _, c := range changes {
		if strings.HasPrefix(c.subject, "chore(release):") {
			continue
		}
		group := "Other changes"
		if match := conventional.FindStringSubmatch(c.subject); match != nil {
			switch match[1] {
			case "feat":
				group = "Features"
			case "fix":
				group = "Fixes"
			case "perf":
				group = "Improvements"
			}
			c.subject = match[2]
		}
		groups[group] = append(groups[group], c)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n\n", tag)
	if repo != "" {
		fmt.Fprintf(&b, "These builds do not use Apple or Windows publisher certificates. See the [first-launch instructions](%s/blob/%s/README.md#download-and-open).\n\n", repo, tag)
	}
	for _, group := range []string{"Features", "Fixes", "Improvements", "Other changes"} {
		if len(groups[group]) == 0 {
			continue
		}
		fmt.Fprintf(&b, "## %s\n\n", group)
		for _, c := range groups[group] {
			ref := c.hash[:7]
			if repo != "" {
				ref = fmt.Sprintf("[%s](%s/commit/%s)", ref, repo, c.hash)
			}
			fmt.Fprintf(&b, "- %s (%s)\n", c.subject, ref)
			if body := strings.TrimSpace(c.body); body != "" {
				b.WriteByte('\n')
				for _, line := range strings.Split(body, "\n") {
					fmt.Fprintf(&b, "  %s\n", line)
				}
			}
			b.WriteByte('\n')
		}
	}
	if repo != "" && previous != "" {
		fmt.Fprintf(&b, "[Full changelog](%s/compare/%s...%s)\n", repo, previous, tag)
	}
	return b.String()
}
