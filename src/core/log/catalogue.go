package log

// entry is one row of the closed catalogue (a5 section 2.3): the actor that writes
// the type and the data fields it requires.
type entry struct {
	actor    string
	required []string
	durable  bool // fsync after the write
}

// catalogue holds the types P1 needs: the log's own repair records, the lock
// takeover, and the benchmark's events. A new type is a change to a5 section
// 2.3 and to this package's tests in the same commit.
var catalogue = map[string]entry{
	"home.lock_taken": {actor: "orchestrator", required: []string{"stale_pid", "stale_started_at"}},
	// A repair has already moved bytes out of the log, so its record is synced even
	// though a5 section 2.1 names no log.* type among the durability points.
	"log.repaired":    {actor: "orchestrator", required: []string{"dropped_bytes", "moved_to"}, durable: true},
	"log.quarantined": {actor: "orchestrator", required: []string{"seq", "reason"}, durable: true},
	// bench.run is written before the worker starts, so a run the runner never finished
	// is still on the log. Beyond a5's row it carries the isolation and contamination
	// readings the Fable critique k3 sections 1.2 and 1.3 require on every run.
	"bench.run": {actor: "bench", required: []string{
		"corpus", "corpus_sha", "task", "class", "arm", "adapter", "gate", "repeat", "model", "provider",
		"billing", "base_sha", "base_exit", "landed_exit", "benchmark", "cap_usd",
		"isolation_proven", "isolation_probe", "history_free", "landed_object_exit",
		"model_cutoff", "public_since", "run",
	}, durable: true},
	"bench.verdict": {actor: "bench", required: []string{
		"corpus", "task", "arm", "repeat", "pass", "false_claim", "columns", "check_exit", "examined",
	}, durable: true},
	// bench.verified is one task's corpus certification; a sweep reuses it while the corpus
	// is clean and the task's record and hidden check still hash to its task_sha.
	"bench.verified": {actor: "bench", required: []string{
		"corpus", "corpus_sha", "task", "base_exit", "landed_exit", "examined",
	}, durable: true},
	// bench.probe is the isolation probe's reading; a sweep starts only after one
	// with proven true in the same invocation (k3 section 1.3 item 1).
	"bench.probe": {actor: "bench", required: []string{
		"corpus", "task", "arm", "model", "isolation", "check_lines", "lines_seen", "denials", "proven", "cost_usd",
	}, durable: true},
	// bench.estimate is the measured projection bench run reads before a sweep (p8 R15).
	"bench.estimate": {actor: "bench", required: []string{
		"corpus", "arms", "repeats", "tasks", "mean_cost_usd", "projected_usd",
	}, durable: true},
}
