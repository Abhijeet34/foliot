package log

import (
	"encoding/json"
	"slices"
)

// BenchRun is bench.run's data (a5 section 2.3). BaseExit and LandedExit are the
// hidden check's exit codes at base_sha and at landed_sha (a5 section 2.16).
type BenchRun struct {
	Corpus     string `json:"corpus"`
	Task       string `json:"task"`
	Class      string `json:"class"`
	Arm        string `json:"arm"`
	Adapter    string `json:"adapter"`
	Gate       string `json:"gate"`
	Repeat     int    `json:"repeat"`
	Model      string `json:"model"`
	Provider   string `json:"provider"`
	Billing    string `json:"billing"`
	BaseSHA    string `json:"base_sha"`
	BaseExit   int    `json:"base_exit"`
	LandedExit int    `json:"landed_exit"`
	Benchmark  bool   `json:"benchmark"`
}

// BenchVerdict is bench.verdict's data; Columns holds the per-run columns of a5
// section 2.16 by name, of which the fold reads cost_usd.
type BenchVerdict struct {
	Corpus     string         `json:"corpus"`
	Task       string         `json:"task"`
	Arm        string         `json:"arm"`
	Repeat     int            `json:"repeat"`
	Pass       bool           `json:"pass"`
	FalseClaim bool           `json:"false_claim"`
	Columns    map[string]any `json:"columns"`
}

// State is the part of a5 section 2.4's derived state that P1's events produce.
type State struct {
	LastSeq int64                           `json:"last_seq"`
	Bench   map[string]map[string]BenchCell `json:"bench"` // [class][model]
}

// BenchCell is state.bench[class][model]. a5 section 2.4 also names spread, which
// no document defines yet; it is left to the bench report that owns the columns.
type BenchCell struct {
	PassRate   float64  `json:"pass_rate"`
	N          int      `json:"n"`
	CostMedian *float64 `json:"cost_median"` // null when no verdict carried cost_usd
}

type runKey struct {
	corpus, task, arm string
	repeat            int
}

// Fold is the pure function from events to state: no clock, no randomness, no
// filesystem (a5 section 2.1). It skips an event the catalogue refuses, an event
// a log.quarantined names, and a verdict whose run it has not seen; a later
// verdict for the same run replaces an earlier one.
func Fold(events []Event) State {
	quarantined := map[int64]bool{}
	for _, e := range events {
		var q struct{ Seq int64 }
		if e.Type == "log.quarantined" && check(&e) == nil && json.Unmarshal(e.Data, &q) == nil {
			quarantined[q.Seq] = true
		}
	}
	st := State{Bench: map[string]map[string]BenchCell{}}
	runs := map[runKey]BenchRun{}
	verdicts := map[runKey]BenchVerdict{}
	for _, e := range events {
		st.LastSeq = max(st.LastSeq, e.Seq)
		if quarantined[e.Seq] || check(&e) != nil {
			continue
		}
		switch e.Type {
		case "bench.run":
			var r BenchRun
			if json.Unmarshal(e.Data, &r) == nil {
				runs[runKey{r.Corpus, r.Task, r.Arm, r.Repeat}] = r
			}
		case "bench.verdict":
			var v BenchVerdict
			if json.Unmarshal(e.Data, &v) != nil {
				continue
			}
			k := runKey{v.Corpus, v.Task, v.Arm, v.Repeat}
			if _, ok := runs[k]; ok {
				verdicts[k] = v
			}
		}
	}
	// Counts and sorted costs do not depend on map order, so the state is stable.
	type acc struct {
		n, pass int
		costs   []float64
	}
	cells := map[[2]string]*acc{}
	for k, v := range verdicts {
		r := runs[k]
		a := cells[[2]string{r.Class, r.Model}]
		if a == nil {
			a = &acc{}
			cells[[2]string{r.Class, r.Model}] = a
		}
		a.n++
		if v.Pass {
			a.pass++
		}
		if c, ok := v.Columns["cost_usd"].(float64); ok {
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
