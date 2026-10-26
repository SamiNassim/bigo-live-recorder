// Package state persists small application snapshots as atomic JSON files.
package state

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type Config struct {
	Window              Window `json:"window"`
	Theme               string `json:"theme"`
	RecordingPath       string `json:"recording_path"`
	FFmpegPath          string `json:"ffmpeg_path,omitempty"`
	Cookie              string `json:"bigo_cookie,omitempty"`
	NativeDeviceID      string `json:"native_device_id"`
	NativeListening     bool   `json:"native_listening"`
	PollingBackup       bool   `json:"polling_backup"`
	PollSeconds         int    `json:"poll_seconds"`
	ColdSeconds         int    `json:"cold_seconds"`
	RequestDelaySeconds int    `json:"request_delay_seconds"`
	AutoPromote         bool   `json:"auto_promote"`
	Notifications       bool   `json:"notifications"`
	FolderPerStreamer   bool   `json:"folder_per_streamer"`
	PostProcess         bool   `json:"post_process"`
	FFmpegArgs          string `json:"ffmpeg_args"`
	OutputExtension     string `json:"output_extension"`
}

type Window struct {
	X          int  `json:"x"`
	Y          int  `json:"y"`
	Width      int  `json:"width"`
	Height     int  `json:"height"`
	Maximized  bool `json:"maximized,omitempty"`
	FullScreen bool `json:"fullScreen,omitempty"`
}

type Streamer struct {
	ID              string    `json:"id"`
	UID             uint32    `json:"uid,omitempty"`
	Name            string    `json:"name,omitempty"`
	CustomName      string    `json:"custom_name,omitempty"`
	Avatar          string    `json:"avatar,omitempty"`
	Paused          bool      `json:"paused"`
	RecordingPaused bool      `json:"recording_paused,omitempty"`
	LowPriority     bool      `json:"low_priority"`
	AddedAt         time.Time `json:"added_at"`
	LastRecordedAt  time.Time `json:"last_recorded_at,omitempty"`
}

func (s Streamer) DisplayName() string {
	if s.CustomName != "" {
		return s.CustomName
	}
	if s.Name != "" {
		return s.Name
	}
	return s.ID
}

type Recording struct {
	ID            string    `json:"id"`
	StreamerID    string    `json:"streamer_id"`
	Name          string    `json:"name"`
	Path          string    `json:"path"`
	StartedAt     time.Time `json:"started_at"`
	EndedAt       time.Time `json:"ended_at,omitempty"`
	Bytes         int64     `json:"bytes"`
	Error         string    `json:"error,omitempty"`
	ProcessedPath string    `json:"processed_path,omitempty"`
}

type Data struct {
	Version    int         `json:"version"`
	Streamers  []Streamer  `json:"streamers"`
	Recordings []Recording `json:"recordings"`
}

func Defaults() Config {
	home, _ := os.UserHomeDir()
	return Config{Theme: "system", RecordingPath: filepath.Join(home, "Movies", "BIGO Live Recorder"), NativeListening: true,
		NativeDeviceID: "web_" + rand.Text(),
		PollSeconds:    75, ColdSeconds: 300, RequestDelaySeconds: 2, AutoPromote: true, Notifications: true, FolderPerStreamer: true,
		FFmpegArgs: `-map 0:v? -map 0:a? -c copy`, OutputExtension: "mkv"}
}

func (c Config) Validate() error {
	if c.NativeDeviceID == "" {
		return errors.New("BIGO listener device identity is missing")
	}
	if c.Theme != "system" && c.Theme != "dark" && c.Theme != "light" {
		return errors.New("choose System, Dark, or Light appearance")
	}
	if strings.TrimSpace(c.RecordingPath) == "" {
		return errors.New("choose a recording folder")
	}
	if c.PollSeconds < 10 || c.PollSeconds > 1800 {
		return errors.New("normal interval must be between 10 and 1800 seconds")
	}
	if c.ColdSeconds < c.PollSeconds || c.ColdSeconds > 7200 {
		return errors.New("low-priority interval must be at least the normal interval and at most 7200 seconds")
	}
	if c.RequestDelaySeconds < 1 || c.RequestDelaySeconds > 30 {
		return errors.New("request delay must be between 1 and 30 seconds")
	}
	if c.OutputExtension != "mkv" && c.OutputExtension != "mp4" && c.OutputExtension != "ts" {
		return errors.New("choose MKV, MP4, or TS for the optional processed copy")
	}
	return nil
}

func DefaultDirectory() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "BIGO Live Recorder"), nil
}

type Store struct {
	mu     sync.RWMutex
	dir    string
	config Config
	data   Data
}

func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	s := &Store{dir: dir, config: Defaults(), data: Data{Version: 1, Streamers: []Streamer{}, Recordings: []Recording{}}}
	migrateConfig := false
	for _, file := range []struct {
		name  string
		value any
	}{{"config.json", &s.config}, {"data.json", &s.data}} {
		b, err := os.ReadFile(filepath.Join(dir, file.name))
		if errors.Is(err, os.ErrNotExist) {
			if err = writeJSON(filepath.Join(dir, file.name), file.value); err != nil {
				return nil, err
			}
			continue
		}
		if err != nil {
			return nil, err
		}
		if err = json.Unmarshal(b, file.value); err != nil {
			return nil, fmt.Errorf("read %s: %w (original file preserved)", file.name, err)
		}
		if file.name == "config.json" {
			var fields map[string]json.RawMessage
			if err = json.Unmarshal(b, &fields); err != nil {
				return nil, err
			}
			_, hadNativeSession := fields["native_session"]
			_, hadDeviceID := fields["native_device_id"]
			migrateConfig = hadNativeSession || !hadDeviceID
			_, hadPostProcess := fields["post_process"]
			_, hadOutputFormat := fields["output_extension"]
			_, hadOutputArgs := fields["ffmpeg_args"]
			legacyConvert, hadConversion := fields["convert_to_mp4"]
			_, hadDeletion := fields["delete_after_conversion"]
			if hadConversion && !hadPostProcess {
				if err := json.Unmarshal(legacyConvert, &s.config.PostProcess); err != nil {
					return nil, fmt.Errorf("read legacy conversion preference: %w", err)
				}
				if s.config.PostProcess {
					if !hadOutputFormat {
						s.config.OutputExtension = "mp4"
					}
					if !hadOutputArgs {
						s.config.FFmpegArgs += " -movflags +faststart"
					}
				}
			}
			migrateConfig = migrateConfig || hadConversion || hadDeletion || !hadPostProcess || !hadOutputFormat || !hadOutputArgs
		}
	}
	if err := s.config.Validate(); err != nil {
		return nil, fmt.Errorf("config.json: %w", err)
	}
	if s.data.Version != 1 {
		return nil, fmt.Errorf("unsupported data.json version %d", s.data.Version)
	}
	// Drop obsolete session and conversion fields only after both files validate.
	// The existing web cookie, preferences, streamers and history are preserved.
	if migrateConfig {
		if err := writeJSON(filepath.Join(dir, "config.json"), s.config); err != nil {
			return nil, fmt.Errorf("migrate configuration: %w", err)
		}
	}
	return s, nil
}

func (s *Store) Directory() string { return s.dir }

func (s *Store) Config() Config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.config
}

func (s *Store) Data() Data {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return clone(s.data)
}
func clone(d Data) Data {
	d.Streamers = append([]Streamer(nil), d.Streamers...)
	d.Recordings = append([]Recording(nil), d.Recordings...)
	return d
}
func (s *Store) SaveConfig(c Config) error {
	if err := c.Validate(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	c.Window = s.config.Window
	if err := writeJSON(filepath.Join(s.dir, "config.json"), c); err != nil {
		return err
	}
	s.config = c
	return nil
}

func (s *Store) SaveWindow(w Window) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.config
	c.Window = w
	if err := writeJSON(filepath.Join(s.dir, "config.json"), c); err != nil {
		return err
	}
	s.config = c
	return nil
}
func (s *Store) Update(fn func(*Data) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := clone(s.data)
	if err := fn(&next); err != nil {
		return err
	}
	if len(next.Recordings) > 2000 {
		next.Recordings = next.Recordings[len(next.Recordings)-2000:]
	}
	if err := writeJSON(filepath.Join(s.dir, "data.json"), next); err != nil {
		return err
	}
	s.data = next
	return nil
}

func writeJSON(name string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(name), ".snapshot-*.tmp")
	if err != nil {
		return err
	}
	temp := f.Name()
	defer os.Remove(temp)
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(append(b, '\n'))
	}
	if err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err = os.Rename(temp, name); err != nil {
		return err
	}
	// Best-effort directory sync: unsupported on some Windows filesystems.
	if dir, e := os.Open(filepath.Dir(name)); e == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}
	return nil
}
