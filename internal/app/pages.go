package app

import (
	"fmt"
	"math"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"bigo-live-recorder/internal/monitor"
	"bigo-live-recorder/internal/state"
	"github.com/egoist/mygo"
	"github.com/egoist/mygo/ui"
)

func (a *application) streamers(c *ui.Context) {
	t := c.Theme()
	header(c, "Streamers", fmt.Sprintf("%d streamers  ·  %d recording  ·  %d recording paused  ·  %d monitoring paused", len(a.snapshot.Rows), a.snapshot.Recording, a.snapshot.RecordingPaused, a.snapshot.MonitoringPaused), func() {
		if ui.PrimaryButton(c, "+  Add streamer").Height(36).Clicked() {
			a.addOpen = true
		}
	})
	ui.Column(c).Grow(1).MinHeight(0).FillWidth().Background(t.Surface).Border(1, t.Border).Radius(8).Children(func() {
		ui.Row(c).Padding(16).Gap(10).Wrap().AlignItems(ui.Center).FillWidth().Children(func() {
			ui.SearchField(c, &a.query).Placeholder("Search streamers…").Label("Search streamers").Grow(1).MinWidth(140).Height(34)
			ui.Select(c, &a.status, []string{"All statuses", "Recording", "Live", "Offline", "Recording paused", "Monitoring paused", "Needs attention", "Setup needed", "Waiting", "Finalizing"}).Width(160).Height(34).Label("Filter status")
			ui.Select(c, &a.priority, []string{"All priorities", "Normal priority", "Low priority"}).Width(150).Height(34).Label("Filter priority")
			ui.Select(c, &a.order, []string{"Last recorded", "Name", "Date added", "Status"}).Width(144).Height(34).Label("Sort streamers")
		})
		ui.Divider(c)
		rows := a.streamerRows()
		a.streamList.Key = func(i int) any { return rows[i].Streamer.ID }
		a.streamList.KeepOffset = true
		a.streamList.CellMenu = func(m *ui.Menu, row, _ int) { a.streamMenu(m, rows[row]) }
		if len(rows) == 0 {
			ui.Column(c).Grow(1).FillWidth().Center().Gap(12).Children(func() {
				title, subtitle := "No streamers yet", "Set up your session in Settings, then add a BIGO ID."
				if len(a.snapshot.Rows) > 0 {
					title = "No matching streamers"
					subtitle = "Try another search or filter."
				}
				ui.Text(c, title).FontSize(20).FontWeight(600)
				ui.Text(c, subtitle).TextColor(t.TextMuted)
			})
		} else {
			columns := []ui.TableColumn{{Title: "Streamer", MinWidth: 160}, {Title: "Status", Width: 160}, {Title: "Duration", Width: 80}, {Title: "Last recorded", Width: 140}, {Title: "Next check", Width: 100}, {Title: "Priority", Width: 80}, {Title: "", ID: "actions", Width: 52, Fixed: true}}
			listening := a.snapshot.Listener.Connected && a.store.Config().NativeListening
			first, last := a.streamList.Visible()
			ui.Table(c, &a.streamList, columns, len(rows), func(row, col int) {
				r := rows[row]
				switch col {
				case 0:
					ui.Row(c).Height(58).Gap(12).AlignItems(ui.Center).Children(func() {
						initial, _ := utf8.DecodeRuneInString(r.Streamer.DisplayName())
						if avatar := a.avatar(c, r.Streamer.Avatar); avatar != nil {
							ui.Image(c, avatar).Label("Avatar for "+r.Streamer.ID).Size(34, 34).Radius(17)
						} else {
							ui.Box(c).Size(34, 34).Radius(17).Background(t.Accent.Alpha(0.15)).Center().Children(func() { ui.Text(c, strings.ToUpper(string(initial))).FontWeight(600).TextColor(t.Accent) })
						}
						ui.Column(c).Grow(1).MinWidth(0).Gap(4).Children(func() {
							ui.Text(c, r.Streamer.DisplayName()).FontWeight(600).SingleLine()
							ui.Text(c, r.Streamer.ID).FontSize(11).TextColor(t.TextMuted).SingleLine()
						})
					})
				case 1:
					color := t.TextMuted
					switch r.Status {
					case "Recording":
						color = t.Danger
					case "Live":
						color = t.Success
					case "Monitoring paused":
						color = t.Warning
					case "Checking", "Starting":
						color = t.Accent
					case "Needs attention":
						color = t.Warning
					}
					ui.Column(c).Gap(4).Children(func() {
						detail := r.Error
						if !r.LiveCheckedAt.IsZero() {
							if detail != "" {
								detail += "\n"
							}
							detail += "Last verified: " + timestamp(r.LiveCheckedAt)
						}
						label := r.Status
						if a.cfg.NativeListening && !a.cfg.PollingBackup && r.Streamer.RecordingPaused {
							if label == "Live" {
								label = "Live (last check)"
							}
							detail += "\nNative-only presence is last confirmed status; it is updated on events, not continuously polled."
						}
						ui.Text(c, "●  "+label).FontSize(12).TextColor(color).SingleLine().Tooltip(detail)
						if r.Streamer.RecordingPaused && !r.Streamer.Paused {
							ui.Text(c, "Recording paused").FontSize(10).TextColor(t.Warning)
						}
					})
				case 2:
					label := "—"
					if !r.StartedAt.IsZero() {
						elapsed := c.Now().Sub(r.StartedAt)
						if row >= first && row <= last {
							refreshClock(c, elapsed, false)
						}
						label = duration(elapsed)
					}
					ui.Text(c, label).FontSize(12)
				case 3:
					ui.Text(c, timestamp(r.Streamer.LastRecordedAt)).FontSize(11).TextColor(t.TextMuted)
				case 4:
					label := nextCheckText(c, r, row >= first && row <= last, listening)
					color := t.TextMuted
					if label == "Listening" {
						color = t.Success
					}
					ui.Text(c, label).FontSize(11).TextColor(color)
				case 5:
					label := "Normal"
					if r.Streamer.LowPriority {
						label = "Low"
					}
					ui.Text(c, label).FontSize(11).TextColor(t.TextMuted)
				case 6:
					ui.ButtonBase(c).Label("Actions for "+r.Streamer.DisplayName()).Size(32, 30).Radius(5).Border(1, t.Border).Background(t.Surface).Children(func() {
						ui.Icon(c, dotsIcon).Size(18, 18)
					}).Menu(func(m *ui.Menu) { a.streamMenu(m, r) })
				}
			}).Grow(1).MinHeight(0).FillWidth()
		}
		ui.Divider(c)
		ui.Row(c).Padding(14, 18).FillWidth().Children(func() {
			ui.Text(c, fmt.Sprintf("%d of %d streamers", len(rows), len(a.snapshot.Rows))).FontSize(11).TextColor(t.TextMuted).Grow(1)
			ui.Text(c, "Sorted by "+strings.ToLower(a.order)).FontSize(11).TextColor(t.TextMuted)
		})
	})
}

func (a *application) streamMenu(m *ui.Menu, r monitor.Row) {
	s := r.Streamer
	if m.Item("Open BIGO page").Chosen() {
		_ = mygo.Shell.OpenExternal("https://www.bigo.tv/" + s.ID)
	}
	if m.Item("Rename…").Chosen() {
		a.renameID = s.ID
		a.renameName = s.CustomName
		a.renameOpen = true
	}
	pause := "Pause monitoring"
	if s.Paused {
		pause = "Resume monitoring"
	}
	recordingAction := "Pause recording"
	if s.RecordingPaused {
		recordingAction = "Resume recording"
	}
	if m.Item(recordingAction).Disabled(a.preview).Chosen() {
		a.work(func() error {
			if err := a.engine.SetRecordingPaused(s.ID, !s.RecordingPaused); err != nil {
				return err
			}
			message := "Recording paused; monitoring remains active"
			if s.RecordingPaused {
				message = "Recording enabled; checking stream"
			}
			if s.Paused {
				message = "Recording preference saved; monitoring is still paused"
			}
			a.update(func() { a.toasts = append(a.toasts, message) })
			return nil
		})
	}
	if m.Item(pause).Disabled(a.preview).Chosen() {
		a.work(func() error { return a.engine.SetMonitoringPaused(s.ID, !s.Paused) })
	}
	priority := "Set low priority"
	if s.LowPriority {
		priority = "Set normal priority"
	}
	if m.Item(priority).Disabled(a.preview).Chosen() {
		a.work(func() error { return a.engine.Edit(s.ID, func(v *state.Streamer) { v.LowPriority = !v.LowPriority }) })
	}
	if r.Status == "Recording" {
		m.Separator()
		if m.Item("Show recording in folder").Chosen() {
			a.reveal(r.Path)
		}
	}
	m.Separator()
	if m.Item("Remove streamer…").Disabled(a.preview).Chosen() {
		a.removeID = s.ID
		a.removeOpen = true
	}
}

func (a *application) logsPage(c *ui.Context) {
	t := c.Theme()
	header(c, "Logs", "Monitoring, listener, and recording events from this device.", func() {
		if ui.Button(c, "Open log folder").Clicked() {
			a.openFolder(a.store.Directory())
		}
	})
	ui.Column(c).Grow(1).MinHeight(0).FillWidth().Background(t.Surface).Border(1, t.Border).Radius(8).Children(func() {
		ui.Row(c).Padding(16).Gap(12).FillWidth().Children(func() {
			ui.SearchField(c, &a.logQuery).Placeholder("Search logs…").Label("Search logs").Grow(1).Height(34)
			ui.Select(c, &a.level, []string{"All levels", "ERROR", "WARN", "INFO", "DEBUG"}).Label("Log level").Width(144).Height(34)
			if ui.Button(c, "Clear view").Height(34).Clicked() {
				a.log.ClearView()
				a.logs = nil
			}
		})
		ui.Divider(c)
		entries := a.logRows()
		a.logList.Key = func(i int) any { return entries[i].Sequence }
		a.logList.KeepOffset = true
		services := c.Services()
		a.logList.CellMenu = func(m *ui.Menu, row, _ int) {
			logMenu(m, entries[row], services)
		}
		ui.Table(c, &a.logList, []ui.TableColumn{{Title: "Time", Width: 150}, {Title: "Level", Width: 70}, {Title: "Streamer", Width: 140}, {Title: "Message", MinWidth: 200}}, len(entries), func(row, col int) {
			e := entries[row]
			switch col {
			case 0:
				ui.Text(c, timestamp(e.Time)).FontSize(11).TextColor(t.TextMuted)
			case 1:
				color := t.Accent
				if e.Level == "WARN" {
					color = t.Warning
				}
				if e.Level == "ERROR" {
					color = t.Danger
				}
				ui.Text(c, e.Level).FontSize(11).TextColor(color)
			case 2:
				ui.Text(c, e.Streamer).FontSize(11).TextColor(t.TextMuted).SingleLine()
			case 3:
				ui.Text(c, e.Message).FontSize(12).MaxLines(4).Padding(12, 0).Selectable().Tooltip(e.Message).ContextMenu(func(m *ui.Menu) { logMenu(m, e, services) })
			}
		}).Grow(1).MinHeight(0).FillWidth()
		ui.Divider(c)
		ui.Text(c, fmt.Sprintf("%d matching · %d retained · newest first · right-click for details · disk logs rotate at 15 MB total", len(entries), len(a.logs))).Padding(14, 18).FontSize(11).TextColor(t.TextMuted)
	})
}

func (a *application) recordings(c *ui.Context) {
	t := c.Theme()
	hasCompleted := slices.ContainsFunc(a.data.Recordings, func(r state.Recording) bool { return !r.EndedAt.IsZero() })
	header(c, "Recordings", "Original quality. Local files. Short-cluster Matroska by default.", func() {
		if ui.Button(c, "Clear history…").Disabled(a.busy || a.preview || !hasCompleted).Clicked() {
			a.clearHistoryOpen = true
		}
		if ui.PrimaryButton(c, "Open recordings folder").Clicked() {
			a.openFolder(a.cfg.RecordingPath)
		}
	})
	recordings := a.recordingRows()
	a.recordList.Key = func(i int) any { return recordings[i].ID }
	a.recordList.KeepOffset = true
	services := c.Services()
	a.recordList.CellMenu = func(m *ui.Menu, row, _ int) { a.recordingMenu(m, recordings[row], services) }
	first, last := a.recordList.Visible()
	ui.Column(c).Grow(1).MinHeight(0).FillWidth().Background(t.Surface).Border(1, t.Border).Radius(8).Children(func() {
		ui.Table(c, &a.recordList, []ui.TableColumn{{Title: "Streamer", Width: 150}, {Title: "Started", Width: 150}, {Title: "Duration", Width: 90}, {Title: "Size", Width: 85}, {Title: "File", MinWidth: 140}, {Title: "", ID: "open", Width: 75}}, len(recordings), func(row, col int) {
			r := recordings[row]
			switch col {
			case 0:
				ui.Text(c, r.Name).FontWeight(600).SingleLine()
			case 1:
				ui.Text(c, timestamp(r.StartedAt)).FontSize(11)
			case 2:
				d := c.Now().Sub(r.StartedAt)
				if !r.EndedAt.IsZero() {
					d = r.EndedAt.Sub(r.StartedAt)
				} else if row >= first && row <= last {
					refreshClock(c, d, false)
				}
				ui.Text(c, duration(d)).FontSize(11)
			case 3:
				label := bytesLabel(r.Bytes)
				if r.EndedAt.IsZero() {
					label = "Recording…"
				}
				ui.Text(c, label).FontSize(11)
			case 4:
				ui.Text(c, filepath.Base(r.Path)).FontSize(11).SingleLine().Tooltip(r.Path + "\n" + r.ProcessedPath + "\n" + r.Error)
			case 5:
				if ui.Button(c, "Show").Clicked() {
					a.reveal(r.Path)
				}
			}
		}).Grow(1).MinHeight(0).FillWidth()
		ui.Text(c, fmt.Sprintf("%d recordings in history · clearing history never deletes files", len(recordings))).FontSize(11).TextColor(t.TextMuted).Padding(14, 18)
	})
}

func section(c *ui.Context, title, subtitle string, body func()) ui.Element {
	t := c.Theme()
	return ui.Column(c).FillWidth().Padding(20).Gap(15).Background(t.Surface).Border(1, t.Border).Radius(8).Children(func() {
		ui.Text(c, title).FontSize(16).FontWeight(600)
		if subtitle != "" {
			ui.Text(c, subtitle).TextColor(t.TextMuted).FontSize(12).MaxLines(3)
		}
		body()
	})
}
func toggle(c *ui.Context, label, detail string, value *bool) {
	ui.Row(c).FillWidth().Gap(20).AlignItems(ui.Center).Children(func() {
		ui.Column(c).Grow(1).Gap(4).Children(func() {
			ui.Text(c, label).FontWeight(500)
			if detail != "" {
				ui.Text(c, detail).TextColor(c.Theme().TextMuted).FontSize(11).MaxLines(3)
			}
		})
		ui.Switch(c, value).Label(label)
	})
}
func number(c *ui.Context, label string, value *float64, lo, hi float64) {
	ui.Column(c).Grow(1).Gap(8).Children(func() {
		ui.Text(c, label).FontSize(11).TextColor(c.Theme().TextMuted)
		ui.NumberInput(c, value, lo, hi, 1).FillWidth().Height(34).Label(label)
	})
}

func (a *application) pollingQueue(c *ui.Context) {
	inactive := a.cfg.NativeListening && !a.cfg.PollingBackup
	ui.Column(c).FillWidth().Disabled(inactive).Children(func() {
		section(c, "Polling queue", "Checks are serialized and spaced globally. Streamers being recorded are excluded from API checks.", func() {
			ui.Row(c).Gap(18).FillWidth().Children(func() {
				number(c, "Normal interval (seconds)", &a.normalSeconds, 10, 1800)
				number(c, "Low-priority interval (seconds)", &a.coldSeconds, 10, 7200)
				number(c, "Between requests (seconds)", &a.delaySeconds, 1, 30)
			})
			eligible := a.snapshot.QueueTargets
			cfg := a.cfg
			cfg.PollSeconds = int(a.normalSeconds)
			cfg.ColdSeconds = int(a.coldSeconds)
			cfg.RequestDelaySeconds = int(a.delaySeconds)
			normal, cold := monitor.EffectiveIntervals(cfg, max(eligible, 0))
			ui.Text(c, fmt.Sprintf("Queue calculator · %d eligible streamers\nEstimated cadence: normal %s  /  low priority %s\nQueue floor: %s. Network latency can increase these estimates.", eligible, duration(normal), duration(cold), duration(time.Duration(max(eligible, 0)*cfg.RequestDelaySeconds)*time.Second))).Padding(14).FillWidth().Radius(6).Background(c.Theme().Accent.Alpha(0.08)).FontSize(12)
			if inactive {
				ui.Text(c, "Native-only mode: these intervals are inactive except for initial and event-triggered checks.").FontSize(11).TextColor(c.Theme().TextMuted)
			}
			toggle(c, "Promote low-priority streamers when live", "Move a low-priority account to the normal queue after recording starts.", &a.cfg.AutoPromote)
		})
	})
}

func (a *application) settings(c *ui.Context) {
	t := c.Theme()
	header(c, "Settings", "One BIGO account, native notifications, and your recording workflow.", func() {
		if a.unsavedSettings() {
			ui.Row(c).AlignItems(ui.Center).Gap(6).Shrink(0).TextColor(t.Warning).Children(func() {
				ui.Icon(c, warningIcon).Size(16, 16).Label("Unsaved settings warning")
				ui.Text(c, "Unsaved settings").FontSize(12).FontWeight(500)
			})
		}
		if ui.PrimaryButton(c, "Save settings").Disabled(a.busy || a.preview || a.ffmpegInstalling).Height(36).Clicked() {
			cfg := a.settingsConfig()
			a.work(func() error { return a.saveSettings(cfg) })
		}
	})
	if a.accountSettingsRequested {
		// Reveal from below so the native scroller aligns the section's start,
		// rather than only bringing its bottom into view. Layout sets the offset.
		a.settingsScroll.Y = math.MaxInt32
	}
	ui.Scroll(c).Label("Settings sections").TrackScroll(&a.settingsScroll).Grow(1).MinHeight(0).FillWidth().Gap(18).DrawOver(func(p *ui.Painter, r ui.Rect) {
		// A click can rebuild the view more than once before layout. Keep the
		// reveal request until the requested frame has actually been drawn.
		a.accountSettingsRequested = false
		drawScrollFades(p, r, &a.settingsScroll, t.Background)
	}).Children(func() {
		section(c, "Monitoring", "Native notifications are the default. Regular polling runs only when you explicitly select it below.", func() {
			toggle(c, "Native live notifications", "One shared BIGO connection, authenticated with your web cookie. Automatically follows monitored accounts and enables their live notices.", &a.cfg.NativeListening)
			if a.cfg.NativeListening {
				toggle(c, "Enable polling backup", "Also check the queue at the configured cadence. Disabling this prevents automatic polling if the listener disconnects.", &a.cfg.PollingBackup)
			}
			color := t.Warning
			if a.snapshot.Listener.Connected && a.snapshot.Listener.TotalTargets > 0 && a.snapshot.Listener.ReadyTargets == a.snapshot.Listener.TotalTargets {
				color = t.Success
			}
			message := a.snapshot.Listener.Message
			if message == "" {
				message = "Paste your BIGO uniqid cookie below to connect."
			}
			if !a.cfg.NativeListening {
				message = "Native listener disabled. Queued polling is selected."
				color = t.TextMuted
			}
			ui.Text(c, "●  "+message).FontSize(12).TextColor(color).MaxLines(4)
			ui.Text(c, fmt.Sprintf("%d / %d native subscriptions ready", a.snapshot.Listener.ReadyTargets, a.snapshot.Listener.TotalTargets)).FontSize(11).TextColor(t.TextMuted)
		})
		accountSection := section(c, "BIGO account", "One cookie for native notifications and recording. No Android session, emulator, or running browser is needed.", func() {
			ui.Row(c).Gap(10).FillWidth().Children(func() {
				if ui.TextInput(c, &a.cfg.Cookie).Password().Placeholder("Paste the uniqid cookie value").Label("BIGO web cookie").Grow(1).Height(34).Changed() {
					a.cookieChanged()
				}
				if ui.Button(c, "Paste cookie").Clicked() {
					a.paste(&a.cfg.Cookie)
					a.cookieChanged()
				}
				if ui.Button(c, "Validate account").Disabled(a.validating).Clicked() {
					a.validateCookie()
				}
			})
			if a.accountMessage != "" {
				color := t.Warning
				if a.account != nil {
					color = t.Success
				}
				ui.Text(c, a.accountMessage).TextColor(color).Selectable()
			}
			ui.Text(c, "1. Sign in at bigo.tv in Brave or Chrome.\n2. Open Developer Tools → Application → Cookies → https://www.bigo.tv.\n3. Copy the Value of uniqid and paste it above. Validate, then Save settings.").FontSize(12).TextColor(t.TextMuted).Selectable()
			ui.Row(c).FillWidth().Gap(8).AlignItems(ui.Start).TextColor(t.Danger).Children(func() {
				ui.Icon(c, warningIcon).Size(16, 16).Shrink(0).Label("BIGO account warning")
				ui.Text(c, "Use this account in only one native listener at a time. Another app or browser connected with this account can disconnect this app.\nMake sure live notifications are enabled in your BIGO account settings.").Grow(1).MinWidth(0).FontSize(11)
			})
		})
		if a.accountSettingsRequested {
			accountSection.ScrollIntoView()
		}
		a.pollingQueue(c)
		section(c, "Recording", "The original audio and video are copied, not re-encoded. Short MKV clusters limit damage if the process crashes.", func() {
			ui.Row(c).FillWidth().Gap(10).Children(func() {
				ui.TextInput(c, &a.cfg.RecordingPath).Grow(1).Label("Recording folder").Height(34)
				if ui.Button(c, "Choose folder…").Clicked() {
					a.choosePath(true, &a.cfg.RecordingPath)
				}
			})
			toggle(c, "Separate folder for each streamer", "Folder names start with the BIGO ID, followed by the display name.", &a.cfg.FolderPerStreamer)
			ui.Row(c).FillWidth().AlignItems(ui.Center).Children(func() {
				ui.Text(c, "Stream quality").Grow(1)
				quality := "Origin (native)"
				if a.cfg.StreamQuality == "auto" {
					quality = "Web (automatic)"
				}
				if ui.Select(c, &quality, []string{"Origin (native)", "Web (automatic)"}).Width(170).Label("Stream quality").Disabled(!a.cfg.NativeListening).Changed() {
					a.cfg.StreamQuality = "origin"
					if quality == "Web (automatic)" {
						a.cfg.StreamQuality = "auto"
					}
				}
			})
			ui.Text(c, "Origin uses native live notifications and falls back to the web stream if native media cannot start. With native notifications disabled, recordings use the web stream.").FontSize(11).TextColor(t.TextMuted).MaxLines(3)
			toggle(c, "Create a processed copy with FFmpeg", "Runs after the recording ends. The original MKV is always kept, even if processing fails.", &a.cfg.PostProcess)
			ui.Column(c).FillWidth().Gap(10).Disabled(!a.cfg.PostProcess).Children(func() {
				ui.Row(c).FillWidth().AlignItems(ui.Center).Children(func() {
					ui.Text(c, "Processed copy format").Grow(1)
					ui.Select(c, &a.cfg.OutputExtension, []string{"mkv", "mp4", "ts"}).Width(130).Label("Processed copy format")
				})
				ui.TextInput(c, &a.cfg.FFmpegArgs).FillWidth().Height(36).Label("FFmpeg output options").Placeholder("-map 0:v? -map 0:a? -c copy")
				ui.Text(c, "Output options only. The app supplies the input and output paths; no command name, -i, or placeholders. Example for MP4: -c copy -movflags +faststart").FontSize(11).TextColor(t.TextMuted).MaxLines(3)
			})
			ui.Row(c).FillWidth().Gap(10).Children(func() {
				if ui.TextInput(c, &a.cfg.FFmpegPath).Placeholder("ffmpeg (from PATH)").Grow(1).Height(34).Label("FFmpeg executable").Disabled(a.ffmpegInstalling).Changed() {
					a.ffmpeg = false
				}
				if ui.Button(c, "Choose FFmpeg…").Disabled(a.ffmpegInstalling || a.preview).Clicked() {
					a.choosePath(false, &a.cfg.FFmpegPath)
				}
			})
			ui.Text(c, "Leave blank to use FFmpeg from PATH or an automatically detected installation. Guided setup fills in its installed path.").FontSize(11).TextColor(t.TextMuted).MaxLines(3)
			ui.Row(c).AlignItems(ui.Center).FillWidth().Gap(12).Children(func() {
				label := "FFmpeg is ready"
				color := t.Success
				if !a.ffmpeg {
					label = "FFmpeg is required to record"
					color = t.Warning
				}
				if a.ffmpegChecking {
					label = "Checking FFmpeg…"
				}
				ui.Text(c, label).FontSize(12).TextColor(color).Grow(1)
				if !a.ffmpeg && ui.Button(c, "Install FFmpeg…").Disabled(a.ffmpegChecking || a.preview).Clicked() {
					a.ffmpegOpen = true
				}
				if ui.Button(c, "Check again").Disabled(a.ffmpegChecking || a.ffmpegInstalling || a.preview).Clicked() {
					a.checkFFmpeg(false)
				}
			})
		})
		section(c, "Application", "Appearance, desktop notifications, and local files.", func() {
			ui.Row(c).FillWidth().AlignItems(ui.Center).Children(func() {
				ui.Text(c, "Appearance").Grow(1)
				ui.Select(c, &a.cfg.Theme, []string{"system", "dark", "light"}).Width(150).Label("Appearance")
			})
			toggle(c, "Notify when recording starts", "", &a.cfg.Notifications)
			ui.Text(c, "Config, data, and circular logs: "+a.store.Directory()).FontSize(11).TextColor(t.TextMuted).Selectable()
			ui.Row(c).Gap(10).Children(func() {
				if ui.Button(c, "Open configuration folder").Clicked() {
					a.openFolder(a.store.Directory())
				}
				if ui.Button(c, "Open recordings").Clicked() {
					a.openFolder(a.cfg.RecordingPath)
				}
			})
		})
	})
}

func (a *application) dialogs(c *ui.Context) {
	a.ffmpegDialog(c)
	a.recordingHistoryDialog(c)
	ui.Modal(c, &a.addOpen, func() {
		ui.Text(c, "Add a streamer").FontSize(22).FontWeight(600)
		ui.Text(c, "Paste a BIGO profile URL, or enter an ID or alias.").TextColor(c.Theme().TextMuted)
		ui.TextInput(c, &a.addID).Placeholder("BIGO ID or https://www.bigo.tv/…").Label("New streamer ID").Width(400)
		ui.TextInput(c, &a.addName).Placeholder("Custom name (optional)").Label("Custom name").Width(400)
		ui.Row(c).Justify(ui.End).Gap(10).Children(func() {
			if ui.Button(c, "Cancel").Clicked() {
				a.addOpen = false
			}
			if ui.PrimaryButton(c, "Add streamer").Disabled(a.busy || a.preview).Clicked() {
				id, name := a.addID, a.addName
				a.work(func() error {
					err := a.engine.Add(id, name)
					if err == nil {
						a.update(func() { a.addOpen = false; a.addID = ""; a.addName = "" })
					}
					return err
				})
			}
		})
	})
	ui.Modal(c, &a.renameOpen, func() {
		ui.Text(c, "Rename streamer").FontSize(22).FontWeight(600)
		ui.TextInput(c, &a.renameName).Placeholder("Leave empty to use BIGO's name").Label("Streamer name").Width(400)
		ui.Row(c).Justify(ui.End).Gap(10).Children(func() {
			if ui.Button(c, "Cancel").Clicked() {
				a.renameOpen = false
			}
			if ui.PrimaryButton(c, "Rename").Disabled(a.preview).Clicked() {
				id, name := a.renameID, strings.TrimSpace(a.renameName)
				a.work(func() error { return a.engine.Edit(id, func(s *state.Streamer) { s.CustomName = name }) })
				a.renameOpen = false
			}
		})
	})
	ui.Modal(c, &a.removeOpen, func() {
		ui.Text(c, "Remove this streamer?").FontSize(22).FontWeight(600)
		ui.Text(c, "Monitoring stops and any active recording is finalized.\nExisting recording files are kept.").Width(400)
		ui.Row(c).Justify(ui.End).Gap(10).Children(func() {
			if ui.Button(c, "Cancel").Clicked() {
				a.removeOpen = false
			}
			if ui.PrimaryButton(c, "Remove streamer").Clicked() {
				id := a.removeID
				a.work(func() error {
					if err := a.engine.Remove(id); err != nil {
						return err
					}
					a.update(func() { a.toasts = append(a.toasts, "Streamer removed") })
					return nil
				})
				a.removeOpen = false
			}
		})
	})
}
