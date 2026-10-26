package recording

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"bigo-live-recorder/internal/ffmpeg"
	"bigo-live-recorder/internal/state"
)

// OutputArgs deliberately accepts output options only. Input and destination
// paths are app-owned; no shell, executable, extra input, or extra output can be
// entered through this field. Filter scripts and file attachments are excluded.
func OutputArgs(raw string) ([]string, error) {
	var args []string
	var token strings.Builder
	var quote rune
	escape, started := false, false
	for _, r := range raw {
		if escape {
			token.WriteRune(r)
			escape = false
			started = true
			continue
		}
		if r == '\\' && quote != '\'' {
			escape = true
			continue
		}
		if quote != 0 {
			if r == quote {
				quote = 0
			} else {
				token.WriteRune(r)
			}
			started = true
			continue
		}
		if r == '\'' || r == '"' {
			quote = r
			started = true
			continue
		}
		if unicode.IsSpace(r) {
			if started {
				args = append(args, token.String())
				token.Reset()
				started = false
			}
			continue
		}
		token.WriteRune(r)
		started = true
	}
	if quote != 0 || escape {
		return nil, errors.New("FFmpeg output options contain an unfinished quote or escape")
	}
	if started {
		args = append(args, token.String())
	}
	flags := map[string]bool{"-an": true, "-vn": true, "-sn": true, "-dn": true, "-shortest": true, "-bitexact": true}
	values := map[string]bool{"-c": true, "-codec": true, "-c:v": true, "-c:a": true, "-c:s": true, "-codec:v": true, "-codec:a": true, "-map": true, "-map_metadata": true, "-map_chapters": true, "-b:v": true, "-b:a": true, "-crf": true, "-preset": true, "-tune": true, "-movflags": true, "-fflags": true, "-flvflags": true, "-vf": true, "-af": true, "-r": true, "-s": true, "-pix_fmt": true, "-profile:v": true, "-level:v": true, "-maxrate": true, "-bufsize": true, "-ar": true, "-ac": true, "-metadata": true, "-disposition:v": true, "-disposition:a": true, "-bsf:v": true, "-bsf:a": true, "-f": true, "-fps_mode": true, "-tag:v": true, "-tag:a": true, "-cluster_time_limit": true, "-cluster_size_limit": true, "-flush_packets": true}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if flags[arg] {
			continue
		}
		if !values[arg] {
			return nil, fmt.Errorf("%s is not a supported output option; input and output paths are managed by the app", arg)
		}
		i++
		if i >= len(args) || args[i] == "" {
			return nil, fmt.Errorf("%s needs a value", arg)
		}
		if strings.ContainsAny(args[i], "\r\n\x00") || strings.Contains(args[i], "{input}") || strings.Contains(args[i], "{output}") {
			return nil, errors.New("enter output options only, without paths or placeholders")
		}
		if arg == "-vf" || arg == "-af" { // Filters are expressions, not alternate file inputs.
			lower := strings.ToLower(args[i])
			if strings.Contains(lower, "movie=") || strings.Contains(lower, "filename=") || strings.Contains(lower, "sendcmd") || strings.Contains(lower, "file=") {
				return nil, errors.New("filters cannot read external files")
			}
		}
	}
	return args, nil
}

func Process(parent context.Context, source string, cfg state.Config) (string, error) {
	args, err := OutputArgs(cfg.FFmpegArgs)
	if err != nil {
		return "", err
	}
	path, err := ffmpeg.Find(cfg.FFmpegPath)
	if err != nil {
		return "", err
	}
	if cfg.OutputExtension != "mp4" && cfg.OutputExtension != "mkv" && cfg.OutputExtension != "ts" {
		return "", errors.New("invalid processed output format")
	}
	target := strings.TrimSuffix(source, filepath.Ext(source)) + ".processed." + cfg.OutputExtension
	ctx, cancel := context.WithTimeout(parent, 30*time.Minute)
	defer cancel()
	argv := []string{"-hide_banner", "-loglevel", "error", "-nostdin", "-n", "-i", source}
	argv = append(argv, args...)
	argv = append(argv, target)
	cmd := exec.CommandContext(ctx, path, argv...)
	configureCommand(cmd)
	var stderr tailBuffer
	cmd.Stderr = &stderr
	if err = cmd.Run(); err != nil {
		return "", errors.New("FFmpeg post-processing failed; the original MKV was kept. Check your output options and FFmpeg installation")
	}
	if info, err := os.Stat(target); err != nil || info.Size() == 0 {
		return "", errors.New("FFmpeg produced no processed copy; the original MKV was kept")
	}
	return target, nil
}
