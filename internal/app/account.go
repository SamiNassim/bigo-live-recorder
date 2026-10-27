package app

import (
	"errors"
	"strings"
	"unicode/utf8"

	"bigo-live-recorder/internal/bigo"
	"github.com/egoist/mygo/ui"
)

type sessionBadge struct {
	name, detail, tooltip string
	avatar                string
	attention, invalid    bool
}

// The footer describes the saved session, never an unsaved replacement or an
// identity returned for another cookie. Account credentials are not rendered.
func (a *application) sessionBadge() sessionBadge {
	saved, savedErr := bigo.ParseCookie(a.store.Config().Cookie)
	cookie, err := bigo.ParseCookie(a.cfg.Cookie)
	if err != nil || savedErr != nil {
		return sessionBadge{name: "Session invalid", detail: "Check in Settings", tooltip: "The session cookie format is invalid. Update it in Settings.", invalid: true}
	}
	if cookie != saved {
		return sessionBadge{name: "Session changes unsaved", detail: "Save in Settings", tooltip: "Save your session changes in Settings before this account is used.", attention: true}
	}
	if strings.TrimSpace(saved) == "" {
		return sessionBadge{name: "Session missing", detail: "Set up in Settings", tooltip: "Set up your BIGO session in Settings, then add a streamer.", attention: true}
	}
	if a.account != nil && a.accountCookie == saved && a.account.ID != "" {
		name := strings.TrimSpace(a.account.Name)
		if name == "" {
			name = "BIGO account"
		}
		return sessionBadge{name: name, detail: "BIGO ID " + a.account.ID, avatar: a.account.Avatar, tooltip: name + "\nBIGO ID " + a.account.ID + "\nLast verified account for the saved session. Open Settings to validate again."}
	}
	if a.validating || a.cookieLastChecked != a.cfg.Cookie {
		return sessionBadge{name: "Checking session…", detail: "Verifying BIGO account", tooltip: "Checking the account associated with your saved session.", attention: true}
	}
	if errors.Is(a.accountError, bigo.ErrLoginRequired) {
		return sessionBadge{name: "Session expired", detail: "Update in Settings", tooltip: "BIGO rejected the saved session. Sign in again and update the cookie in Settings.", invalid: true}
	}
	return sessionBadge{name: "Session unverified", detail: "Check in Settings", tooltip: "The saved session could not be verified. Check your connection or validate the account again in Settings.", attention: true}
}

func (a *application) sessionIndicator(c *ui.Context) {
	badge := a.sessionBadge()
	color := c.Theme().Text
	if badge.attention {
		color = c.Theme().Warning
	}
	if badge.invalid {
		color = c.Theme().Danger
	}
	if ui.ButtonBase(c).Label("BIGO session").FillWidth().MinWidth(0).Padding(0).Tooltip(badge.tooltip).Children(func() {
		ui.Row(c).FillWidth().MinWidth(0).Gap(10).AlignItems(ui.Center).Children(func() {
			if badge.avatar != "" {
				if avatar := a.avatar(c, badge.avatar); avatar != nil {
					ui.Image(c, avatar).Label("BIGO account avatar").Size(32, 32).Shrink(0).Radius(16)
				} else {
					initial, _ := utf8.DecodeRuneInString(badge.name)
					ui.Box(c).Label("BIGO account avatar placeholder").Size(32, 32).Shrink(0).Radius(16).Background(c.Theme().Accent.Alpha(0.15)).Center().Children(func() {
						ui.Text(c, strings.ToUpper(string(initial))).FontWeight(600).TextColor(c.Theme().Accent)
					})
				}
			}
			ui.Column(c).Grow(1).MinWidth(0).Gap(4).Children(func() {
				ui.Text(c, badge.name).FontSize(12).FontWeight(600).TextColor(color).FillWidth().MinWidth(0).SingleLine().Tooltip(badge.tooltip)
				ui.Text(c, badge.detail).FontSize(10).TextColor(c.Theme().TextMuted).FillWidth().MinWidth(0).SingleLine().Tooltip(badge.tooltip)
			})
		})
	}).Clicked() {
		a.page = "settings"
		a.accountSettingsRequested = true
	}
}
