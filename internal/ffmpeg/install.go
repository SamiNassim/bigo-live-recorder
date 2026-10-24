package ffmpeg

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const maxDownload = 300 << 20
const maxExecutable = 300 << 20

// Progress reports download bytes; Total may be unknown (zero).
type Progress struct {
	Received, Total int64
	Stage           string
}

type release struct{ URL, ChecksumURL, ChecksumFile, Provider string }

func sourceFor(goos, arch string) (release, error) {
	if goos == "windows" && arch == "amd64" {
		base := "https://github.com/BtbN/FFmpeg-Builds/releases/download/latest/"
		name := "ffmpeg-n9.0-latest-win64-lgpl-9.0.zip"
		return release{URL: base + name, ChecksumURL: base + "checksums.sha256", ChecksumFile: name, Provider: "BtbN (GitHub)"}, nil
	}
	if (goos == "darwin" || goos == "linux") && (arch == "amd64" || arch == "arm64") {
		platform := goos
		if platform == "darwin" {
			platform = "macos"
		}
		return release{URL: "https://ffmpeg.martin-riedl.de/redirect/latest/" + platform + "/" + arch + "/release/ffmpeg.zip", Provider: "ffmpeg.martin-riedl.de"}, nil
	}
	return release{}, errors.New("automatic installation is unavailable for this platform; install FFmpeg using your package manager")
}

func DownloadProvider() (string, error) {
	r, err := sourceFor(runtime.GOOS, runtime.GOARCH)
	return r.Provider, err
}

// Install downloads a release directly from its build provider, verifies its
// SHA-256 checksum, and probes a staged executable before replacing the private
// installation. Cancellation or verification failures leave old installs alone.
func Install(ctx context.Context, progress func(Progress)) (string, error) {
	r, err := sourceFor(runtime.GOOS, runtime.GOARCH)
	if err != nil {
		return "", err
	}
	destination, err := ManagedPath()
	if err != nil {
		return "", err
	}
	client := &http.Client{Timeout: 10 * time.Minute, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if req.URL.Scheme != "https" || len(via) >= 10 {
			return errors.New("unsafe or excessive download redirects")
		}
		return nil
	}}
	return install(ctx, client, r, destination, progress, probe)
}

func install(ctx context.Context, client *http.Client, r release, destination string, progress func(Progress), verify func(context.Context, string) error) (string, error) {
	if progress == nil {
		progress = func(Progress) {}
	}
	dir := filepath.Dir(destination)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", err
	}
	staging, err := os.MkdirTemp(dir, ".ffmpeg-install-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(staging) // Only the unique installer-owned staging folder.
	progress(Progress{Stage: "Connecting to download provider…"})
	// The Windows release checksum is fetched first. Riedl's checksum is
	// addressed by the immutable, versioned URL after resolving its redirect.
	checksum := ""
	if r.ChecksumURL != "" {
		checksum, err = fetchChecksum(ctx, client, r.ChecksumURL, r.ChecksumFile)
		if err != nil {
			return "", err
		}
	}
	resp, err := get(ctx, client, r.URL)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.ContentLength > maxDownload {
		return "", errors.New("FFmpeg download is unexpectedly large")
	}
	if checksum == "" {
		checksum, err = fetchChecksum(ctx, client, resp.Request.URL.String()+".sha256", "")
		if err != nil {
			return "", err
		}
	}
	archive, err := os.Create(filepath.Join(staging, "download.zip"))
	if err != nil {
		return "", err
	}
	hash := sha256.New()
	reader := &progressReader{r: resp.Body, total: resp.ContentLength, report: progress}
	n, copyErr := io.Copy(io.MultiWriter(archive, hash), io.LimitReader(reader, maxDownload+1))
	closeErr := archive.Close()
	if err = errors.Join(copyErr, closeErr); err != nil {
		return "", fmt.Errorf("download FFmpeg: %w", err)
	}
	if n > maxDownload {
		return "", errors.New("FFmpeg download exceeded its size limit")
	}
	if hex.EncodeToString(hash.Sum(nil)) != checksum {
		return "", errors.New("FFmpeg checksum verification failed; no executable was installed. Please retry")
	}
	progress(Progress{Stage: "Verifying and installing FFmpeg…"})
	staged := filepath.Join(staging, filepath.Base(destination))
	if err = extractExecutable(archive.Name(), staged); err != nil {
		return "", err
	}
	if err = ctx.Err(); err != nil {
		return "", err
	}
	if err = verify(ctx, staged); err != nil {
		return "", err
	}
	if err = ctx.Err(); err != nil {
		return "", err
	}
	if err = os.Rename(staged, destination); err != nil {
		return "", fmt.Errorf("install FFmpeg: %w", err)
	}
	return destination, nil
}

func get(ctx context.Context, client *http.Client, address string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, errors.New("could not reach the FFmpeg download provider; check your connection and retry")
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("FFmpeg download provider returned HTTP %d; please retry later", resp.StatusCode)
	}
	return resp, nil
}

func fetchChecksum(parent context.Context, client *http.Client, address, filename string) (string, error) {
	ctx, cancel := context.WithTimeout(parent, 30*time.Second)
	defer cancel()
	resp, err := get(ctx, client, address)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	const limit = 64 << 10
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return "", err
	}
	if len(b) > limit {
		return "", errors.New("FFmpeg checksum manifest is too large")
	}
	var fields []string
	for _, line := range strings.Split(string(b), "\n") {
		v := strings.Fields(line)
		if len(v) > 0 && filename == "" || len(v) == 2 && strings.TrimPrefix(v[1], "*") == filename {
			fields = v
			break
		}
	}
	if len(fields) == 0 || len(fields[0]) != 64 {
		return "", errors.New("invalid FFmpeg checksum response")
	}
	if _, err = hex.DecodeString(fields[0]); err != nil {
		return "", errors.New("invalid FFmpeg SHA-256 checksum")
	}
	return strings.ToLower(fields[0]), nil
}

func extractExecutable(archive, destination string) error {
	z, err := zip.OpenReader(archive)
	if err != nil {
		return fmt.Errorf("open FFmpeg download: %w", err)
	}
	defer z.Close()
	var candidate *zip.File
	for _, file := range z.File {
		name := file.Name
		if strings.ContainsAny(name, "\\:") || path.IsAbs(name) || name == ".." || strings.HasPrefix(name, "../") || path.Clean(name) != strings.TrimSuffix(name, "/") || file.Mode()&os.ModeSymlink != 0 {
			return errors.New("unsafe path in FFmpeg download")
		}
		if path.Base(name) != filepath.Base(destination) || file.FileInfo().IsDir() {
			continue
		}
		if candidate != nil {
			return errors.New("ambiguous executables in FFmpeg download")
		}
		candidate = file
	}
	if candidate == nil || candidate.UncompressedSize64 == 0 || candidate.UncompressedSize64 > maxExecutable {
		return errors.New("FFmpeg executable is missing or unexpectedly large")
	}
	reader, err := candidate.Open()
	if err != nil {
		return err
	}
	defer reader.Close()
	f, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0700)
	if err != nil {
		return err
	}
	n, copyErr := io.Copy(f, io.LimitReader(reader, maxExecutable+1))
	err = errors.Join(copyErr, f.Sync(), f.Close())
	if err != nil {
		return err
	}
	if n > maxExecutable {
		return errors.New("FFmpeg executable exceeded its size limit")
	}
	return nil
}

type progressReader struct {
	r               io.Reader
	total, received int64
	last            time.Time
	report          func(Progress)
}

func (r *progressReader) Read(p []byte) (int, error) {
	n, err := r.r.Read(p)
	r.received += int64(n)
	if time.Since(r.last) >= 150*time.Millisecond || err != nil {
		r.report(Progress{Received: r.received, Total: max(0, r.total), Stage: "Downloading FFmpeg…"})
		r.last = time.Now()
	}
	return n, err
}
