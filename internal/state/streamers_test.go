package state

import "testing"

func TestRecordingAndMonitoringPausePersistIndependently(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Update(func(d *Data) error {
		d.Streamers = []Streamer{{ID: "recording-paused", RecordingPaused: true}, {ID: "monitoring-paused", Paused: true}, {ID: "both-paused", Paused: true, RecordingPaused: true}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(s.Directory())
	if err != nil {
		t.Fatal(err)
	}
	rows := reopened.Data().Streamers
	if !rows[0].RecordingPaused || rows[0].Paused || rows[1].RecordingPaused || !rows[1].Paused || !rows[2].RecordingPaused || !rows[2].Paused {
		t.Fatal("pause preferences were conflated or lost", rows)
	}
}
