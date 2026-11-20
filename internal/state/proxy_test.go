package state

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"bigo-live-recorder/internal/network"
)

func TestProxySettingsRoundTripAndLegacyDefault(t *testing.T) {
	dir := t.TempDir()
	legacy := Defaults()
	raw, _ := json.Marshal(legacy)
	var fields map[string]json.RawMessage
	json.Unmarshal(raw, &fields)
	delete(fields, "proxy")
	raw, _ = json.Marshal(fields)
	if err := os.WriteFile(filepath.Join(dir, "config.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	store, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if store.Config().Proxy.Enabled {
		t.Fatal("proxy enabled for an existing installation")
	}
	cfg := store.Config()
	cfg.Proxy = network.Config{Enabled: true, URL: "socks5://127.0.0.1:1080", Username: "synthetic-user", Password: "synthetic-password"}
	if err := store.SaveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if reopened.Config().Proxy != cfg.Proxy {
		t.Fatal("proxy settings were not persisted")
	}
	cfg.Proxy.URL = "invalid"
	if err := store.SaveConfig(cfg); err == nil {
		t.Fatal("invalid enabled proxy was saved")
	}
	cfg.Proxy.Enabled = false
	if err := store.SaveConfig(cfg); err != nil {
		t.Fatal("disabled proxy could not retain its draft address", err)
	}
}
