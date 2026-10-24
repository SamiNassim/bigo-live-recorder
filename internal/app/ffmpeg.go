package app

import (
	"context"
	"fmt"

	"bigo-live-recorder/internal/ffmpeg"
	"github.com/egoist/mygo/ui"
)

func (a *application) checkFFmpeg(prompt bool) {
	if a.ffmpegChecking || a.ffmpegInstalling {
		return
	}
	a.ffmpegChecking = true
	configured := a.store.Config().FFmpegPath
	go func() {
		_, err := ffmpeg.Check(a.ctx, configured)
		a.update(func() {
			a.ffmpegChecking = false
			a.ffmpeg = err == nil
			if err != nil && prompt {
				a.ffmpegOpen = true
			}
			if err == nil && !prompt {
				a.toasts = append(a.toasts, "FFmpeg is ready")
			}
		})
	}()
}

func (a *application) installFFmpeg() {
	if a.ffmpegInstalling {
		return
	}
	ctx, cancel := context.WithCancel(a.ctx)
	a.ffmpegCancel = cancel
	a.ffmpegInstalling = true
	a.ffmpegError = ""
	a.ffmpegProgress = "Starting download…"
	go func() {
		defer cancel()
		_, err := ffmpeg.Install(ctx, func(p ffmpeg.Progress) {
			text := p.Stage
			if p.Received > 0 {
				text = fmt.Sprintf("Downloading FFmpeg… %.1f MB", float64(p.Received)/(1<<20))
				if p.Total > 0 {
					text += fmt.Sprintf(" / %.1f MB", float64(p.Total)/(1<<20))
				}
			}
			a.update(func() { a.ffmpegProgress = text })
		})
		if err == nil {
			// An old custom path must not hide the newly installed private copy.
			cfg := a.store.Config()
			cfg.FFmpegPath = ""
			err = a.store.SaveConfig(cfg)
		}
		canceled := ctx.Err() != nil
		a.update(func() {
			a.ffmpegInstalling = false
			a.ffmpegCancel = nil
			a.ffmpegProgress = ""
			if canceled {
				a.ffmpegError = "Download canceled. You can retry whenever you’re ready."
				return
			}
			if err != nil {
				a.ffmpegError = err.Error()
				return
			}
			a.cfg.FFmpegPath = ""
			a.ffmpeg = true
			a.ffmpegOpen = false
			a.toasts = append(a.toasts, "FFmpeg installed. You’re ready to record.")
			a.log.Write("INFO", "setup", "FFmpeg installed in the per-user application directory")
		})
	}()
}

func (a *application) ffmpegDialog(c *ui.Context) {
	ui.Modal(c, &a.ffmpegOpen, func() {
		ui.Text(c, "Get ready to record").FontSize(22).FontWeight(600)
		ui.Text(c, "FFmpeg is needed to save your recordings. Download it now, or install it yourself and choose Check again in Settings.").Width(450).MaxLines(4)
		provider, sourceErr := ffmpeg.DownloadProvider()
		if sourceErr == nil {
			ui.Text(c, "Downloads a third-party FFmpeg build from "+provider+" and verifies its checksum. Installs only for your user account; no administrator access or PATH changes.").Width(450).FontSize(12).TextColor(c.Theme().TextMuted).MaxLines(5)
		} else {
			ui.Text(c, sourceErr.Error()).Width(450).TextColor(c.Theme().Warning).MaxLines(4)
		}
		if a.ffmpegProgress != "" {
			ui.Text(c, a.ffmpegProgress).TextColor(c.Theme().Accent)
		}
		if a.ffmpegError != "" {
			ui.Text(c, a.ffmpegError).Width(450).TextColor(c.Theme().Danger).MaxLines(5)
		}
		ui.Row(c).FillWidth().Justify(ui.End).Gap(10).Children(func() {
			if a.ffmpegInstalling {
				if ui.Button(c, "Cancel download").Clicked() && a.ffmpegCancel != nil {
					a.ffmpegCancel()
				}
				return
			}
			if ui.Button(c, "Later").Clicked() {
				a.ffmpegOpen = false
			}
			if ui.PrimaryButton(c, "Download and install").Disabled(sourceErr != nil || a.preview).Clicked() {
				a.installFFmpeg()
			}
		})
	})
}
