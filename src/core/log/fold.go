package log

import (
	"encoding/json"
	"slices"
	"sort"
)

// BenchRun is bench.run's data (a5 section 2.3), written before the worker starts.
// BaseExit and LandedExit are the hidden check's exit codes at base_sha and at
// landed_sha (a5 section 2.16). LandedObjectExit is `git cat-file -e <landed_sha>`
// in the worker's checkout, which is 1 exactly when the checkout is history-free.
type BenchRun struct {
	Corpus           string  `json:"corpus"`
	CorpusSHA        string  `json:"corpus_sha"`
	Task             string  `json:"task"`
	Class            string  `json:"class"`
	Arm              string  `json:"arm"`
	Adapter          string  `json:"adapter"`
	Gate             string  `json:"gate"`
	Repeat           int     `json:"repeat"`
	Model            string  `json:"model"`
	Provider         string  `json:"provider"`
	Billing          string  `json:"billing"`
	BaseSHA          string  `json:"base_sha"`
	BaseExit         int     `json:"base_exit"`
	LandedExit       int     `json:"landed_exit"`
	Benchmark        bool    `json:"benchmark"`
	CapUSD           float64 `json:"cap_usd"`
	IsolationProven  bool    `json:"isolation_proven"`
	IsolationProbe   int64   `json:"isolation_probe"` // the seq of the bench.probe that proved it
	HistoryFree      bool    `json:"history_free"`
	LandedObjectExit int     `json:"landed_object_exit"`
	ModelCutoff      string  `json:"model_cutoff"` // YYYY-MM, the vendor's training data cutoff
	PublicSince      string  `json:"public_since"` // YYYY-MM-DD, the earliest the landed change could be public
	Run              string  `json:"run"`          // the run's directory under <root>/bench/runs
}

// BenchVerdict is bench.verdict's data; Columns holds the per-run columns of a5
// section 2.16 by name, a null value being a reading whose control failed.
type BenchVerdict struct {
	Corpus     string         `json:"corpus"`
	Task       string         `json:"task"`
	Arm        string         `json:"arm"`
	Repeat     int            `json:"repeat"`
	Pass       bool           `json:"pass"`
	FalseClaim bool           `json:"false_claim"`
	Columns    map[string]any `json:"columns"`
	CheckExit  int            `json:"check_exit"`
	Examined   int            `json:"examined"`
}

// BenchProbe is bench.probe's data. Isolation is "sandbox" when the run profile was
// written and "absent" when the probe ran without it, which only a red proof does.
type BenchProbe struct {
	Corpus     string   `json:"corpus"`
	Task       string   `json:"task"`
	Arm        string   `json:"arm"`
	Model      string   `json:"model"`
	Isolation  string   `json:"isolation"`
	CheckLines int      `json:"check_lines"`
	LinesSeen  int      `json:"lines_seen"`
	Denials    int      `json:"denials"`
	Attempts   int      `json:"attempts"`
	Proven     bool     `json:"proven"`
	CostUSD    *float64 `json:"cost_usd"`
	Run        string   `json:"run"`
}

// BenchEstimate is bench.estimate's data: the measured mean cost per run per arm
// over the tasks it ran, and the sweep cost projected from it.
type BenchEstimate struct {
	Corpus       string             `json:"corpus"`
	Arms         []string           `json:"arms"`
	Repeats      int                `json:"repeats"`
	Tasks        []string           `json:"tasks"`
	MeanCostUSD  map[string]float64 `json:"mean_cost_usd"`
	ProjectedUSD float64            `json:"projected_usd"`
	CorpusTasks  int                `json:"corpus_tasks"`
}

// State is the part of a5 section 2.4's derived state that P1's events produce.
type State struct {
	LastSeq int64                           `json:"last_seq"`
	Bench   map[string]map[string]BenchCell `json:"bench"` // [class][model]
}

// BenchCell is state.bench[class][model]. Spread is computed by bench report, which
// owns the columns.
type BenchCell struct {
	PassRate   float64  `json:"pass_rate"`
	N          int      `json:"n"`
	CostMedian *float64 `json:"cost_median"` // null when no verdict carried cost_usd
}

// RunRecord is one run with the verdict that ended it, nil while none has.
type RunRecord struct {
	Seq     int64
	Run     BenchRun
	Verdict *BenchVerdict
}

type runKey struct {
	corpus, task, arm string
	repeat            int
}

// usable yields the events a fold may read: those the catalogue accepts and no
// log.quarantined names.
func usable(events []Event) []Event {
	quarantined := map[int64]bool{}
	for _, e := range events {
		var q struct{ Seq int64 }
		if e.Type == "log.quarantined" && check(&e) == nil && json.Unmarshal(e.Data, &q) == nil {
			quarantined[q.Seq] = true
		}
	}
	var out []Event
	for _, e := range events {
		if !quarantined[e.Seq] && check(&e) == nil {
			out = append(out, e)
		}
	}
	return out
}

// Runs folds the benchmark's runs: a later bench.run for the same (corpus, task, arm,
// repeat) starts that run again and drops its earlier verdict, a verdict for a run not
// seen is skipped, and a later verdict replaces an earlier one. Ordered by run seq.
func Runs(events []Event) []RunRecord {
	byKey := map[runKey]*RunRecord{}
	for _, e := range usable(events) {
		switch e.Type {
		case "bench.run":
			var r BenchRun
			if json.Unmarshal(e.Data, &r) == nil {
				byKey[runKey{r.Corpus, r.Task, r.Arm, r.Repeat}] = &RunRecord{Seq: e.Seq, Run: r}
			}
		case "bench.verdict":
			var v BenchVerdict
			if json.Unmarshal(e.Data, &v) != nil {
				continue
			}
			if rec := byKey[runKey{v.Corpus, v.Task, v.Arm, v.Repeat}]; rec != nil {
				rec.Verdict = &v
			}
		}
	}
	out := make([]RunRecord, 0, len(byKey))
	for _, rec := range byKey {
		out = append(out, *rec)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Seq < out[j].Seq })
	return out
}

// Estimates folds every bench.estimate, oldest first.
func Estimates(events []Event) []BenchEstimate {
	var out []BenchEstimate
	for _, e := range usable(events) {
		var est BenchEstimate
		if e.Type == "bench.estimate" && json.Unmarshal(e.Data, &est) == nil {
			out = append(out, est)
		}
	}
	return out
}

// Fold is the pure function from events to state: no clock, no randomness, no
// filesystem (a5 section 2.1).
func Fold(events []Event) State {
	st := State{Bench: map[string]map[string]BenchCell{}}
	for _, e := range events {
		st.LastSeq = max(st.LastSeq, e.Seq)
	}
	// Counts and sorted costs do not depend on map order, so the state is stable.
	type acc struct {
		n, pass int
		costs   []float64
	}
	cells := map[[2]string]*acc{}
	for _, rec := range Runs(events) {
		if rec.Verdict == nil {
			continue
		}
		k := [2]string{rec.Run.Class, rec.Run.Model}
		a := cells[k]
		if a == nil {
			a = &acc{}
			cells[k] = a
		}
		a.n++
		if rec.Verdict.Pass {
			a.pass++
		}
		if c, ok := rec.Verdict.Columns["cost_usd"].(float64); ok {
			a.costs = append(a.costs, c)
		}
	}
	for key, a := range cells {
		if st.Bench[key[0]] == nil {
			st.Bench[key[0]] = map[string]BenchCell{}
		}
		st.Bench[key[0]][key[1]] = BenchCell{PassRate: float64(a.pass) / float64(a.n), N: a.n, CostMedian: median(a.costs)}
	}
	return st
}

func median(xs []float64) *float64 {
	if len(xs) == 0 {
		return nil
	}
	slices.Sort(xs)
	m := xs[len(xs)/2]
	if len(xs)%2 == 0 {
		m = (xs[len(xs)/2-1] + m) / 2
	}
	return &m
}
