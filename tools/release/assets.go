package main

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

func packageAssets(root string, c config, platform, output string) error {
	if err := os.MkdirAll(output, 0755); err != nil {
		return err
	}
	goos, arch, ok := strings.Cut(platform, "/")
	if !ok || (arch != "amd64" && arch != "arm64" && arch != "universal") {
		return errors.New("unsupported release platform")
	}
	dir := filepath.Join(root, c.Out, goos+"-"+arch)
	slug := strings.ToLower(strings.Join(strings.Fields(c.Name), "-"))
	prefix := slug + "-" + c.Version
	copyAsset := func(name, target string) error {
		return copyFile(filepath.Join(dir, name), filepath.Join(output, prefix+"-"+target))
	}
	switch goos {
	case "darwin":
		name := c.Name + " " + c.Version
		if arch != "universal" {
			name += " " + arch
		}
		return copyAsset(name+".dmg", "macos-"+arch+".dmg")
	case "windows":
		if arch == "universal" {
			return errors.New("Windows requires a concrete processor architecture")
		}
		return copyAsset(c.Name+".exe", "windows-"+arch+".exe")
	case "linux":
		if arch == "universal" {
			return errors.New("Linux requires a concrete processor architecture")
		}
		if err := copyAsset(slug+"_"+c.Version+"_"+arch+".deb", "linux-"+arch+".deb"); err != nil {
			return err
		}
		return copyAsset(prefix+"-linux-"+arch+".tar.gz", "linux-"+arch+".tar.gz")
	default:
		return errors.New("unsupported release operating system")
	}
}

func copyFile(source, target string) error {
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	info, err := in.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() == 0 {
		return errors.New("release assets must be nonempty regular files")
	}
	out, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	_, err = io.Copy(out, in)
	return errors.Join(err, out.Close())
}

func checksums(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	var lines []string
	for _, entry := range entries {
		if entry.IsDir() || entry.Name() == "SHA256SUMS.txt" {
			continue
		}
		f, err := os.Open(filepath.Join(dir, entry.Name()))
		if err != nil {
			return err
		}
		h := sha256.New()
		_, err = io.Copy(h, f)
		f.Close()
		if err != nil {
			return err
		}
		lines = append(lines, fmt.Sprintf("%x  %s\n", h.Sum(nil), entry.Name()))
	}
	if len(lines) == 0 {
		return errors.New("no release assets were produced")
	}
	sort.Strings(lines)
	return os.WriteFile(filepath.Join(dir, "SHA256SUMS.txt"), []byte(strings.Join(lines, "")), 0644)
}
