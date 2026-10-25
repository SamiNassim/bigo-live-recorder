package app

import (
	"errors"
	"math"
	"strings"
	"testing"

	"bigo-live-recorder/internal/bigo"
	"bigo-live-recorder/internal/logging"
	"bigo-live-recorder/internal/state"
	"github.com/egoist/mygo/ui"
)

func accountFixture(t *testing.T) *application {
	t.Helper()
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	log, err := logging.Open(store.Directory())
	if err != nil {
		t.Fatal(err)
	}
	a := newApplication(store, log)
	a.preview = true // No real account requests or monitoring in UI tests.
	t.Cleanup(func() { a.cancel(); log.Close() })
	return a
}

func TestSessionBadgeStates(t *testing.T) {
	for _, test := range []struct {
		name, saved, draft, checked, accountCookie string
		account                                    *bigo.Account
		err                                        error
		validating                                 bool
		want                                       string
	}{
		{name: "missing", want: "Session missing"},
		{name: "unsaved", draft: "synthetic-cookie", want: "Session changes unsaved"},
		{name: "removed but unsaved", saved: "synthetic-cookie", want: "Session changes unsaved"},
		{name: "startup", saved: "synthetic-cookie", draft: "synthetic-cookie", want: "Checking session…"},
		{name: "validating", saved: "synthetic-cookie", draft: "synthetic-cookie", checked: "synthetic-cookie", validating: true, want: "Checking session…"},
		{name: "expired", saved: "synthetic-cookie", draft: "synthetic-cookie", checked: "synthetic-cookie", err: bigo.ErrLoginRequired, want: "Session expired"},
		{name: "temporary failure", saved: "synthetic-cookie", draft: "synthetic-cookie", checked: "synthetic-cookie", err: errors.New("temporary network failure"), want: "Session unverified"},
		{name: "bad format", saved: "invalid value", draft: "invalid value", want: "Session invalid"},
		{name: "verified", saved: "synthetic-cookie", draft: "synthetic-cookie", accountCookie: "synthetic-cookie", account: &bigo.Account{ID: "900000001", Name: "Demo Viewer"}, want: "Demo Viewer"},
		{name: "canonical header", saved: "uniqid=synthetic-cookie", draft: "synthetic-cookie", accountCookie: "synthetic-cookie", account: &bigo.Account{ID: "900000001", Name: "Demo Viewer"}, want: "Demo Viewer"},
		{name: "no display name", saved: "synthetic-cookie", draft: "synthetic-cookie", accountCookie: "synthetic-cookie", account: &bigo.Account{ID: "900000001"}, want: "BIGO account"},
		{name: "stale identity", saved: "synthetic-cookie", draft: "synthetic-cookie", accountCookie: "other-synthetic-cookie", account: &bigo.Account{ID: "900000002", Name: "Old Account"}, want: "Checking session…"},
	} {
		t.Run(test.name, func(t *testing.T) {
			a := accountFixture(t)
			cfg := a.store.Config()
			cfg.Cookie = test.saved
			if err := a.store.SaveConfig(cfg); err != nil {
				t.Fatal(err)
			}
			a.cfg.Cookie = test.draft
			a.cookieLastChecked = test.checked
			a.accountCookie = test.accountCookie
			a.account = test.account
			a.accountError = test.err
			a.validating = test.validating
			badge := a.sessionBadge()
			if badge.name != test.want {
				t.Fatalf("got %q, want %q", badge.name, test.want)
			}
			if strings.Contains(badge.tooltip, "synthetic-cookie") {
				t.Fatal("credentials exposed in the session tooltip")
			}
		})
	}
}

func TestEmptyStreamersExplainSessionFirstAndSidebarOpensSettings(t *testing.T) {
	a := accountFixture(t)
	tt := ui.NewTester(a.view, 1050, 650)
	if !tt.HasText("Set up your session in Settings, then add a BIGO ID.") || !tt.HasText("Session missing") {
		t.Fatal(tt.Texts())
	}
	if err := tt.Click("BIGO session"); err != nil {
		t.Fatal(err)
	}
	if a.page != "settings" || !tt.HasText("BIGO account") {
		t.Fatal("session indicator did not open account settings")
	}
}

func TestSidebarAccountTextStaysWithinItsColumn(t *testing.T) {
	for _, name := range []string{"Demo Viewer", strings.Repeat("Long username ", 20), strings.Repeat("界🎵", 50)} {
		t.Run(name[:min(len(name), 12)], func(t *testing.T) {
			a := accountFixture(t)
			a.cfg.Cookie = "synthetic-cookie"
			if err := a.store.SaveConfig(a.cfg); err != nil {
				t.Fatal(err)
			}
			id := strings.Repeat("9", 64)
			a.account = &bigo.Account{Name: name, ID: id, Avatar: "https://cdn.example/avatar.png"}
			a.avatars[a.account.Avatar] = a.logo
			a.accountCookie = a.cfg.Cookie
			tt := ui.NewTester(a.view, 1050, 650)
			for _, label := range []string{strings.TrimSpace(name), "BIGO ID " + id} {
				r, ok := tt.Find(label)
				if !ok || r.X < 0 || r.X+r.W > 220 || r.W > 130 {
					t.Fatalf("account text escaped sidebar: %+v (%v)", r, ok)
				}
			}
			badge := a.sessionBadge()
			if !strings.Contains(badge.tooltip, strings.TrimSpace(name)) || !strings.Contains(badge.tooltip, id) {
				t.Fatal("full account identity missing from tooltip")
			}
			if r, ok := tt.Find("v" + Version); !ok || r.Y+r.H > 650 {
				t.Fatal("sidebar footer overflowed compact window", r)
			}
		})
	}
}

func TestSidebarAvatarUsesOnlyVerifiedSessionAndCachedImage(t *testing.T) {
	a := accountFixture(t)
	a.cfg.Cookie = "synthetic-cookie"
	if err := a.store.SaveConfig(a.cfg); err != nil {
		t.Fatal(err)
	}
	address := "https://cdn.example/avatar.png"
	a.account = &bigo.Account{ID: "900000001", Name: "Demo Viewer", Avatar: address}
	a.accountCookie = a.cfg.Cookie
	tt := ui.NewTester(a.view, 1050, 650)
	if _, ok := tt.Find("BIGO account avatar placeholder"); !ok {
		t.Fatal("avatar has no placeholder before the image is cached")
	}
	a.avatars[address] = a.logo
	tt.Frame()
	r, ok := tt.Find("BIGO account avatar")
	if !ok || r.W != 32 || r.H != 32 || r.X < 0 || r.X+r.W > 220 {
		t.Fatal("cached avatar missing or escaped the sidebar", r)
	}
	name, _ := tt.Find("Demo Viewer")
	if name.X < r.X+r.W {
		t.Fatal("avatar overlaps the account name")
	}
	if len(a.avatarLoading) != 0 {
		t.Fatal("rendering the avatar initiated another image download")
	}
	for _, cookie := range []string{"different-synthetic-cookie", ""} {
		a.accountCookie = cookie
		tt.Frame()
		if _, ok := tt.Find("BIGO account avatar"); ok {
			t.Fatal("avatar from an unverified or different account is visible")
		}
	}
	a.accountCookie = a.cfg.Cookie
	a.account.Avatar = ""
	tt.Frame()
	if _, ok := tt.Find("BIGO account avatar"); ok {
		t.Fatal("avatar retained after account stopped supplying one")
	}
}

func TestSessionShortcutAnchorsAccountSettings(t *testing.T) {
	for _, size := range [][2]int{{1050, 650}, {1400, 900}} {
		for _, native := range []bool{true, false} {
			a := accountFixture(t)
			a.cfg.NativeListening = native
			a.snapshot.Listener.Message = strings.Repeat("Waiting for the native listener to reconnect. ", 12)
			tt := ui.NewTester(a.view, size[0], size[1])
			for range 2 {
				if err := tt.Click("BIGO session"); err != nil {
					t.Fatal(err)
				}
				viewport, ok := tt.Find("Settings sections")
				if !ok {
					t.Fatal("settings scroller missing")
				}
				heading, ok := tt.Find("BIGO account")
				if !ok || math.Abs(float64(heading.Y-viewport.Y-21)) > 1 {
					t.Fatalf("account heading not at the top: heading=%+v viewport=%+v native=%v", heading, viewport, native)
				}
				if a.accountSettingsRequested {
					t.Fatal("account anchor request was not consumed")
				}
				a.settingsScroll.Y = a.settingsScroll.MaxY
				tt.Frame()
				if a.settingsScroll.Y != a.settingsScroll.MaxY {
					t.Fatal("account anchor prevented scrolling away")
				}
			}
			if err := tt.Click("Streamers"); err != nil {
				t.Fatal(err)
			}
			if err := tt.Click("Settings"); err != nil {
				t.Fatal(err)
			}
			if a.settingsScroll.Y != a.settingsScroll.MaxY {
				t.Fatal("ordinary navigation reset the settings scroll position")
			}
		}
	}
}

func TestChangingCookieClearsAccountIdentity(t *testing.T) {
	a := accountFixture(t)
	a.account = &bigo.Account{ID: "900000001", Name: "Demo Viewer"}
	a.accountCookie = "synthetic-cookie"
	a.accountError = bigo.ErrLoginRequired
	a.cookieLastChecked = "synthetic-cookie"
	a.cookieChanged()
	if a.account != nil || a.accountCookie != "" || a.accountError != nil || a.cookieLastChecked != "" {
		t.Fatal("old session state was retained")
	}
}
