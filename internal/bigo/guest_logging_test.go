package bigo

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"bigo-live-recorder/internal/fault"
)

type guestLogEntry struct {
	level, message string
	cause          error
}

func TestGuestReconnectNoticesCoalesceRetriesAndRetainBoundedDiagnostics(t *testing.T) {
	var entries []guestLogEntry
	l := guestReconnectLog{track: "video", recovery: StreamRecovery{ReportError: func(level, message string, cause error) {
		entries = append(entries, guestLogEntry{level, message, cause})
	}}}
	start := time.Unix(1000, 0)
	l.resumed(start)
	first := errors.New("synthetic failure 001")
	l.failed(start, first)
	for i := 2; i <= 20; i++ {
		l.failed(start.Add(time.Duration(i)*time.Second), fmt.Errorf("synthetic failure %03d", i))
	}
	if len(entries) != 1 || entries[0].level != "INFO" || entries[0].message != "Room video connection reconnecting." {
		t.Fatal("brief retries produced duplicate notices or warnings", entries)
	}
	l.waiting(start.Add(time.Minute - time.Nanosecond))
	if len(entries) != 1 {
		t.Fatal("warned before the interruption persisted")
	}
	l.waiting(start.Add(time.Minute))
	for i := 21; i <= 40; i++ {
		l.failed(start.Add(time.Minute+time.Duration(i)*time.Second), fmt.Errorf("synthetic failure %03d", i))
	}
	if len(entries) != 2 || entries[1].level != "WARN" {
		t.Fatal("persistent retries did not produce exactly one warning", entries)
	}
	l.resumed(start.Add(2 * time.Minute))
	if len(entries) != 3 || entries[2].level != "INFO" || entries[2].message != "Room video reception resumed after 2m0s." {
		t.Fatal("missing media recovery summary", entries)
	}
	details := fault.Details(entries[2].cause)
	if !errors.Is(entries[2].cause, first) || !strings.Contains(details, "40 failures") || !strings.Contains(details, "synthetic failure 040") || strings.Count(details, "synthetic failure") != 8 {
		t.Fatal("recovery lost its first/latest failures or retained unbounded diagnostics", details)
	}
	if strings.Contains(fault.Details(entries[0].cause), "synthetic failure 040") {
		t.Fatal("later retries mutated an earlier notice")
	}
	l.failed(start.Add(3*time.Minute), errors.New("another interruption"))
	if len(entries) != 4 || entries[3].level != "INFO" || strings.Contains(fault.Details(entries[3].cause), "synthetic failure") {
		t.Fatal("a separate interruption inherited old diagnostics or severity", entries)
	}
}

func TestGuestReconnectKeepsUnusedVideoQuietAndReportsAudioStartup(t *testing.T) {
	for _, track := range []string{"video", "audio"} {
		t.Run(track, func(t *testing.T) {
			var entries []guestLogEntry
			l := guestReconnectLog{track: track, enabled: track == "audio", recovery: StreamRecovery{ReportError: func(level, message string, cause error) {
				entries = append(entries, guestLogEntry{level, message, cause})
			}}}
			start := time.Unix(1000, 0)
			cause := errors.New("synthetic connection failure")
			l.failed(start, cause)
			l.waiting(start.Add(time.Hour))
			if track == "video" {
				if len(entries) != 0 || !errors.Is(l.cause(cause), cause) {
					t.Fatal("unused cameras created notices or lost the final failure", entries)
				}
			} else if len(entries) != 2 || entries[0].level != "INFO" || entries[1].level != "WARN" {
				t.Fatal("audio startup failures were hidden", entries)
			}
		})
	}
}

func TestGuestReconnectRequiresMediaRatherThanSocketAcknowledgement(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	const viewer, owner = 900000001, 900000002
	source := Stream{UID: viewer, native: &pJoinChannelRes{source: viewer, sid: 42, cookie: []byte("synthetic-ticket"), video: []mediaProxy{{ip: 0x0100007f, tcp: []uint16{uint16(listener.Addr().(*net.TCPAddr).Port)}}}}}
	r := &guestRoom{ctx: ctx, epoch: time.Now(), frames: make(chan guestFrame, 4)}
	var entries []guestLogEntry
	l := guestReconnectLog{track: "video", enabled: true, recovery: StreamRecovery{ReportError: func(level, message string, cause error) {
		entries = append(entries, guestLogEntry{level, message, cause})
	}}}
	l.failed(time.Now(), errors.New("synthetic interruption"))
	for attempt := range 3 {
		serverResult := make(chan error, 1)
		go func() {
			serverResult <- func() error {
				conn, err := listener.Accept()
				if err != nil {
					return err
				}
				defer conn.Close()
				conn.SetDeadline(time.Now().Add(5 * time.Second))
				if _, err = readNativePacket(conn); err != nil {
					return err
				}
				ack := binary.LittleEndian.AppendUint32(nil, viewer)
				ack = binary.LittleEndian.AppendUint64(ack, 0)
				if err = writeNativeTestPacket(conn, 0x5a02, ack); err != nil || attempt == 0 {
					return err
				}
				part := nativeTestVideo(100, true)
				part.owner = owner
				if attempt == 1 {
					part, _ = splitNativeTestVideo(part)
				}
				return writeNativeTestVideo(conn, part)
			}()
		}()
		err := r.receiveTrack(source, true, attempt, &l)
		if serverErr := <-serverResult; serverErr != nil || err == nil {
			t.Fatal("synthetic video connection failed", serverErr, err)
		}
		if attempt < 2 {
			l.failed(time.Now(), err)
			if len(entries) != 1 {
				t.Fatal("acknowledgement or incomplete frame ended the interruption", entries)
			}
		}
	}
	if len(entries) != 2 || entries[1].level != "INFO" || !strings.Contains(entries[1].message, "Room video reception resumed") || len(r.frames) != 1 {
		t.Fatal("complete media did not end the interruption exactly once", entries, len(r.frames))
	}
}

func TestGuestReconnectExhaustionRetainsRoomFailureAndRefreshDiagnostics(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		var entries []guestLogEntry
		refreshes := 0
		r := &guestRoom{ctx: ctx, cancel: cancel, source: Stream{native: &pJoinChannelRes{}}, recovery: StreamRecovery{
			Refresh: func(context.Context) (Stream, error) {
				refreshes++
				return Stream{}, fmt.Errorf("synthetic ticket refresh failure %d", refreshes)
			},
			ReportError: func(level, message string, cause error) {
				entries = append(entries, guestLogEntry{level, message, cause})
			},
		}}
		r.transport(false)
		if ctx.Err() == nil || r.err == nil || refreshes != 2 || !strings.Contains(fault.Reason(r.err), "room's shared audio connection after 3 attempts") {
			t.Fatal("failed recovery lost its bounds or room-level explanation", r.err, refreshes)
		}
		if len(entries) != 1 || entries[0].level != "INFO" {
			t.Fatal("retry exhaustion spammed notices", entries)
		}
		details := fault.Details(r.err)
		if !strings.Contains(details, "no UDP route") || !strings.Contains(details, "synthetic ticket refresh failure 1") || !strings.Contains(details, "synthetic ticket refresh failure 2") {
			t.Fatal("final failure lost connection or ticket refresh diagnostics", details)
		}
	})
}
