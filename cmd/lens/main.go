// Command lens makes an assay stream readable by a person.
//
//	lens top      what is worst right now
//	lens trend    how a metric moved, as a sparkline
//	lens diff     what changed between two points
//	lens compare  the same metric across repos
//
// It reads JSONL on stdin, so it works on any conforming stream — not just one
// that came out of strata. That is the test of whether this is a real tool or a
// strata subcommand wearing a disguise:
//
//	strata query --repo myservice --metric cognitive | lens top
//	ratchet scan . --emit measures | lens top --metric cognitive
//
// grep and jq can do all of this. They just cannot do it at a glance, and the
// point of this tool is the glance.
package main

import (
	"flag"
	"fmt"
	"math"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/sherzing/assay/internal/store"
	"github.com/sherzing/assay/pkg/schema"
)

const version = "0.1.0"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "top":
		err = cmdTop(os.Args[2:])
	case "trend":
		err = cmdTrend(os.Args[2:])
	case "diff":
		err = cmdDiff(os.Args[2:])
	case "compare":
		err = cmdCompare(os.Args[2:])
	case "calibrate":
		err = cmdCalibrate(os.Args[2:])
	case "version", "-v", "--version":
		fmt.Println("lens", version)
	case "help", "-h", "--help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "lens: unknown command %q\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "lens:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `lens — read an assay stream at a glance

usage:
  lens top     [--metric M] [--scope S] [--n 15] [--repo R]     worst right now
  lens trend   [--metric M] [--repo R] [--period week]          sparkline over time
  lens diff    --since DATE [--metric M] [--repo R]             what changed
  lens compare [--metric M] [--scope project]                   repos side by side
  lens calibrate [--store DIR]                                  derive bands from your own code

input: JSONL on stdin, or --store DIR to read a strata store

  strata query --repo myservice --metric cognitive | lens top
  ratchet scan . --emit measures | lens top --metric cognitive --n 10
  lens trend --store .assay --repo myservice --metric cognitive.p90
`)
}

// read takes measures from stdin or from a store, so every command works both
// as a pipe stage and standalone.
func read(dir string, q store.Query) ([]schema.Measure, error) {
	if dir != "" {
		s, err := store.Open(dir)
		if err != nil {
			return nil, err
		}
		return s.QueryMeasures(q)
	}
	st, _ := os.Stdin.Stat()
	if st != nil && st.Mode()&os.ModeCharDevice != 0 {
		return nil, fmt.Errorf("no input: pipe JSONL in, or pass --store DIR")
	}
	var out []schema.Measure
	err := schema.Decode(os.Stdin, func(rec schema.Record) error {
		if rec.Measure != nil {
			out = append(out, *rec.Measure)
		}
		return nil
	}, nil)
	sort.Slice(out, func(i, j int) bool { return out[i].TS.Before(out[j].TS) })
	return out, err
}

func inputFlags(fs *flag.FlagSet) (*string, *string, *string, *string) {
	return fs.String("store", "", "read a strata store instead of stdin"),
		fs.String("repo", "", "filter by repo"),
		fs.String("metric", "", "filter by metric"),
		fs.String("scope", "", "project|module|file|function|class")
}

func filter(ms []schema.Measure, repo, metric, scope string) []schema.Measure {
	var out []schema.Measure
	for _, m := range ms {
		if repo != "" && m.Repo != repo {
			continue
		}
		if metric != "" && m.Metric != metric {
			continue
		}
		if scope != "" && string(m.Scope) != scope {
			continue
		}
		out = append(out, m)
	}
	return out
}

// ---------- top ----------

func cmdTop(args []string) error {
	fs := flag.NewFlagSet("top", flag.ExitOnError)
	dir, repo, metric, scope := inputFlags(fs)
	n := fs.Int("n", 15, "how many")
	plain := fs.Bool("plain", false, "values only, no interpretation (for scripting)")
	fs.Parse(args)
	if *metric == "" {
		*metric = "cognitive"
	}
	if *scope == "" {
		*scope = "function"
	}

	// Read every metric at this scope: the chosen one to rank by, the others to
	// explain with.
	ms0, err := read(*dir, store.Query{Repo: *repo, Scope: schema.Scope(*scope)})
	if err != nil {
		return err
	}
	ms := filter(ms0, *repo, *metric, *scope)
	if len(ms) == 0 {
		return fmt.Errorf("no %s measures at scope %s", *metric, *scope)
	}

	// Latest value per path — a store holds history, and "worst right now"
	// means the most recent reading, not every reading ever taken.
	latest := map[string]schema.Measure{}
	for _, m := range ms {
		k := m.Repo + "\x00" + m.Path
		if cur, ok := latest[k]; !ok || m.TS.After(cur.TS) {
			latest[k] = m
		}
	}
	list := make([]schema.Measure, 0, len(latest))
	for _, m := range latest {
		list = append(list, m)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Value > list[j].Value })
	if len(list) > *n {
		list = list[:*n]
	}

	// Context: a value only means something against the codebase it came from.
	all := make([]float64, 0, len(latest))
	for _, m := range latest {
		all = append(all, m.Value)
	}
	med := median(all)

	// Companion metrics let us say WHY a function is hard, which decides the fix.
	cyc := latestBy(ms0, "cyclomatic")
	nest := latestBy(ms0, "nesting")

	maxV := list[0].Value
	fmt.Printf("worst %d by %s (%s scope)  ·  median is %s\n\n", len(list), *metric, *scope, num(med))
	for _, m := range list {
		label := m.Path
		if *repo == "" {
			label = m.Repo + " " + label
		}
		fmt.Printf("%7s %s  %s\n", num(m.Value), bar(m.Value, maxV, 22), elide(label, 66))
		if *plain {
			continue
		}
		verdict, action := classify(*metric, m.Path, m.Value)
		var notes []string
		if verdict != "" {
			notes = append(notes, verdict)
		}
		if med > 0 {
			notes = append(notes, fmt.Sprintf("%.0fx the median", m.Value/med))
		}
		k := m.Repo + "\x00" + m.Path
		if d := driver(cyc[k], nest[k]); d != "" {
			notes = append(notes, d)
		}
		if len(notes) > 0 {
			fmt.Printf("        ↳ %s\n", strings.Join(notes, " · "))
		}
		if action != "" {
			fmt.Printf("        ↳ %s\n", action)
		}
	}
	if !*plain {
		fmt.Printf("\nwhat to do: start at the top. Fix one, run `ratchet baseline --tighten`,\n")
		fmt.Printf("and the improvement is locked in so it cannot regress.\n")
	}
	return nil
}

// latestBy indexes the most recent value of one metric by repo+path, so top can
// say whether branching or nesting is the real problem.
func latestBy(ms []schema.Measure, metric string) map[string]float64 {
	out := map[string]float64{}
	seen := map[string]time.Time{}
	for _, m := range ms {
		if m.Metric != metric {
			continue
		}
		k := m.Repo + "\x00" + m.Path
		if t, ok := seen[k]; !ok || m.TS.After(t) {
			seen[k], out[k] = m.TS, m.Value
		}
	}
	return out
}

// ---------- trend ----------

func cmdTrend(args []string) error {
	fs := flag.NewFlagSet("trend", flag.ExitOnError)
	dir, repo, metric, scope := inputFlags(fs)
	period := fs.String("period", "week", "day|week|month")
	plain := fs.Bool("plain", false, "values only, no interpretation (for scripting)")
	fs.Parse(args)
	if *metric == "" {
		*metric = "cognitive.p90"
	}
	if *scope == "" {
		*scope = "project"
	}

	ms, err := read(*dir, store.Query{Repo: *repo, Metric: *metric, Scope: schema.Scope(*scope)})
	if err != nil {
		return err
	}
	ms = filter(ms, *repo, *metric, *scope)
	if len(ms) == 0 {
		return fmt.Errorf("no %s measures", *metric)
	}

	byRepo := map[string][]schema.Measure{}
	for _, m := range ms {
		byRepo[m.Repo] = append(byRepo[m.Repo], m)
	}
	fmt.Printf("%s over time, by %s\n\n", *metric, *period)
	for _, r := range sortedKeys(byRepo) {
		pts := bucketLast(byRepo[r], *period)
		if len(pts) == 0 {
			continue
		}
		vals := make([]float64, len(pts))
		for i, p := range pts {
			vals[i] = p.v
		}
		first, last := vals[0], vals[len(vals)-1]
		fmt.Printf("%-16s %s  %s → %s  %s\n", elide(r, 16), spark(vals), num(first), num(last), delta(first, last))
		if len(pts) > 1 {
			fmt.Printf("%-16s %s → %s  (%d points)\n", "",
				pts[0].t.Format("2006-01-02"), pts[len(pts)-1].t.Format("2006-01-02"), len(pts))
		}
		if !*plain {
			fmt.Printf("%-16s ↳ %s\n", "", trendVerdict(vals))
			if v, _ := classify(*metric, "", last); v != "" {
				fmt.Printf("%-16s ↳ current value is %q by the usual thresholds\n", "", v)
			}
		}
		fmt.Println()
	}
	return nil
}

type pt struct {
	t time.Time
	v float64
}

// bucketLast takes the final reading in each period — the state the codebase
// was left in, which is what a trend should show. A mean over a period during
// which the value changed is a number that never existed.
func bucketLast(ms []schema.Measure, period string) []pt {
	key := func(t time.Time) time.Time {
		u := t.UTC()
		d := time.Date(u.Year(), u.Month(), u.Day(), 0, 0, 0, 0, time.UTC)
		switch period {
		case "day":
			return d
		case "month":
			return time.Date(u.Year(), u.Month(), 1, 0, 0, 0, 0, time.UTC)
		default:
			return d.AddDate(0, 0, -(int(d.Weekday()+6) % 7))
		}
	}
	acc := map[time.Time]pt{}
	for _, m := range ms {
		k := key(m.TS)
		if cur, ok := acc[k]; !ok || m.TS.After(cur.t) {
			acc[k] = pt{m.TS, m.Value}
		}
	}
	out := make([]pt, 0, len(acc))
	for k, v := range acc {
		out = append(out, pt{k, v.v})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].t.Before(out[j].t) })
	return out
}

// ---------- diff ----------

func cmdDiff(args []string) error {
	fs := flag.NewFlagSet("diff", flag.ExitOnError)
	dir, repo, metric, scope := inputFlags(fs)
	since := fs.String("since", "", "YYYY-MM-DD (required)")
	n := fs.Int("n", 20, "how many movers to show")
	fs.Parse(args)
	if *since == "" {
		return fmt.Errorf("--since YYYY-MM-DD is required")
	}
	cut, err := time.Parse("2006-01-02", *since)
	if err != nil {
		return fmt.Errorf("--since: %w", err)
	}
	if *scope == "" {
		*scope = "function"
	}
	if *metric == "" {
		*metric = "cognitive"
	}

	ms, err := read(*dir, store.Query{Repo: *repo, Metric: *metric, Scope: schema.Scope(*scope)})
	if err != nil {
		return err
	}
	ms = filter(ms, *repo, *metric, *scope)

	// before = last reading at or before the cut; after = last reading overall
	before, after := map[string]schema.Measure{}, map[string]schema.Measure{}
	for _, m := range ms {
		k := m.Repo + "\x00" + m.Path
		if !m.TS.After(cut) {
			if c, ok := before[k]; !ok || m.TS.After(c.TS) {
				before[k] = m
			}
		}
		if c, ok := after[k]; !ok || m.TS.After(c.TS) {
			after[k] = m
		}
	}

	type mover struct {
		path      string
		from, to  float64
		isNew     bool
		isRemoved bool
	}
	var movers []mover
	for k, a := range after {
		if b, ok := before[k]; ok {
			if a.Value != b.Value {
				movers = append(movers, mover{k, b.Value, a.Value, false, false})
			}
		} else {
			movers = append(movers, mover{k, 0, a.Value, true, false})
		}
	}
	for k, b := range before {
		if _, ok := after[k]; !ok {
			movers = append(movers, mover{k, b.Value, 0, false, true})
		}
	}
	if len(movers) == 0 {
		fmt.Printf("no change in %s since %s\n", *metric, *since)
		return nil
	}
	sort.Slice(movers, func(i, j int) bool {
		return math.Abs(movers[i].to-movers[i].from) > math.Abs(movers[j].to-movers[j].from)
	})

	worse, better := 0, 0
	for _, m := range movers {
		if m.to > m.from {
			worse++
		} else {
			better++
		}
	}
	fmt.Printf("%s since %s — %d worse, %d better\n\n", *metric, *since, worse, better)
	if len(movers) > *n {
		movers = movers[:*n]
	}
	for _, m := range movers {
		label := strings.Replace(m.path, "\x00", " ", 1)
		tag := ""
		switch {
		case m.isNew:
			tag = " (new)"
		case m.isRemoved:
			tag = " (gone)"
		}
		fmt.Printf("  %6s → %-6s %-8s %s%s\n", num(m.from), num(m.to), delta(m.from, m.to), elide(label, 60), tag)
	}
	return nil
}

// ---------- compare ----------

func cmdCompare(args []string) error {
	fs := flag.NewFlagSet("compare", flag.ExitOnError)
	dir, repo, metric, scope := inputFlags(fs)
	fs.Parse(args)
	if *scope == "" {
		*scope = "project"
	}

	ms, err := read(*dir, store.Query{Repo: *repo, Metric: *metric, Scope: schema.Scope(*scope)})
	if err != nil {
		return err
	}
	ms = filter(ms, *repo, *metric, *scope)
	if len(ms) == 0 {
		return fmt.Errorf("no measures at scope %s", *scope)
	}

	// latest value per repo+metric
	latest := map[string]map[string]float64{}
	for _, m := range ms {
		if latest[m.Metric] == nil {
			latest[m.Metric] = map[string]float64{}
		}
		latest[m.Metric][m.Repo] = m.Value
	}
	repos := map[string]bool{}
	for _, r := range latest {
		for k := range r {
			repos[k] = true
		}
	}
	rs := make([]string, 0, len(repos))
	for r := range repos {
		rs = append(rs, r)
	}
	sort.Strings(rs)

	fmt.Printf("%-24s", "metric")
	for _, r := range rs {
		fmt.Printf("%14s", elide(r, 13))
	}
	fmt.Println()
	fmt.Println(strings.Repeat("─", 24+14*len(rs)))
	for _, mt := range sortedFloatKeys(latest) {
		fmt.Printf("%-24s", elide(mt, 23))
		for _, r := range rs {
			if v, ok := latest[mt][r]; ok {
				fmt.Printf("%14s", num(v))
			} else {
				fmt.Printf("%14s", "–")
			}
		}
		fmt.Println()
	}
	return nil
}

// ---------- presentation ----------

var blocks = []rune("▁▂▃▄▅▆▇█")

// spark renders a sparkline. Scaled to the series' own min/max, because the
// question a trend answers is "did this move", not "is this big".
func spark(v []float64) string {
	if len(v) == 0 {
		return ""
	}
	lo, hi := v[0], v[0]
	for _, x := range v {
		lo, hi = math.Min(lo, x), math.Max(hi, x)
	}
	var b strings.Builder
	for _, x := range v {
		if hi == lo {
			b.WriteRune(blocks[len(blocks)/2]) // flat series: mid-height, not empty
			continue
		}
		i := int((x - lo) / (hi - lo) * float64(len(blocks)-1))
		b.WriteRune(blocks[i])
	}
	return b.String()
}

func bar(v, max float64, width int) string {
	if max <= 0 {
		return strings.Repeat(" ", width)
	}
	n := int(v / max * float64(width))
	return strings.Repeat("█", n) + strings.Repeat("·", width-n)
}

// delta shows direction. Higher is worse for every metric assay emits, so up
// is flagged and down is not.
func delta(from, to float64) string {
	switch {
	case to > from:
		if from == 0 {
			return "▲ new"
		}
		return fmt.Sprintf("▲ +%.0f%%", (to-from)/math.Abs(from)*100)
	case to < from:
		if to == 0 {
			return "▼ gone"
		}
		return fmt.Sprintf("▼ %.0f%%", (to-from)/math.Abs(from)*100)
	}
	return "  flat"
}

// num formats a value for a human: integers stay integers, and a float gets two
// decimals rather than the seventeen that %g is happy to print.
func num(v float64) string {
	if v == math.Trunc(v) && math.Abs(v) < 1e9 {
		return fmt.Sprintf("%.0f", v)
	}
	return fmt.Sprintf("%.2f", v)
}

func elide(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n <= 1 {
		return s[:n]
	}
	return "…" + s[len(s)-n+1:] // keep the tail: filenames matter more than paths
}

func sortedKeys[T any](m map[string]T) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedFloatKeys(m map[string]map[string]float64) []string {
	return sortedKeys(m)
}

// ---------- interpretation ----------
//
// A number alone tells nobody what to do. These turn a value into a judgement
// and a next action, which is the whole difference between a viewer and
// something worth opening.
//
// The bands below are conventional, not laws. They are the thresholds most
// tools and reviewers converge on, and they exist so a reader who has never
// seen a cognitive-complexity score knows whether 27 is fine or alarming.

type band struct {
	limit  float64
	label  string
	action string
}

// Bands are PERCENTILES OF REAL CODE, not folklore.
//
// The Go set is derived from 3,053 functions across two production services
// (service-a, 5 years; service-b, 3 months). The two distributions agree closely,
// which is what makes them usable as a default:
//
//	             p50   p90   p99   max
//	cognitive      1     5    15    37
//	cyclomatic     2     6    12    31
//	nesting        1     2     3     6
//
// So "elevated" starts at p90 — the top tenth — and "outlier" at p99. A function
// over the p99 line is in the worst 1% of code we have measured, which is a
// defensible reason to send someone to look at it.
//
// These are a starting point, not a law. Two supervised Go services is not all of
// Go. Run `lens calibrate --store .assay` to derive bands from your own corpus;
// that is strictly better than anything shipped here.
var bandsByLang = map[string]map[string][]band{
	"go": {
		"cognitive": {
			{6, "typical", ""},
			{16, "elevated", "read it; if you cannot hold it in your head, split it"},
			{1e9, "outlier (worst 1%)", "extract the nested branches into named functions"},
		},
		"cyclomatic": {
			{7, "typical", ""},
			{13, "elevated", "check the tests cover each branch"},
			{1e9, "outlier (worst 1%)", "too many paths to test honestly; decompose"},
		},
		"nesting": {
			{3, "typical", ""},
			{4, "elevated", "invert conditions and return early"},
			{1e9, "outlier (worst 1%)", "invert conditions and return early"},
		},
	},
	// Fallback for languages we have not calibrated. Conventional thresholds,
	// deliberately looser, because guessing tight is worse than guessing loose:
	// a noisy band trains people to ignore the column.
	"": {
		"cognitive": {
			{15, "typical", ""},
			{25, "elevated", "read it; if you cannot hold it in your head, split it"},
			{1e9, "high", "extract the nested branches into named functions"},
		},
		"cyclomatic": {
			{10, "typical", ""},
			{20, "elevated", "check the tests cover each branch"},
			{1e9, "high", "too many paths to test honestly; decompose"},
		},
		"nesting": {
			{4, "typical", ""},
			{6, "elevated", "invert conditions and return early"},
			{1e9, "high", "invert conditions and return early"},
		},
	},
}

// langOf guesses the language from a measured path, so a Go function is judged
// against Go norms and a C# one is not.
func langOf(path string) string {
	switch {
	case strings.Contains(path, ".go:"), strings.HasSuffix(path, ".go"):
		return "go"
	case strings.Contains(path, ".dart:"), strings.HasSuffix(path, ".dart"):
		return "dart"
	}
	return ""
}

// classify returns a label and a suggested action for a metric value, judged
// against the norms of the language it came from.
func classify(metric, path string, v float64) (string, string) {
	base := metric
	if i := strings.IndexByte(base, '.'); i > 0 {
		base = base[:i] // cognitive.p90 -> cognitive
	}
	set, ok := bandsByLang[langOf(path)]
	if !ok {
		set = bandsByLang[""]
	}
	bs, ok := set[base]
	if !ok {
		return "", ""
	}
	for _, b := range bs {
		if v < b.limit {
			return b.label, b.action
		}
	}
	return "", ""
}

// driver names what is making a function hard: branching or nesting. They call
// for different fixes, so saying which one dominates saves the reader a trip.
func driver(cyc, nest float64) string {
	switch {
	case nest >= 4 && cyc < 15:
		return "deep nesting"
	case cyc >= 15 && nest < 3:
		return "many branches"
	case cyc >= 15 && nest >= 4:
		return "both branching and nesting"
	}
	return ""
}

func median(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	c := append([]float64(nil), v...)
	sort.Float64s(c)
	return c[len(c)/2]
}

// trendVerdict compares the recent window against the whole series. A reversal
// — long improvement followed by recent worsening — is the signal most worth
// surfacing, and the one a raw sparkline hides.
func trendVerdict(vals []float64) string {
	n := len(vals)
	if n < 4 {
		return "too few points to read a trend"
	}
	w := n / 4
	if w < 2 {
		w = 2
	}
	first, last := vals[0], vals[n-1]
	recentFrom, recentTo := vals[n-w-1], vals[n-1]

	pct := func(a, b float64) float64 {
		if a == 0 {
			return 0
		}
		return (b - a) / math.Abs(a) * 100
	}
	overall, recent := pct(first, last), pct(recentFrom, recentTo)

	switch {
	case overall < -10 && recent > 10:
		return fmt.Sprintf("REVERSAL: improved %.0f%% overall, but the last %d points rose %.0f%% — worth investigating",
			-overall, w, recent)
	case recent > 20:
		return fmt.Sprintf("worsening: up %.0f%% over the last %d points", recent, w)
	case overall > 20:
		return fmt.Sprintf("worsening: up %.0f%% across the series", overall)
	case overall < -20:
		return fmt.Sprintf("improving: down %.0f%% across the series", -overall)
	case math.Abs(overall) <= 10 && math.Abs(recent) <= 10:
		return "stable — no action needed"
	}
	return fmt.Sprintf("drifting: %+.0f%% overall, %+.0f%% recently", overall, recent)
}

// ---------- calibrate ----------

// cmdCalibrate derives bands from the user's own corpus.
//
// Shipped defaults come from two Go services. That is a reasonable starting
// point and a poor universal truth. A team with their own history can do better
// in one command, and a band grounded in your codebase is far easier to defend
// in review than a number someone read in a book.
func cmdCalibrate(args []string) error {
	fs := flag.NewFlagSet("calibrate", flag.ExitOnError)
	dir, repo, _, _ := inputFlags(fs)
	lang := fs.String("lang", "", "label the output with this language")
	fs.Parse(args)

	ms, err := read(*dir, store.Query{Repo: *repo, Scope: schema.ScopeFunction})
	if err != nil {
		return err
	}
	byMetric := map[string][]float64{}
	repos := map[string]bool{}
	for _, m := range ms {
		if m.Scope != schema.ScopeFunction {
			continue
		}
		if *repo != "" && m.Repo != *repo {
			continue
		}
		byMetric[m.Metric] = append(byMetric[m.Metric], m.Value)
		repos[m.Repo] = true
	}
	if len(byMetric) == 0 {
		return fmt.Errorf("no function-scope measures found")
	}

	fmt.Printf("calibrated from %d repos: %s\n\n", len(repos), strings.Join(sortedKeys(repos), ", "))
	fmt.Printf("%-12s %8s %6s %6s %6s %6s %7s\n", "metric", "n", "p50", "p75", "p90", "p99", "max")
	for _, mt := range sortedKeys(byMetric) {
		v := byMetric[mt]
		sort.Float64s(v)
		fmt.Printf("%-12s %8d %6s %6s %6s %6s %7s\n", mt, len(v),
			num(q(v, .50)), num(q(v, .75)), num(q(v, .90)), num(q(v, .99)), num(v[len(v)-1]))
	}

	fmt.Printf("\nproposed bands — typical below p90, elevated below p99, outlier above:\n\n")
	label := *lang
	if label == "" {
		label = "yourlang"
	}
	fmt.Printf("\t%q: {\n", label)
	for _, mt := range sortedKeys(byMetric) {
		if _, known := bandsByLang["go"][mt]; !known {
			continue // only emit bands for metrics we know how to act on
		}
		v := byMetric[mt]
		sort.Float64s(v)
		fmt.Printf("\t\t%q: {{%.0f, \"typical\", \"\"}, {%.0f, \"elevated\", \"…\"}, {1e9, \"outlier\", \"…\"}},\n",
			mt, q(v, .90)+1, q(v, .99)+1)
	}
	fmt.Printf("\t},\n")
	fmt.Printf("\nPaste into bandsByLang in cmd/lens/main.go, or keep it as the number\n")
	fmt.Printf("you quote in review. Sanity-check it: if p90 equals p99 your corpus\n")
	fmt.Printf("is too small or too uniform to calibrate from.\n")
	return nil
}

func q(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	return sorted[min(int(p*float64(len(sorted)-1)), len(sorted)-1)]
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
