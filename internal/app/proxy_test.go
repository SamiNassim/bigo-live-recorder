package app

import (
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/egoist/mygo/ui"
)

func TestNativeProxySettingsTogglePersistAndHidePassword(t *testing.T) {
	for _, theme := range []string{"dark", "light"} {
		t.Run(theme, func(t *testing.T) {
			a := accountFixture(t)
			a.cfg.Theme = theme
			a.page = "settings"
			tt := ui.NewTester(a.view, 1050, 750)
			if a.cfg.Proxy.Enabled {
				t.Fatal("proxy is enabled by default")
			}
			if _, ok := tt.Find("Proxy URL"); ok {
				t.Fatal("proxy details visible before enabling")
			}
			control, ok := tt.Find("Enable proxy connection")
			if !ok {
				t.Fatal("native proxy switch missing")
			}
			viewport, _ := tt.Find("Settings sections")
			a.settingsScroll.Y += control.Y - viewport.Y - 80
			tt.Frame()
			if err := tt.Click("Enable proxy connection"); err != nil {
				t.Fatal(err)
			}
			if !a.cfg.Proxy.Enabled || !a.unsavedSettings() {
				t.Fatal("switch did not update draft settings")
			}
			for _, label := range []string{"Proxy URL", "Proxy username", "Proxy password"} {
				if _, ok := tt.Find(label); !ok {
					t.Fatal("missing proxy input", label)
				}
			}
			a.cfg.Proxy.URL = "socks5://127.0.0.1:1080"
			a.cfg.Proxy.Username = "synthetic-user"
			a.cfg.Proxy.Password = "synthetic-password"
			tt.Frame()
			if strings.Contains(strings.Join(tt.Texts(), "\n"), a.cfg.Proxy.Password) {
				t.Fatal("proxy password is visible")
			}
			if err := a.store.SaveConfig(a.settingsConfig()); err != nil {
				t.Fatal(err)
			}
			a.cfg = a.store.Config()
			a.loadNumbers()
			if a.unsavedSettings() {
				t.Fatal("saved proxy still marked unsaved")
			}
			a.settingsScroll.Y = max(0, a.settingsScroll.Y-40)
			tt.Frame()
			tt.Advance(300 * time.Millisecond)
			if dir := os.Getenv("BIGO_UI_SNAPSHOTS"); dir != "" {
				if err := os.MkdirAll(dir, 0755); err != nil {
					t.Fatal(err)
				}
				file, err := os.Create(filepath.Join(dir, "proxy-"+theme+".png"))
				if err != nil {
					t.Fatal(err)
				}
				err = png.Encode(file, tt.Image())
				closeErr := file.Close()
				if err != nil || closeErr != nil {
					t.Fatal(err, closeErr)
				}
			}
			if err := tt.Click("Enable proxy connection"); err != nil {
				t.Fatal(err)
			}
			if a.cfg.Proxy.Enabled || a.cfg.Proxy.Password != "synthetic-password" {
				t.Fatal("disabling proxy lost credentials or left proxy enabled")
			}
		})
	}
}
