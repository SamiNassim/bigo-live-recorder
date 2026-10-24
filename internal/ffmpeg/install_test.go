package ffmpeg

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"debug/pe"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func zipFixture(t *testing.T, names ...string) []byte {
	t.Helper()
	var b bytes.Buffer
	z := zip.NewWriter(&b)
	for _, name := range names {
		w, err := z.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = io.WriteString(w, "fixture executable"); err != nil {
			t.Fatal(err)
		}
	}
	if err := z.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func TestVerifiedDownloadAndFailurePreservesExistingInstall(t *testing.T) {
	archive := zipFixture(t, "release/bin/ffmpeg", "release/README.txt")
	hash := sha256.Sum256(archive)
	for _, scenario := range []string{"success", "versioned checksum", "bad checksum", "bad executable", "canceled", "http error"} {
		t.Run(scenario, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, ".sha256") {
					if scenario == "bad checksum" {
						io.WriteString(w, strings.Repeat("0", 64))
						return
					}
					io.WriteString(w, hex.EncodeToString(hash[:])+"  ffmpeg.zip\n")
					return
				}
				if r.URL.Path == "/latest" {
					http.Redirect(w, r, "/v1/ffmpeg.zip", http.StatusTemporaryRedirect)
					return
				}
				if scenario == "http error" {
					w.WriteHeader(http.StatusServiceUnavailable)
					return
				}
				w.Write(archive)
			}))
			defer srv.Close()
			destination := filepath.Join(t.TempDir(), "ffmpeg")
			if err := os.WriteFile(destination, []byte("old installation"), 0700); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if scenario == "canceled" {
				cancel()
			}
			verified := 0
			verify := func(ctx context.Context, file string) error {
				verified++
				if scenario == "bad executable" {
					return errors.New("wrong architecture")
				}
				b, err := os.ReadFile(file)
				if err != nil || string(b) != "fixture executable" {
					t.Fatalf("staged file: %q, %v", b, err)
				}
				return ctx.Err()
			}
			r := release{URL: srv.URL + "/latest", ChecksumURL: srv.URL + "/checksum.sha256"}
			if scenario == "versioned checksum" {
				r.ChecksumURL = ""
			}
			_, err := install(ctx, srv.Client(), r, destination, nil, verify)
			b, readErr := os.ReadFile(destination)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if scenario == "success" || scenario == "versioned checksum" {
				if err != nil || string(b) != "fixture executable" || verified != 1 {
					t.Fatal(string(b), verified, err)
				}
			} else if err == nil || string(b) != "old installation" {
				t.Fatal("failed installation replaced existing executable", err)
			}
			if scenario == "bad checksum" && verified != 0 {
				t.Fatal("ran executable before verifying its checksum")
			}
			files, _ := os.ReadDir(filepath.Dir(destination))
			if len(files) != 1 {
				t.Fatal("installer left staging files behind", files)
			}
		})
	}
}

func TestRejectUnsafeAndAmbiguousArchives(t *testing.T) {
	for _, names := range [][]string{{"../ffmpeg"}, {"/ffmpeg"}, {"bin/../ffmpeg"}, {"bin\\ffmpeg"}, {"a/ffmpeg", "b/ffmpeg"}, {"ffprobe"}} {
		dir := t.TempDir()
		archive := filepath.Join(dir, "download.zip")
		if err := os.WriteFile(archive, zipFixture(t, names...), 0600); err != nil {
			t.Fatal(err)
		}
		if err := extractExecutable(archive, filepath.Join(dir, "ffmpeg")); err == nil {
			t.Fatal("accepted unsafe/incomplete archive", names)
		}
	}
}

func TestBuildProviders(t *testing.T) {
	for _, target := range [][2]string{{"darwin", "arm64"}, {"darwin", "amd64"}, {"linux", "amd64"}, {"linux", "arm64"}, {"windows", "amd64"}} {
		r, err := sourceFor(target[0], target[1])
		if err != nil || !strings.HasPrefix(r.URL, "https://") || r.Provider == "" {
			t.Fatal(target, r, err)
		}
	}
	if _, err := sourceFor("unsupported", "arch"); err == nil {
		t.Fatal("accepted unsupported platform")
	}
}

func TestChecksumManifestMatchesExactArchive(t *testing.T) {
	want := strings.Repeat("a", 64)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, strings.Repeat("b", 64)+"  other.zip\n"+want+"  selected.zip\n")
	}))
	defer srv.Close()
	if got, err := fetchChecksum(context.Background(), srv.Client(), srv.URL, "selected.zip"); err != nil || got != want {
		t.Fatal(got, err)
	}
	if _, err := fetchChecksum(context.Background(), srv.Client(), srv.URL, "missing.zip"); err == nil {
		t.Fatal("used a checksum for a different archive")
	}
}

// Opt-in download check: installs in a temporary directory, never the user's
// real installation. Ordinary tests are entirely offline.
func TestLiveFFmpegDownload(t *testing.T) {
	if os.Getenv("BIGO_FFMPEG_DOWNLOAD_TEST") != "1" {
		t.Skip("set BIGO_FFMPEG_DOWNLOAD_TEST=1 to test the build provider")
	}
	goos, arch := runtime.GOOS, runtime.GOARCH
	if target := os.Getenv("BIGO_FFMPEG_DOWNLOAD_TARGET"); target != "" {
		var ok bool
		goos, arch, ok = strings.Cut(target, "/")
		if !ok {
			t.Fatal("BIGO_FFMPEG_DOWNLOAD_TARGET must be os/arch")
		}
	}
	r, err := sourceFor(goos, arch)
	if err != nil {
		t.Fatal(err)
	}
	name := "ffmpeg"
	if goos == "windows" {
		name += ".exe"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	destination := filepath.Join(t.TempDir(), name)
	verify := probe
	if goos != runtime.GOOS || arch != runtime.GOARCH {
		if goos != "windows" || arch != "amd64" {
			t.Skip("only Windows amd64 has a cross-host format verifier")
		}
		verify = func(_ context.Context, path string) error {
			binary, err := pe.Open(path)
			if err != nil {
				return err
			}
			defer binary.Close()
			if binary.Machine != pe.IMAGE_FILE_MACHINE_AMD64 {
				return errors.New("incorrect Windows executable architecture")
			}
			return nil
		}
	}
	if _, err = install(ctx, &http.Client{Timeout: 3 * time.Minute}, r, destination, nil, verify); err != nil {
		t.Fatal(err)
	}
	t.Log("Provider download, SHA-256, extraction and executable validation succeeded for", goos, arch)
}
