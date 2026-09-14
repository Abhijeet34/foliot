package bench

import (
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Abhijeet34/foliot/src/core/log"
)

// ReportOptions select what bench report reads.
type ReportOptions struct {
	Root, Corpus string
	Arms         []string // empty means every arm with a run in scope
	Tasks        []string // the scope; empty means the whole corpus
	Out          io.Writer
}

// column is one reported column: numeric columns aggregate by mean or median, categorical
// ones print their value counts and carry no spread.
type column struct {
	name, agg string // agg: mean, median or counts
}

// reportColumns is p8 R13's list for the bare arms, in its order, with the two verdict
// columns as rates. The routed arm's two columns arrive with the router (P2).
var reportColumns = []column{
	{"pass_rate", "mean"}, {"false_claim_rate", "mean"},
	{"input_tokens", "mean"}, {"output_tokens", "mean"}, {"cache_read", "mean"}, {"cache_write", "mean"},
	{"cost_usd", "mean"}, {"wall_ms", "median"}, {"cpu_ms", "median"}, {"load_at_start", "median"},
	{"ended_by", "counts"}, {"denials_in_scope", "mean"}, {"refusals_routed", "mean"},
	{"questions_after", "mean"}, {"orchestrator_tokens", "mean"}, {"billing", "counts"},
}

// Report is foliot bench report. It returns ErrRefused when any arm, class or column in
// scope rests on zero runs (a5 section 1.1 `bench`: never a column over zero runs).
func Report(o ReportOptions) error {
	corpusDir := filepath.Join(o.Root, "bench", "corpus", o.Corpus)
	m, err := LoadManifest(filepath.Join(corpusDir, "corpus.json"))
	if err != nil {
		return err
	}
	ids, err := taskIDs(corpusDir)
	if err != nil {
		return err
	}
	tasks := map[string]*Task{}
	for _, id := range ids {
		t, refusals := LoadTask(filepath.Join(corpusDir, id, "task.json"), m)
		if t == nil {
			return fmt.Errorf("task %s: %s", id, strings.Join(refusals, "; "))
		}
		tasks[id] = t
	}
	scope := map[string]bool{}
	if len(o.Tasks) == 0 {
		for _, id := range ids {
			scope[id] = true
		}
	}
	for _, id := range o.Tasks {
		if tasks[id] == nil {
			return refusef("task %q is not in corpus %s", id, o.Corpus)
		}
		scope[id] = true
	}
	classesInScope := map[string]bool{}
	for id := range scope {
		classesInScope[tasks[id].Class] = true
	}

	events, err := log.Read(log.Path(o.Root), 1)
	if err != nil {
		return err
	}
	var runs, corpusRuns []log.RunRecord
	for _, rec := range log.Runs(events) {
		if rec.Run.Corpus != o.Corpus {
			continue
		}
		corpusRuns = append(corpusRuns, rec)
		if scope[rec.Run.Task] && rec.Verdict != nil {
			runs = append(runs, rec)
		}
	}

	var refusals []string
	w := o.Out
	header(w, o.Corpus, ids, tasks, len(scope), len(runs), corpusRuns)

	arms := o.Arms
	if len(arms) == 0 {
		seen := map[string]bool{}
		for _, r := range runs { // in the order each arm first ran
			if !seen[r.Run.Arm] {
				seen[r.Run.Arm] = true
				arms = append(arms, r.Run.Arm)
			}
		}
	}
	if len(arms) == 0 {
		refusals = append(refusals, fmt.Sprintf("no arm has a run in scope in corpus %s", o.Corpus))
	}
	for _, arm := range arms {
		var armRuns []log.RunRecord
		for _, r := range runs {
			if r.Run.Arm == arm {
				armRuns = append(armRuns, r)
			}
		}
		if len(armRuns) == 0 {
			refusals = append(refusals, fmt.Sprintf("arm %s has zero runs in scope, so no column of it is reported", arm))
			continue
		}
		refusals = append(refusals, row(w, arm, "all", armRuns)...)
		contamination(w, arm, armRuns)
		for _, class := range Classes {
			if !classesInScope[class] {
				continue
			}
			var classRuns []log.RunRecord
			for _, r := range armRuns {
				if r.Run.Class == class {
					classRuns = append(classRuns, r)
				}
			}
			label := class
			if class == "defect" {
				label = "defect(gating)"
			}
			if len(classRuns) == 0 {
				refusals = append(refusals, fmt.Sprintf("arm %s class %s has zero runs in scope", arm, class))
				continue
			}
			refusals = append(refusals, row(w, arm, label, classRuns)...)
		}
	}
	for _, r := range refusals {
		fmt.Fprintf(w, "refused: %s\n", r)
	}
	if len(refusals) > 0 {
		return refusef("%d report refusals", len(refusals))
	}
	return nil
}

// header states what the corpus is drawn from, so no reader takes one repository's
// numbers for a general result (Fable critique k3 section 1.2).
// A repository's visibility and language are read from any run of the corpus, in scope or not.
func header(w io.Writer, corpus string, ids []string, tasks map[string]*Task, inScope, scored int, runs []log.RunRecord) {
	byRepo := map[string]int{}
	for _, id := range ids {
		byRepo[tasks[id].Repository]++
	}
	var repos []string
	for r := range byRepo {
		repos = append(repos, r)
	}
	sort.Strings(repos)
	for _, repo := range repos {
		visibility, language := "visibility-unread", "language-unread"
		for _, r := range runs {
			if r.Run.Repository == repo && r.Run.RepositoryPublic != nil {
				visibility, language = "private", r.Run.RepositoryLanguage
				if *r.Run.RepositoryPublic {
					visibility = "public"
				}
			}
		}
		count := "one"
		if len(repos) > 1 {
			count = fmt.Sprintf("%d", len(repos))
		}
		fmt.Fprintf(w, "corpus %s is %d of %d tasks from %s %s %s repository (%s)\n", corpus, byRepo[repo], len(ids), count, visibility, language, repo)
	}
	fmt.Fprintf(w, "scope: %d of %d tasks; runs with a verdict: %d; spread is max minus min of the column over repeat indices\n", inScope, len(ids), scored)
}

// contamination prints the arm's training cutoff against when its tasks' landed changes
// became public: a run whose cutoff is not before that date may be recall, not capability.
func contamination(w io.Writer, arm string, runs []log.RunRecord) {
	cutoff, earliest, n := "", "", 0
	for _, r := range runs {
		cutoff = r.Run.ModelCutoff
		if earliest == "" || r.Run.PublicSince < earliest {
			earliest = r.Run.PublicSince
		}
		if r.Run.ModelCutoff != "" && r.Run.PublicSince != "" && r.Run.ModelCutoff >= r.Run.PublicSince[:min(7, len(r.Run.PublicSince))] {
			n++
		}
	}
	fmt.Fprintf(w, "  %s contamination: model_cutoff=%s earliest_public_since=%s runs_at_or_after_cutoff=%d of %d\n", arm, cutoff, earliest, n, len(runs))
}

// row prints one arm-by-class line and returns a refusal for every column with no known
// value among its runs.
func row(w io.Writer, arm, class string, runs []log.RunRecord) []string {
	var refusals []string
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s n=%d", arm, class, len(runs))
	for _, c := range reportColumns {
		byRepeat := map[int][]float64{}
		var all []float64
		counts := map[string]int{}
		for _, r := range runs {
			v, ok := value(r, c.name)
			if !ok {
				continue
			}
			switch x := v.(type) {
			case float64:
				all = append(all, x)
				byRepeat[r.Run.Repeat] = append(byRepeat[r.Run.Repeat], x)
			case string:
				counts[x]++
			}
		}
		if c.agg == "counts" {
			if len(counts) == 0 {
				refusals = append(refusals, fmt.Sprintf("arm %s class %s column %s has zero runs with a reading", arm, class, c.name))
				fmt.Fprintf(&b, " %s=refused", c.name)
				continue
			}
			var parts []string
			for k, n := range counts {
				parts = append(parts, fmt.Sprintf("%s:%d", k, n))
			}
			sort.Strings(parts)
			fmt.Fprintf(&b, " %s=%s", c.name, strings.Join(parts, ","))
			continue
		}
		if len(all) == 0 {
			refusals = append(refusals, fmt.Sprintf("arm %s class %s column %s has zero runs with a reading", arm, class, c.name))
			fmt.Fprintf(&b, " %s=refused", c.name)
			continue
		}
		agg := aggregate(c.agg, all)
		spread := "n/a"
		if len(byRepeat) > 1 {
			lo, hi := 0.0, 0.0
			first := true
			for _, xs := range byRepeat {
				v := aggregate(c.agg, xs)
				if first || v < lo {
					lo = v
				}
				if first || v > hi {
					hi = v
				}
				first = false
			}
			spread = num(hi - lo)
		}
		unknown := ""
		if len(all) < len(runs) {
			unknown = fmt.Sprintf(" unknown=%d", len(runs)-len(all))
		}
		fmt.Fprintf(&b, " %s=%s spread=%s%s", c.name, num(agg), spread, unknown)
	}
	fmt.Fprintln(w, b.String())
	return refusals
}

func value(r log.RunRecord, name string) (any, bool) {
	v := r.Verdict
	switch name {
	case "pass_rate":
		return b2f(v.Pass), true
	case "false_claim_rate":
		return b2f(v.FalseClaim), true
	}
	x, ok := v.Columns[name]
	if !ok || x == nil {
		return nil, false
	}
	switch t := x.(type) {
	case float64, string:
		return t, true
	}
	return nil, false
}

func b2f(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

func aggregate(kind string, xs []float64) float64 {
	s := append([]float64(nil), xs...)
	sort.Float64s(s)
	if kind == "median" {
		if len(s)%2 == 1 {
			return s[len(s)/2]
		}
		return (s[len(s)/2-1] + s[len(s)/2]) / 2
	}
	var sum float64
	for _, x := range s {
		sum += x
	}
	return sum / float64(len(s))
}

func num(x float64) string {
	if x == float64(int64(x)) && (x > 1 || x < -1 || x == 0) {
		return fmt.Sprintf("%d", int64(x))
	}
	return fmt.Sprintf("%.4g", x)
}
