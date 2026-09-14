package log

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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
	if seq := mustAppend(t, l, run("v1", "t1", "defect", "m", 1)); seq != 1 {
		t.Fatalf("first seq %d, want 1", seq)
	}
	e := verdict("v1", "t1", "m", 1, true, 0.5)
	e.Task, e.Evidence = &task, []Evidence{{Kind: "file", Ref: "bench/checks/t1/check.sh"}}
	cause := int64(1)
	e.Cause = &cause
	if seq := mustAppend(t, l, e); seq != 2 {
		t.Fatalf("second seq %d, want 2", seq)
	}
	data, _ := os.ReadFile(Path(root))
	lines := strings.Split(string(data), "\n")
	want := `{"seq":2,"ts":"2026-09-14T12:00:00.003Z","type":"bench.verdict","task":"log-core-l1","attempt":null,"actor":"bench","cause":1,"evidence":[{"kind":"file","ref":"bench/checks/t1/check.sh"}],"data":{"corpus":"v1","task":"t1","arm":"bare-m","repeat":1,"pass":true,"false_claim":false,"columns":{"cost_usd":0.5}},"v":1}`
	if len(lines) != 3 || lines[2] != "" || lines[1] != want {
		t.Fatalf("log is\n%s\nwant line 2\n%s", data, want)
	}
	if !strings.Contains(lines[0], `"evidence":[]`) {
		t.Fatalf("an event with no evidence must carry an empty array: %s", lines[0])
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
	if seq := mustAppend(t, l, run("v1", "t1", "defect", "m", 1)); seq != 1 {
		t.Fatalf("seq after ten refusals is %d, want 1", seq)
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
	firstLine := bytes.IndexByte(full, '\n') + 1
	torn := full[:len(full)-9] // the second line loses its last nine bytes, newline included
	os.WriteFile(Path(root), torn, 0o600)

	if err := Verify(Path(root)); err == nil || !strings.Contains(err.Error(), "log seq 2: torn last line, ") {
		t.Fatalf("Verify before repair = %v, want the torn line named at seq 2", err)
	}
	l = open(t, root)
	evs := readAll(t, root)
	if got := types(evs); got != "1:bench.run 2:log.repaired" {
		t.Fatalf("events after repair: %s", got)
	}
	var rep struct {
		DroppedBytes int    `json:"dropped_bytes"`
		MovedTo      string `json:"moved_to"`
	}
	json.Unmarshal(evs[1].Data, &rep)
	moved, err := os.ReadFile(filepath.Join(root, rep.MovedTo))
	if err != nil || !bytes.Equal(moved, torn[firstLine:]) || rep.DroppedBytes != len(moved) {
		t.Fatalf("moved_to %s holds %q (err %v), dropped_bytes %d; want the %d torn bytes", rep.MovedTo, moved, err, rep.DroppedBytes, len(torn)-firstLine)
	}
	now, _ := os.ReadFile(Path(root))
	if !bytes.HasPrefix(now, full[:firstLine]) {
		t.Fatal("the last complete event was not left intact")
	}
	if err := Verify(Path(root)); err != nil {
		t.Fatalf("Verify after repair: %v", err)
	}
	if seq := mustAppend(t, l, run("v1", "t2", "defect", "m", 1)); seq != 3 {
		t.Fatalf("next seq %d, want 3", seq)
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
	if got := types(evs); got != "1:bench.run 3:bench.run 4:log.quarantined" {
		t.Fatalf("events: %s", got)
	}
	if !strings.HasPrefix(string(evs[2].Data), `{"reason":"unparseable: `) || !strings.HasSuffix(string(evs[2].Data), `,"seq":2}`) {
		t.Fatalf("quarantine data %s", evs[2].Data)
	}
	if err := Verify(Path(root)); err != nil {
		t.Fatalf("Verify after quarantine: %v", err)
	}
	if seq := mustAppend(t, l, run("v1", "t9", "defect", "m", 1)); seq != 5 {
		t.Fatalf("next seq %d, want 5", seq)
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
	if got := types(evs); got != "3:log.quarantined" {
		t.Fatalf("events: %s, want only the quarantine of seq 1 (the unknown type is skipped, not quarantined)", got)
	}
	if !strings.Contains(string(evs[0].Data), `missing required data field \"task\"`) {
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
		var r BenchRun
		json.Unmarshal(e.Data, &r)
		tasks[r.Task] = true
	}
	if len(lines) != writers*each || len(returned) != writers*each || len(tasks) != writers*each {
		t.Fatalf("%d lines, %d distinct seqs returned, %d distinct tasks; want %d of each", len(lines), len(returned), len(tasks), writers*each)
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
	if got := types(evs); got != "1:bench.run 2:home.lock_taken 3:bench.run" {
		t.Fatalf("events: %s", got)
	}
	if !strings.HasPrefix(string(evs[1].Data), fmt.Sprintf(`{"stale_pid":%d,"stale_started_at":"`, child.Process.Pid)) {
		t.Fatalf("lock_taken data %s", evs[1].Data)
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
