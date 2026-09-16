package log

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/Abhijeet34/foliot/internal/testenv"
)

func TestMain(m *testing.M) {
	if os.Getenv(helperEnv) != "" {
		helper()
		return
	}
	os.Exit(testenv.Main(m))
}

// clock returns a now function that starts at a fixed instant and moves by step
// on every call; a negative step is a clock going backwards.
func clock(step time.Duration) func() time.Time {
	t := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	var mu sync.Mutex
	return func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		t = t.Add(step)
		return t
	}
}

func run(corpus, task, class, model string, repeat int) Entry {
	return Entry{Type: "bench.run", Actor: "bench", Data: BenchRun{
		Corpus: corpus, Task: task, Class: class, Arm: "bare-" + model, Adapter: "fake", Gate: "none",
		Repeat: repeat, Model: model, Provider: "p", Billing: "subscription", BaseSHA: "abc", BaseExit: 1, LandedExit: 0, Benchmark: true,
	}}
}

func verdict(corpus, task, model string, repeat int, pass bool, cost any) Entry {
	cols := map[string]any{}
	if cost != nil {
		cols["cost_usd"] = cost
	}
	return Entry{Type: "bench.verdict", Actor: "bench", Data: BenchVerdict{
		Corpus: corpus, Task: task, Arm: "bare-" + model, Repeat: repeat, Pass: pass, Columns: cols,
	}}
}

func open(t *testing.T, root string) *Log {
	t.Helper()
	l, err := Open(root, clock(time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	return l
}

func mustAppend(t *testing.T, l *Log, e Entry) int64 {
	t.Helper()
	seq, err := l.Append(e)
	if err != nil {
		t.Fatal(err)
	}
	return seq
}

func readAll(t *testing.T, root string) []Event {
	t.Helper()
	evs, err := Read(Path(root), 1)
	if err != nil {
		t.Fatal(err)
	}
	return evs
}

func types(evs []Event) string {
	var s []string
	for _, e := range evs {
		s = append(s, fmt.Sprintf("%d:%s", e.Seq, e.Type))
	}
	return strings.Join(s, " ")
}

func TestRoot(t *testing.T) {
	testenv.Isolate(t)
	for _, c := range []struct{ env, want string }{
		{"", ""}, {"relative/home", ""}, {"/abs/./home/", "/abs/home"},
	} {
		got, err := Root(func(string) string { return c.env })
		if got != c.want || (c.want == "") != errors.Is(err, ErrNoRoot) {
			t.Errorf("Root(FOLIOT_HOME=%q) = %q, %v; want %q", c.env, got, err, c.want)
		}
	}
}

func TestAppendWritesTheEnvelopeInOrder(t *testing.T) {
	root := testenv.Isolate(t)
	l := open(t, root)
	task := "log-core-l1"
	// seq 1 and the log's first line are this process's home.opened.
	if seq := mustAppend(t, l, run("v1", "t1", "defect", "m", 1)); seq != 2 {
		t.Fatalf("first appended seq %d, want 2", seq)
	}
	e := verdict("v1", "t1", "m", 1, true, 0.5)
	e.Task, e.Evidence = &task, []Evidence{{Kind: "file", Ref: "bench/checks/t1/check.sh"}}
	cause := int64(2)
	e.Cause = &cause
	if seq := mustAppend(t, l, e); seq != 3 {
		t.Fatalf("second appended seq %d, want 3", seq)
	}
	data, _ := os.ReadFile(Path(root))
	lines := strings.Split(string(data), "\n")
	want := `{"seq":3,"ts":"2026-09-14T12:00:00.004Z","type":"bench.verdict","task":"log-core-l1","attempt":null,"actor":"bench","cause":2,"evidence":[{"kind":"file","ref":"bench/checks/t1/check.sh"}],"data":{"corpus":"v1","task":"t1","arm":"bare-m","repeat":1,"pass":true,"false_claim":false,"columns":{"cost_usd":0.5},"check_exit":0,"examined":0,"check_confined":false},"v":1}`
	if len(lines) != 4 || lines[3] != "" || lines[2] != want {
		t.Fatalf("log is\n%s\nwant line 3\n%s", data, want)
	}
	if !strings.Contains(lines[1], `"evidence":[]`) {
		t.Fatalf("an event with no evidence must carry an empty array: %s", lines[1])
	}
}

func TestCatalogueRefusesAndConsumesNoSeq(t *testing.T) {
	root := testenv.Isolate(t)
	l := open(t, root)
	bad := "Not_A_Task"
	cause := int64(7)
	big := run("v1", "t1", "defect", strings.Repeat("m", MaxEventBytes), 1)
	missing := Entry{Type: "bench.run", Actor: "bench", Data: map[string]any{"corpus": "v1"}}
	for name, e := range map[string]Entry{
		"unknown type":       {Type: "task.dispatched", Actor: "orchestrator", Data: map[string]any{}},
		"wrong actor":        {Type: "bench.run", Actor: "orchestrator", Data: run("v1", "t", "c", "m", 1).Data},
		"missing field":      missing,
		"data not an object": {Type: "bench.run", Actor: "bench", Data: []int{1}},
		"null data":          {Type: "bench.run", Actor: "bench"},
		"bad task id":        {Type: "bench.run", Actor: "bench", Task: &bad, Data: run("v1", "t", "c", "m", 1).Data},
		"bad evidence kind":  {Type: "bench.run", Actor: "bench", Evidence: []Evidence{{Kind: "blob", Ref: "x"}}, Data: run("v1", "t", "c", "m", 1).Data},
		"multiline ref":      {Type: "bench.run", Actor: "bench", Evidence: []Evidence{{Kind: "file", Ref: "a\nb"}}, Data: run("v1", "t", "c", "m", 1).Data},
		"cause not earlier":  {Type: "bench.run", Actor: "bench", Cause: &cause, Data: run("v1", "t", "c", "m", 1).Data},
		"enormous event":     big,
	} {
		if _, err := l.Append(e); err == nil || !strings.HasPrefix(err.Error(), "refused ") {
			t.Errorf("%s: err = %v, want a refusal", name, err)
		}
	}
	if seq := mustAppend(t, l, run("v1", "t1", "defect", "m", 1)); seq != 2 {
		t.Fatalf("seq after ten refusals is %d, want 2 (home.opened took seq 1)", seq)
	}
	if fi, _ := os.Stat(Path(root)); fi.Size() > 1024 {
		t.Fatalf("a refused event reached the file: %d bytes", fi.Size())
	}
}

func TestTornLastLineIsCutMovedAndRecorded(t *testing.T) {
	root := testenv.Isolate(t)
	l := open(t, root)
	mustAppend(t, l, run("v1", "t1", "defect", "m", 1))
	mustAppend(t, l, run("v1", "t2", "defect", "m", 1))
	l.Close()
	full, _ := os.ReadFile(Path(root))
	lastLine := bytes.LastIndex(full[:len(full)-1], []byte("\n")) + 1
	torn := full[:len(full)-9] // the last line loses its last nine bytes, newline included
	os.WriteFile(Path(root), torn, 0o600)

	if err := Verify(Path(root)); err == nil || !strings.Contains(err.Error(), "log seq 4: torn last line, ") {
		t.Fatalf("Verify before repair = %v, want the torn line named at seq 4", err)
	}
	l = open(t, root)
	evs := readAll(t, root)
	if got := types(evs); got != "1:home.opened 2:bench.run 3:bench.run 4:log.repaired 5:home.opened" {
		t.Fatalf("events after repair: %s", got)
	}
	var rep struct {
		DroppedBytes int    `json:"dropped_bytes"`
		MovedTo      string `json:"moved_to"`
	}
	json.Unmarshal(evs[3].Data, &rep)
	moved, err := os.ReadFile(filepath.Join(root, rep.MovedTo))
	if err != nil || !bytes.Equal(moved, torn[lastLine:]) || rep.DroppedBytes != len(moved) {
		t.Fatalf("moved_to %s holds %q (err %v), dropped_bytes %d; want the %d torn bytes", rep.MovedTo, moved, err, rep.DroppedBytes, len(torn)-lastLine)
	}
	now, _ := os.ReadFile(Path(root))
	if !bytes.HasPrefix(now, full[:lastLine]) {
		t.Fatal("the last complete event was not left intact")
	}
	if err := Verify(Path(root)); err != nil {
		t.Fatalf("Verify after repair: %v", err)
	}
	if seq := mustAppend(t, l, run("v1", "t2", "defect", "m", 1)); seq != 6 {
		t.Fatalf("next seq %d, want 6", seq)
	}
}

func TestGarbageLineIsQuarantinedOnceAndNeverEdited(t *testing.T) {
	root := testenv.Isolate(t)
	l := open(t, root)
	for i := range 3 {
		mustAppend(t, l, run("v1", fmt.Sprint("t", i), "defect", "m", 1))
	}
	l.Close()
	data, _ := os.ReadFile(Path(root))
	lines := bytes.SplitAfter(data, []byte("\n"))
	copy(lines[1][10:30], bytes.Repeat([]byte{'#'}, 20)) // overwrite bytes inside seq 2's line
	corrupt := bytes.Join(lines, nil)
	os.WriteFile(Path(root), corrupt, 0o600)

	if err := Verify(Path(root)); err == nil || !strings.Contains(err.Error(), "log seq 2: not quarantined: unparseable") {
		t.Fatalf("Verify = %v, want seq 2 named as not quarantined", err)
	}
	l = open(t, root)
	l.Close()
	l = open(t, root) // a second open must not quarantine the same line again
	after, _ := os.ReadFile(Path(root))
	if !bytes.HasPrefix(after, corrupt) {
		t.Fatal("the corrupt line or its neighbours were edited")
	}
	evs := readAll(t, root)
	if got := types(evs); got != "1:home.opened 3:bench.run 4:bench.run 5:home.closed 6:log.quarantined 7:home.opened 8:home.closed 9:home.opened" {
		t.Fatalf("events: %s", got)
	}
	if !strings.HasPrefix(string(evs[4].Data), `{"reason":"unparseable: `) || !strings.HasSuffix(string(evs[4].Data), `,"seq":2}`) {
		t.Fatalf("quarantine data %s", evs[4].Data)
	}
	if err := Verify(Path(root)); err != nil {
		t.Fatalf("Verify after quarantine: %v", err)
	}
	if seq := mustAppend(t, l, run("v1", "t9", "defect", "m", 1)); seq != 10 {
		t.Fatalf("next seq %d, want 10", seq)
	}
}

func TestCorruptedSeqOrLastLineIsQuarantinedNotFatal(t *testing.T) {
	for name, c := range map[string]struct {
		corrupt func(lines [][]byte)
		want    string
	}{
		"seq digit overwritten": {func(l [][]byte) { copy(l[1], `{"seq":9`) }, `{"reason":"seq 9 where seq 2 belongs","seq":2}`},
		"seq duplicated":        {func(l [][]byte) { copy(l[1], `{"seq":1`) }, `{"reason":"seq 1 where seq 2 belongs","seq":2}`},
		"last complete line":    {func(l [][]byte) { copy(l[2], `garbage!`) }, `{"reason":"unparseable: invalid character 'g' looking for beginning of value","seq":3}`},
	} {
		t.Run(name, func(t *testing.T) {
			root := testenv.Isolate(t)
			l := open(t, root)
			for i := range 3 {
				mustAppend(t, l, run("v1", fmt.Sprint("t", i), "defect", "m", 1))
			}
			l.Close()
			data, _ := os.ReadFile(Path(root))
			lines := bytes.SplitAfter(data, []byte("\n"))
			c.corrupt(lines)
			os.WriteFile(Path(root), bytes.Join(lines, nil), 0o600)
			open(t, root)
			evs := readAll(t, root)
			q := evs[len(evs)-2] // the open that quarantined it announces itself after
			if q.Type != "log.quarantined" || q.Seq != 6 || string(q.Data) != c.want {
				t.Fatalf("events %s, quarantine data %s; want seq 6 quarantining %s", types(evs), q.Data, c.want)
			}
			if err := Verify(Path(root)); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestInvalidKnownEventIsQuarantinedAndSkippedByTheFold(t *testing.T) {
	root := testenv.Isolate(t)
	line := `{"seq":1,"ts":"2026-09-14T12:00:00.000Z","type":"bench.run","task":null,"attempt":null,"actor":"bench","cause":null,"evidence":[],"data":{"corpus":"v1"},"v":1}` + "\n" +
		`{"seq":2,"ts":"2026-09-14T12:00:00.000Z","type":"task.from.a.newer.binary","task":null,"attempt":null,"actor":"orchestrator","cause":null,"evidence":[],"data":{},"v":1}` + "\n"
	os.MkdirAll(filepath.Join(root, "log"), 0o700)
	os.WriteFile(Path(root), []byte(line), 0o600)
	open(t, root)
	evs := readAll(t, root)
	if got := types(evs); got != "3:log.quarantined 4:home.opened" {
		t.Fatalf("events: %s, want only the quarantine of seq 1 (the unknown type is skipped, not quarantined)", got)
	}
	if !strings.Contains(string(evs[0].Data), `missing required data field \"corpus_sha\"`) {
		t.Fatalf("quarantine reason %s", evs[0].Data)
	}
}

func TestGapAndNewerVersionAreRefusedWithoutEditing(t *testing.T) {
	root := testenv.Isolate(t)
	l := open(t, root)
	for i := range 3 {
		mustAppend(t, l, run("v1", fmt.Sprint("t", i), "defect", "m", 1))
	}
	l.Close()
	data, _ := os.ReadFile(Path(root))
	lines := bytes.SplitAfter(data, []byte("\n"))
	for name, c := range map[string]struct {
		log  []byte
		want string
	}{
		"deleted line": {append(append([]byte{}, lines[0]...), lines[2]...), "log seq 2: expected seq 2, found 3 after seq 1 and 0 unreadable lines"},
		"reordered":    {bytes.Join([][]byte{lines[1], lines[0], lines[2]}, nil), "log seq 1: expected seq 1, found 2"},
		"newer v":      {bytes.Replace(data, []byte(`"v":1}`+"\n"+`{"seq":3`), []byte(`"v":2}`+"\n"+`{"seq":3`), 1), "envelope version 2 is newer"},
	} {
		os.WriteFile(Path(root), c.log, 0o600)
		_, err := Open(root, clock(time.Millisecond))
		var ve *VerifyError
		if !errors.As(err, &ve) || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: Open = %v, want %q", name, err, c.want)
		}
		if after, _ := os.ReadFile(Path(root)); !bytes.Equal(after, c.log) {
			t.Errorf("%s: the refused log was edited", name)
		}
		if err := Verify(Path(root)); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: Verify = %v, want %q", name, err, c.want)
		}
	}
}

func TestConcurrentAppendsInterleaveNothingAndLoseNothing(t *testing.T) {
	root := testenv.Isolate(t)
	l := open(t, root)
	const writers, each = 32, 250
	var wg sync.WaitGroup
	seqs := make(chan int64, writers*each)
	for w := range writers {
		wg.Go(func() {
			for i := range each {
				// Different sizes make an interleaved write visible as a broken line.
				seqs <- mustAppend(t, l, run("v1", fmt.Sprintf("w%d-%d-%s", w, i, strings.Repeat("x", i)), "defect", "m", 1))
			}
		})
	}
	wg.Wait()
	close(seqs)
	returned := map[int64]bool{}
	for s := range seqs {
		returned[s] = true
	}
	data, _ := os.ReadFile(Path(root))
	lines := bytes.Split(bytes.TrimSuffix(data, []byte("\n")), []byte("\n"))
	tasks := map[string]bool{}
	for i, line := range lines {
		var e Event
		if err := json.Unmarshal(line, &e); err != nil || e.Seq != int64(i+1) || check(&e) != nil {
			t.Fatalf("line %d is not event seq %d: %v %s", i+1, i+1, err, line)
		}
		if e.Type == "bench.run" {
			var r BenchRun
			json.Unmarshal(e.Data, &r)
			tasks[r.Task] = true
		}
	}
	// Line 1 is this process's home.opened, which no writer appended.
	if len(lines) != writers*each+1 || len(returned) != writers*each || len(tasks) != writers*each {
		t.Fatalf("%d lines, %d distinct seqs returned, %d distinct tasks; want %d appends over one home.opened", len(lines), len(returned), len(tasks), writers*each)
	}
	if err := Verify(Path(root)); err != nil {
		t.Fatal(err)
	}
}

func TestClockGoingBackwardsDoesNotReorder(t *testing.T) {
	root := testenv.Isolate(t)
	l, err := Open(root, clock(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	for i := range 3 {
		mustAppend(t, l, run("v1", fmt.Sprint("t", i), "defect", "m", 1))
	}
	evs := readAll(t, root)
	if evs[0].TS <= evs[2].TS || evs[2].Seq != 3 {
		t.Fatalf("ts %s then %s at seq %d: the clock should run backwards while seq still increases", evs[0].TS, evs[2].TS, evs[2].Seq)
	}
	if err := Verify(Path(root)); err != nil {
		t.Fatal(err)
	}
}

func TestSecondWriterIsRefused(t *testing.T) {
	root := testenv.Isolate(t)
	open(t, root)
	link := filepath.Join(t.TempDir(), "link")
	os.Symlink(root, link)
	for _, r := range []string{root, link} {
		_, err := Open(r, clock(time.Millisecond))
		var le *LockedError
		if !errors.As(err, &le) || !strings.Contains(le.Holder, fmt.Sprintf(`"pid":%d`, os.Getpid())) {
			t.Fatalf("second Open through %s = %v, want LockedError naming pid %d", r, err, os.Getpid())
		}
	}
}

func TestReplacedLockOrLogStopsTheWriter(t *testing.T) {
	for _, victim := range []string{"home.lock", filepath.Join("log", "events.jsonl")} {
		t.Run(victim, func(t *testing.T) {
			root := testenv.Isolate(t)
			l := open(t, root)
			mustAppend(t, l, run("v1", "t1", "defect", "m", 1))
			os.Remove(filepath.Join(root, victim))
			if _, err := l.Append(run("v1", "t2", "defect", "m", 1)); err == nil || !strings.Contains(err.Error(), "replaced or removed") {
				t.Fatalf("Append after removing %s = %v, want a refusal", victim, err)
			}
		})
	}
}

func TestHostileRoots(t *testing.T) {
	base := testenv.Isolate(t)
	if _, err := Open("relative", time.Now); !errors.Is(err, ErrNoRoot) {
		t.Errorf("relative root: %v", err)
	}
	ro := filepath.Join(base, "readonly")
	os.Mkdir(ro, 0o500)
	if _, err := Open(ro, time.Now); !errors.Is(err, os.ErrPermission) {
		t.Errorf("read-only root: %v, want a permission error", err)
	}
	sym := filepath.Join(base, "symlinked-log")
	os.MkdirAll(filepath.Join(sym, "log"), 0o700)
	os.WriteFile(filepath.Join(base, "elsewhere"), nil, 0o600)
	os.Symlink(filepath.Join(base, "elsewhere"), Path(sym))
	if _, err := Open(sym, time.Now); !errors.Is(err, syscall.ELOOP) {
		t.Errorf("events.jsonl as a symlink: %v, want ELOOP", err)
	}
}

// helperEnv makes the test binary act as a second process holding or appending
// to a home, for the cross-process cases below.
const helperEnv = "FOLIOT_LOG_HELPER"

func helper() {
	root := os.Getenv("FOLIOT_HOME")
	l, err := Open(root, time.Now)
	if err != nil {
		fmt.Println("open:", err)
		os.Exit(3)
	}
	for i := 0; ; i++ {
		if _, err := l.Append(run("v1", fmt.Sprint("child-", i), "defect", "m", 1)); err != nil {
			fmt.Println("append:", err)
			os.Exit(4)
		}
		if i == 0 {
			fmt.Println("held") // after the first event, so the parent can count on it
		}
		if os.Getenv(helperEnv) == "hold" {
			time.Sleep(time.Hour)
		}
	}
}

func startHelper(t *testing.T, root, mode string) *exec.Cmd {
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), helperEnv+"="+mode, "FOLIOT_HOME="+root)
	stdout, _ := cmd.StdoutPipe()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
	line, _ := bufio.NewReader(stdout).ReadString('\n')
	if line != "held\n" {
		t.Fatalf("helper said %q", line)
	}
	return cmd
}

func TestOtherProcessHoldingTheLockRefusesThenDiesAndIsTakenOver(t *testing.T) {
	root := testenv.Isolate(t)
	child := startHelper(t, root, "hold")
	_, err := Open(root, time.Now)
	var le *LockedError
	if !errors.As(err, &le) || !strings.Contains(le.Holder, `"pid":`+strconv.Itoa(child.Process.Pid)) {
		t.Fatalf("Open while pid %d holds the lock = %v", child.Process.Pid, err)
	}
	child.Process.Signal(syscall.SIGKILL)
	child.Wait()
	l := open(t, root)
	mustAppend(t, l, run("v1", "parent", "defect", "m", 1))
	evs := readAll(t, root)
	if got := types(evs); got != "1:home.opened 2:bench.run 3:home.lock_taken 4:home.opened 5:bench.run" {
		t.Fatalf("events: %s", got)
	}
	if !strings.HasPrefix(string(evs[2].Data), fmt.Sprintf(`{"stale_pid":%d,"stale_started_at":"`, child.Process.Pid)) {
		t.Fatalf("lock_taken data %s", evs[2].Data)
	}
}

func TestKillMidAppendLosesNoCompleteEvent(t *testing.T) {
	root := testenv.Isolate(t)
	for round := range 5 {
		child := startHelper(t, root, "stream")
		time.Sleep(time.Duration(5+round*7) * time.Millisecond)
		child.Process.Signal(syscall.SIGKILL)
		child.Wait()
		l, err := Open(root, time.Now)
		if err != nil {
			t.Fatalf("round %d: reopen after SIGKILL: %v", round, err)
		}
		l.Close()
		if err := Verify(Path(root)); err != nil {
			t.Fatalf("round %d: %v", round, err)
		}
	}
	evs := readAll(t, root)
	for i, e := range evs {
		if e.Seq != int64(i+1) {
			t.Fatalf("event %d has seq %d: the log has a gap", i+1, e.Seq)
		}
	}
	if len(evs) < 5 {
		t.Fatalf("only %d events after five killed writers", len(evs))
	}
	t.Logf("%d events across five SIGKILLed writers, gapless", len(evs))
}

// homePID decodes the pid a home.opened or home.closed names.
func homePID(e Event) int {
	var d struct {
		PID int `json:"pid"`
	}
	json.Unmarshal(e.Data, &d)
	return d.PID
}

// unpairedOpens returns the pid of every home.opened no home.closed named, which is
// exactly the unclean-exit condition a reconcile reads (critique c8 F5).
func unpairedOpens(evs []Event) []int {
	open := map[int64]int{} // seq of a home.opened -> its pid
	for _, e := range evs {
		switch e.Type {
		case "home.opened":
			open[e.Seq] = homePID(e)
		case "home.closed":
			var d struct {
				OpenedSeq int64 `json:"opened_seq"`
			}
			json.Unmarshal(e.Data, &d)
			delete(open, d.OpenedSeq)
		}
	}
	var pids []int
	for _, seq := range slices.Sorted(maps.Keys(open)) {
		pids = append(pids, open[seq])
	}
	return pids
}

// A killed writer leaves a home.opened its home.closed never paired; a clean exit
// leaves the pair. That difference is what a later reconcile has to read to know a
// home was left with work in it (critique c8 F5).
func TestHomeOpenedIsPairedByHomeClosedOnlyOnACleanExit(t *testing.T) {
	root := testenv.Isolate(t)
	child := startHelper(t, root, "hold")
	child.Process.Signal(syscall.SIGKILL)
	child.Wait()
	if got := unpairedOpens(readAll(t, root)); len(got) != 1 || got[0] != child.Process.Pid {
		t.Fatalf("after SIGKILL the unpaired opens are %v, want just the killed pid %d\nevents: %s",
			got, child.Process.Pid, types(readAll(t, root)))
	}
	l, err := Open(root, clock(time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	mustAppend(t, l, run("v1", "parent", "defect", "m", 1))
	if got := unpairedOpens(readAll(t, root)); len(got) != 2 {
		t.Fatalf("a live writer over an unclean home has unpaired opens %v, want the dead one and its own", got)
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	got := unpairedOpens(readAll(t, root))
	if len(got) != 1 || got[0] != child.Process.Pid {
		t.Fatalf("after a clean Close the unpaired opens are %v, want only the killed pid %d\nevents: %s",
			got, child.Process.Pid, types(readAll(t, root)))
	}
	t.Logf("events: %s", types(readAll(t, root)))
}

// The catalogue is closed, so a new type is a change to a5 section 2.3 and to this list
// in the same commit; the count is the number the successor critique reads against.
func TestCatalogueIsTheClosedListItDeclares(t *testing.T) {
	testenv.Isolate(t)
	want := []string{
		"bench.estimate", "bench.probe", "bench.refused", "bench.run", "bench.verdict",
		"bench.verified", "home.closed", "home.lock_taken", "home.opened",
		"log.quarantined", "log.repaired",
	}
	if got := slices.Sorted(maps.Keys(catalogue)); !slices.Equal(got, want) {
		t.Fatalf("the catalogue holds %d types\n%v\nwant %d\n%v", len(got), got, len(want), want)
	}
}
