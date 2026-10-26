package state

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLegacyConversionSettingsMigrateWithoutDeletingOriginals(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		for _, deletion := range []bool{false, true} {
			dir := t.TempDir()
			cfg := Defaults()
			cfg.Cookie = "synthetic-cookie"
			cfg.Window.Width = 1200
			raw, _ := json.Marshal(cfg)
			var fields map[string]any
			json.Unmarshal(raw, &fields)
			for _, key := range []string{"post_process", "ffmpeg_args", "output_extension"} {
				delete(fields, key)
			}
			fields["convert_to_mp4"], fields["delete_after_conversion"] = enabled, deletion
			raw, _ = json.Marshal(fields)
			name := filepath.Join(dir, "config.json")
			if err := os.WriteFile(name, raw, 0600); err != nil {
				t.Fatal(err)
			}
			store, err := Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			got := store.Config()
			if got.PostProcess != enabled || got.Cookie != cfg.Cookie || got.Window != cfg.Window || got.NativeDeviceID != cfg.NativeDeviceID {
				t.Fatal("migration changed account, identity, or preferences")
			}
			want := "mkv"
			if enabled {
				want = "mp4"
			}
			if got.OutputExtension != want || !strings.Contains(got.FFmpegArgs, "-c copy") || enabled && !strings.Contains(got.FFmpegArgs, "+faststart") {
				t.Fatal("conversion preference was not migrated", got.OutputExtension, got.FFmpegArgs)
			}
			data, err := os.ReadFile(name)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(data), "convert_to_mp4") || strings.Contains(string(data), "delete_after_conversion") {
				t.Fatal("obsolete conversion or deletion setting was retained")
			}
			reopened, err := Open(dir)
			if err != nil || reopened.Config() != got {
				t.Fatal("migration was not stable", err)
			}
		}
	}
}

func TestExplicitProcessingSettingsOverrideLegacyFields(t *testing.T) {
	dir := t.TempDir()
	cfg := Defaults()
	cfg.OutputExtension = "ts"
	cfg.FFmpegArgs = "-c copy"
	raw, _ := json.Marshal(cfg)
	var fields map[string]any
	json.Unmarshal(raw, &fields)
	fields["convert_to_mp4"], fields["delete_after_conversion"] = true, true
	raw, _ = json.Marshal(fields)
	if err := os.WriteFile(filepath.Join(dir, "config.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	store, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := store.Config(); got.PostProcess || got.OutputExtension != "ts" || got.FFmpegArgs != "-c copy" {
		t.Fatal("legacy fields overrode new preferences")
	}
}

func TestProcessingFormatMustBeSupported(t *testing.T) {
	for _, format := range []string{"mkv", "mp4", "ts", "avi", ""} {
		cfg := Defaults()
		cfg.OutputExtension = format
		err := cfg.Validate()
		if (err == nil) != (format == "mkv" || format == "mp4" || format == "ts") {
			t.Fatal("wrong format validation", format, err)
		}
	}
}
