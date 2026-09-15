package log

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"

	"github.com/Abhijeet34/foliot/internal/testenv"
)

func TestFoldBenchState(t *testing.T) {
	root := testenv.Isolate(t)
	l, err := Open(root, clock(time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	for _, e := range []Entry{
		run("v1", "t1", "defect", "small", 1),
		run("v1", "t1", "defect", "small", 2),
		run("v1", "t2", "defect", "small", 1),
		run("v1", "t1", "feature", "large", 1),
		verdict("v1", "t1", "small", 1, false, 0.9),
		verdict("v1", "t1", "small", 1, true, 1.0), // replaces the verdict above
		verdict("v1", "t1", "small", 2, false, 3.0),
		verdict("v1", "t2", "small", 1, true, 2.0),
		verdict("v1", "t1", "large", 1, true, nil),
		verdict("v1", "never-ran", "small", 1, true, 5.0), // no run: skipped
	} {
		if _, err := l.Append(e); err != nil {
			t.Fatal(err)
		}
	}
	evs, err := Read(Path(root), 1)
	if err != nil {
		t.Fatal(err)
	}
	st := Fold(evs)
	got, _ := json.Marshal(st)
	want := `{"last_seq":10,"bench":{"defect":{"small":{"pass_rate":0.6666666666666666,"n":3,"cost_median":2}},"feature":{"large":{"pass_rate":1,"n":1,"cost_median":null}}}}`
	if string(got) != want {
		t.Fatalf("state\n%s\nwant\n%s", got, want)
	}
	// K1: the same events fold to the same bytes, in any number of passes.
	again, _ := json.Marshal(Fold(evs))
	if !bytes.Equal(got, again) {
		t.Fatal("two folds of one log differ")
	}
	// An event a log.quarantined names is skipped even when it reads as valid.
	q := Event{Seq: 11, Type: "log.quarantined", Actor: "orchestrator", Evidence: []Evidence{}, Data: json.RawMessage(`{"seq":8,"reason":"test"}`), V: 1}
	skipped, _ := json.Marshal(Fold(append(evs, q)))
	if want := `{"last_seq":11,"bench":{"defect":{"small":{"pass_rate":0.5,"n":2,"cost_median":2}},"feature":{"large":{"pass_rate":1,"n":1,"cost_median":null}}}}`; string(skipped) != want {
		t.Fatalf("state with seq 8 quarantined\n%s\nwant\n%s", skipped, want)
	}
}

func TestRunsRestartsARunAndDropsItsOldVerdict(t *testing.T) {
	root := testenv.Isolate(t)
	l, err := Open(root, clock(time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	for _, e := range []Entry{
		run("v1", "t1", "defect", "small", 1),
		verdict("v1", "t1", "small", 1, true, 1.0),
		run("v1", "t1", "defect", "small", 1), // the same run started again
		run("v1", "t2", "defect", "small", 1),
		verdict("v1", "t2", "small", 1, false, 2.0),
	} {
		if _, err := l.Append(e); err != nil {
			t.Fatal(err)
		}
	}
	evs, _ := Read(Path(root), 1)
	runs := Runs(evs)
	if len(runs) != 2 || runs[0].Seq != 3 || runs[0].Verdict != nil || runs[1].Seq != 4 || runs[1].Verdict == nil || runs[1].Verdict.Pass {
		t.Fatalf("runs %+v", runs)
	}
}

func TestRecordsWrittenBeforeTheClockStillFold(t *testing.T) {
	testenv.Isolate(t)
	old := []Event{
		{Seq: 1, Type: "bench.verified", Actor: "bench", Evidence: []Evidence{}, V: 1,
			Data: json.RawMessage(`{"corpus":"v1","corpus_sha":"c","task":"t1","task_sha":"s","base_exit":1,"landed_exit":0,"examined":3}`)},
		{Seq: 2, Type: "bench.run", Actor: "bench", Evidence: []Evidence{}, V: 1, Data: func() json.RawMessage {
			var m map[string]any
			b, _ := json.Marshal(run("v1", "t1", "defect", "small", 1).Data)
			json.Unmarshal(b, &m)
			delete(m, "clock_at")
			delete(m, "clock_offset_ms")
			b, _ = json.Marshal(m)
			return b
		}()},
	}
	if v, ok := Verified(old)[[3]string{"v1", "t1", "s"}]; !ok || v.ClockAt != "" || v.Examined != 3 {
		t.Fatalf("a bench.verified with no clock: %+v %t", v, ok)
	}
	if runs := Runs(old); len(runs) != 1 || runs[0].Run.ClockAt != "" {
		t.Fatalf("a bench.run with no clock: %+v", runs)
	}
}
