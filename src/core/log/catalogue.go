package log

// entry is one row of the closed catalogue (a5 section 2.3): the actor that writes
// the type and the data fields it requires.
type entry struct {
	actor    string
	required []string
	durable  bool // fsync after the write
}

// catalogue holds the types P1 needs: the log's own repair records, the lock
// takeover, and the benchmark's two events. A new type is a change to a5 section
// 2.3 and to this package's tests in the same commit.
var catalogue = map[string]entry{
	"home.lock_taken": {actor: "orchestrator", required: []string{"stale_pid", "stale_started_at"}},
	// A repair has already moved bytes out of the log, so its record is synced even
	// though a5 section 2.1 names no log.* type among the durability points.
	"log.repaired":    {actor: "orchestrator", required: []string{"dropped_bytes", "moved_to"}, durable: true},
	"log.quarantined": {actor: "orchestrator", required: []string{"seq", "reason"}, durable: true},
	"bench.run": {actor: "bench", required: []string{
		"corpus", "task", "class", "arm", "adapter", "gate", "repeat", "model", "provider",
		"billing", "base_sha", "base_exit", "landed_exit", "benchmark",
	}},
	"bench.verdict": {actor: "bench", required: []string{
		"corpus", "task", "arm", "repeat", "pass", "false_claim", "columns",
	}},
}
