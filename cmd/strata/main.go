// Command strata is the append-only history store for assay records.
//
//	… | strata append              read JSONL on stdin, write to date partitions
//	strata query --metric X        range query, JSONL on stdout
//	strata rollup --period week    aggregate to periods
//	strata verdicts                resolve the verdict log to current state
//	strata stat                    what is in the store
//
// Nothing here is quality-specific. Point it at any conforming JSONL stream and
// you get range queries back — which is the test of whether the decomposition
// is real rather than three entry points on a monolith.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/sherzing/assay/internal/evidence"
	"github.com/sherzing/assay/internal/store"
	"github.com/sherzing/assay/pkg/schema"
)

const version = "0.2.1"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "append":
		err = cmdAppend(os.Args[2:])
	case "query":
		err = cmdQuery(os.Args[2:])
	case "rollup":
		err = cmdRollup(os.Args[2:])
	case "verdicts":
		err = cmdVerdicts(os.Args[2:])
	case "stat":
		err = cmdStat(os.Args[2:])
	case "precision":
		err = cmdPrecision(os.Args[2:])
	case "export":
		err = cmdExport(os.Args[2:])
	case "verify":
		err = cmdVerify(os.Args[2:])
	case "promote-check":
		err = cmdPromoteCheck(os.Args[2:])
	case "version", "-v", "--version":
		fmt.Println("strata", version)
	case "help", "-h", "--help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "strata: unknown command %q\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "strata:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `strata — append-only history for assay records

usage:
  … | strata append [--store DIR] [--repo myservice]
  strata query    [--store DIR] [--repo N] [--metric M] [--scope S] [--since D] [--until D]
  strata rollup   [--period day|week|month] [same filters]
  strata verdicts [--store DIR] [--rule R]
  strata stat     [--store DIR]
  strata precision [--store DIR] [--rule R] [--min-judged N]
  strata export   --org NAME [--period YYYY-Qn] [--min-judged 10]
  strata verify   <file.jsonl>
  strata promote-check [--evidence DIR | --store DIR] [--rule R]

dates are YYYY-MM-DD. default store is .assay
everything reads and writes JSONL, so it composes:
  ratchet scan . --emit measures | strata append --repo myservice
  strata query --repo myservice --metric cognitive.p90 | jq -r '[.ts,.value]|@csv'
`)
}

func storeFlag(fs *flag.FlagSet) *string {
	return fs.String("store", ".assay", "store directory")
}

func parseDay(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	return time.Parse("2006-01-02", s)
}

func cmdAppend(args []string) error {
	fs := flag.NewFlagSet("append", flag.ExitOnError)
	dir := storeFlag(fs)
	repo := fs.String("repo", "", "stamp records that carry no repo with this name")
	fs.Parse(args)

	s, err := store.Open(*dir)
	if err != nil {
		return err
	}

	in := os.Stdin
	// Stamping the repo rewrites the stream before it reaches the store, so
	// producers stay ignorant of where their output ends up.
	if *repo != "" {
		pr, pw, perr := os.Pipe()
		if perr != nil {
			return perr
		}
		go func() {
			defer pw.Close()
			enc := schema.NewEncoder(pw)
			defer enc.Flush()
			_ = schema.Decode(os.Stdin, func(rec schema.Record) error {
				switch {
				case rec.Measure != nil:
					if rec.Measure.Repo == "" {
						rec.Measure.Repo = *repo
					}
					return enc.Write(rec.Measure)
				case rec.Finding != nil:
					if rec.Finding.Repo == "" {
						rec.Finding.Repo = *repo
					}
					return enc.Write(rec.Finding)
				case rec.Verdict != nil:
					if rec.Verdict.Repo == "" {
						rec.Verdict.Repo = *repo
					}
					return enc.Write(rec.Verdict)
				}
				return nil
			}, nil)
		}()
		in = pr
	}

	m, f, v, err := s.Append(in)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "appended %d measures, %d findings, %d verdicts to %s\n", m, f, v, *dir)
	return nil
}

type filters struct {
	dir, repo, metric, scope, since, until *string
}

func queryFlags(fs *flag.FlagSet) filters {
	return filters{
		dir:    storeFlag(fs),
		repo:   fs.String("repo", "", "filter by repo"),
		metric: fs.String("metric", "", "filter by metric"),
		scope:  fs.String("scope", "", "project|module|file|function"),
		since:  fs.String("since", "", "YYYY-MM-DD"),
		until:  fs.String("until", "", "YYYY-MM-DD"),
	}
}

func (f filters) query() (store.Query, error) {
	sd, err := parseDay(*f.since)
	if err != nil {
		return store.Query{}, fmt.Errorf("--since: %w", err)
	}
	ud, err := parseDay(*f.until)
	if err != nil {
		return store.Query{}, fmt.Errorf("--until: %w", err)
	}
	if !ud.IsZero() {
		ud = ud.Add(24*time.Hour - time.Nanosecond) // --until includes that whole day
	}
	return store.Query{
		Repo: *f.repo, Metric: *f.metric, Scope: schema.Scope(*f.scope), Since: sd, Until: ud,
	}, nil
}

func cmdQuery(args []string) error {
	fs := flag.NewFlagSet("query", flag.ExitOnError)
	f := queryFlags(fs)
	format := fs.String("format", "jsonl", "jsonl|csv")
	fs.Parse(args)

	s, err := store.Open(*f.dir)
	if err != nil {
		return err
	}
	q, err := f.query()
	if err != nil {
		return err
	}
	ms, err := s.QueryMeasures(q)
	if err != nil {
		return err
	}
	if *format == "csv" {
		fmt.Println("ts,repo,commit,scope,path,metric,value")
		for _, m := range ms {
			fmt.Printf("%s,%s,%s,%s,%s,%s,%g\n",
				m.TS.UTC().Format(time.RFC3339), m.Repo, m.Commit, m.Scope, m.Path, m.Metric, m.Value)
		}
		return nil
	}
	enc := schema.NewEncoder(os.Stdout)
	defer enc.Flush()
	for i := range ms {
		if err := enc.Write(&ms[i]); err != nil {
			return err
		}
	}
	return nil
}

func cmdRollup(args []string) error {
	fs := flag.NewFlagSet("rollup", flag.ExitOnError)
	f := queryFlags(fs)
	period := fs.String("period", "week", "day|week|month")
	format := fs.String("format", "table", "table|jsonl|csv")
	fs.Parse(args)

	s, err := store.Open(*f.dir)
	if err != nil {
		return err
	}
	q, err := f.query()
	if err != nil {
		return err
	}
	bs, err := s.Rollup(q, *period)
	if err != nil {
		return err
	}
	switch *format {
	case "jsonl":
		e := json.NewEncoder(os.Stdout)
		for _, b := range bs {
			if err := e.Encode(b); err != nil {
				return err
			}
		}
	case "csv":
		fmt.Println("start,repo,metric,n,min,mean,max,last")
		for _, b := range bs {
			fmt.Printf("%s,%s,%s,%d,%g,%.4f,%g,%g\n",
				b.Start.Format("2006-01-02"), b.Repo, b.Metric, b.N, b.Min, b.Mean, b.Max, b.Last)
		}
	default:
		if len(bs) == 0 {
			fmt.Println("no data")
			return nil
		}
		fmt.Printf("%-12s %-16s %-22s %5s %8s %8s %8s\n", "start", "repo", "metric", "n", "min", "max", "last")
		for _, b := range bs {
			fmt.Printf("%-12s %-16s %-22s %5d %8g %8g %8g\n",
				b.Start.Format("2006-01-02"), trunc(b.Repo, 16), trunc(b.Metric, 22), b.N, b.Min, b.Max, b.Last)
		}
	}
	return nil
}

func cmdVerdicts(args []string) error {
	fs := flag.NewFlagSet("verdicts", flag.ExitOnError)
	dir := storeFlag(fs)
	rule := fs.String("rule", "", "filter by rule")
	fs.Parse(args)

	s, err := store.Open(*dir)
	if err != nil {
		return err
	}
	vs, err := s.Verdicts()
	if err != nil {
		return err
	}
	fps := make([]string, 0, len(vs))
	for fp := range vs {
		fps = append(fps, fp)
	}
	sort.Strings(fps) // map order is random; a stable stream is diffable

	counts := map[schema.Judgement]int{}
	enc := schema.NewEncoder(os.Stdout)
	defer enc.Flush()
	for _, fp := range fps {
		v := vs[fp]
		if *rule != "" && v.Rule != *rule {
			continue
		}
		counts[v.Verdict]++
		if err := enc.Write(&v); err != nil {
			return err
		}
	}
	fmt.Fprintf(os.Stderr, "%d accepted, %d false-positive, %d wont-fix\n",
		counts[schema.Accepted], counts[schema.FalsePositive], counts[schema.WontFix])
	return nil
}

func cmdStat(args []string) error {
	fs := flag.NewFlagSet("stat", flag.ExitOnError)
	dir := storeFlag(fs)
	fs.Parse(args)

	s, err := store.Open(*dir)
	if err != nil {
		return err
	}
	ms, err := s.QueryMeasures(store.Query{})
	if err != nil {
		return err
	}
	repos := map[string]int{}
	metrics := map[string]int{}
	var first, last time.Time
	for _, m := range ms {
		repos[m.Repo]++
		metrics[m.Metric]++
		if first.IsZero() || m.TS.Before(first) {
			first = m.TS
		}
		if m.TS.After(last) {
			last = m.TS
		}
	}
	// Findings are a separate partition and were previously not counted at
	// all, so a store holding thousands of them reported "measures: 0,
	// verdicts: 0" and looked empty. stat is the "did my pipeline work"
	// command; it has to answer for every record kind the store accepts.
	finds, err := s.QueryFindings(store.Query{})
	if err != nil {
		return err
	}
	rules := map[string]int{}
	judged := 0
	for _, f := range finds {
		rules[f.Rule]++
		repos[f.Repo]++
		if f.Verdict != "" {
			judged++
		}
	}
	delete(repos, "") // findings emitted without --repo would otherwise show as blank

	vs, _ := s.Verdicts()
	fmt.Printf("store:    %s\n", s.Root)
	fmt.Printf("measures: %d", len(ms))
	if len(ms) > 0 {
		fmt.Printf("  %s → %s", first.Format("2006-01-02"), last.Format("2006-01-02"))
	}
	fmt.Printf("\nfindings: %d", len(finds))
	if len(finds) > 0 {
		// Unjudged is called out because it is the answer to the question stat
		// leads to: precision reports nothing until findings carry verdicts.
		fmt.Printf("  (%d judged, %d unjudged)", judged, len(finds)-judged)
	}
	fmt.Printf("\nverdicts: %d\n", len(vs))
	if len(repos) > 0 {
		fmt.Printf("repos:    %s\n", strings.Join(sortedKeys(repos), ", "))
	}
	if len(metrics) > 0 {
		fmt.Printf("metrics:  %s\n", strings.Join(sortedKeys(metrics), ", "))
	}
	if len(rules) > 0 {
		fmt.Printf("rules:    %s\n", strings.Join(sortedKeys(rules), ", "))
	}
	return nil
}

func sortedKeys(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

// cmdPrecision reports what the evidence says about each rule.
//
// This is the number that decides whether a rule deserves to ship. A rule that
// cannot show its true-positive rate across real repositories has not earned a
// place in anyone's CI, and no vendor publishes this because it needs many
// codebases and many independent judgements.
func cmdPrecision(args []string) error {
	fs := flag.NewFlagSet("precision", flag.ExitOnError)
	dir := storeFlag(fs)
	rule := fs.String("rule", "", "filter to one rule")
	minJudged := fs.Int("min-judged", 1, "hide rules with fewer judged findings")
	asJSON := fs.Bool("json", false, "emit JSON")
	fs.Parse(args)

	s, err := store.Open(*dir)
	if err != nil {
		return err
	}
	rows, err := s.Precision()
	if err != nil {
		return err
	}
	var keep []store.RulePrecision
	for _, r := range rows {
		if *rule != "" && r.Rule != *rule {
			continue
		}
		if r.Judged < *minJudged {
			continue
		}
		keep = append(keep, r)
	}
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(keep)
	}
	if len(keep) == 0 {
		fmt.Println("no rules with enough judged findings yet")
		fmt.Println("evidence accrues as findings are fixed or annotated — come back after some work lands")
		return nil
	}

	fmt.Printf("%-30s %6s %8s %5s %7s %6s %6s %5s %4s\n",
		"rule", "prec", "carriage", "fixed", "carried", "fp", "unjud", "repos", "orgs")
	fmt.Println(strings.Repeat("─", 89))
	for _, r := range keep {
		prec := "  –   "
		if r.Judged > 0 {
			prec = fmt.Sprintf("%6.2f", r.Precision)
		}
		carr := "   –    "
		if r.Fixed+r.Carried > 0 {
			carr = fmt.Sprintf("%7.0f%%", r.Carriage*100)
		}
		fmt.Printf("%-30s %s %s %5d %7d %6d %6d %5d %4d\n",
			trunc(r.Rule, 30), prec, carr, r.Fixed, r.Carried, r.FalsePositive, r.Unjudged, r.Repos, r.Orgs)
	}

	fmt.Println()
	for _, r := range keep {
		switch {
		case r.Judged >= 5 && r.Precision < 0.5:
			fmt.Printf("  %s — precision %.2f over %d judged. Mostly wrong; fix the rule or drop it.\n",
				r.Rule, r.Precision, r.Judged)
		case r.Fixed+r.Carried >= 5 && r.Carriage > 0.8:
			fmt.Printf("  %s — %.0f%% of confirmed findings are carried, not fixed. The rule is right\n"+
				"      and the problem may be one the ecosystem has given up on. Consider shipping it\n"+
				"      as informational rather than as a gate.\n", r.Rule, r.Carriage*100)
		case r.Coverage < 0.1 && r.Seen >= 20:
			fmt.Printf("  %s — only %.0f%% of %d findings judged. Too little evidence to trust the number.\n",
				r.Rule, r.Coverage*100, r.Seen)
		}
	}
	return nil
}

// cmdExport produces the aggregate an organisation can share.
//
// Refuses without an --org, because unattributed evidence cannot count toward
// the multi-organisation bar and would just be noise in the corpus.
func cmdExport(args []string) error {
	fs := flag.NewFlagSet("export", flag.ExitOnError)
	dir := storeFlag(fs)
	org := fs.String("org", "", "organisation to attribute this evidence to (required)")
	period := fs.String("period", "", "YYYY-Qn (default: current quarter)")
	minJudged := fs.Int("min-judged", 10, "skip rules with fewer judged findings")
	rule := fs.String("rule", "", "export only this rule")
	fs.Parse(args)

	s, err := store.Open(*dir)
	if err != nil {
		return err
	}
	rows, err := s.Precision()
	if err != nil {
		return err
	}
	opt := evidence.Options{Org: *org, Period: *period, MinJudged: *minJudged}
	if *rule != "" {
		opt.Rules = map[string]bool{*rule: true}
	}
	recs, skipped, err := evidence.Build(rows, opt)
	if err != nil {
		return err
	}

	// Self-verify before anything is written. The exporter and the verifier
	// disagreeing is exactly the bug that would leak something.
	enc := json.NewEncoder(os.Stdout)
	for _, r := range recs {
		raw, _ := json.Marshal(r)
		if probs := evidence.Verify(raw); evidence.Fatal(probs) {
			return fmt.Errorf("refusing to emit %s — the exporter produced something the verifier rejects:\n  %v",
				r.Rule, probs[0])
		}
		if err := enc.Encode(r); err != nil {
			return err
		}
	}

	fmt.Fprintf(os.Stderr, "\n%d records exported", len(recs))
	if len(skipped) > 0 {
		fmt.Fprintf(os.Stderr, ", %d skipped as too thin:\n", len(skipped))
		for _, sk := range skipped {
			fmt.Fprintf(os.Stderr, "  %s\n", sk)
		}
	} else {
		fmt.Fprintln(os.Stderr)
	}
	fmt.Fprintf(os.Stderr, "\nThis carries counts only — no paths, fingerprints, repo names or reasons.\n")
	fmt.Fprintf(os.Stderr, "Read it before you send it; `strata verify` checks it again on arrival.\n")
	return nil
}

// cmdVerify checks a submitted evidence file. Intended for CI on the receiving
// side, and for contributors to run before anything leaves their network.
func cmdVerify(args []string) error {
	fs := flag.NewFlagSet("verify", flag.ExitOnError)
	quiet := fs.Bool("quiet", false, "only report problems")
	fs.Parse(args)
	if fs.NArg() == 0 {
		return fmt.Errorf("usage: strata verify <file.jsonl>")
	}

	bad := 0
	for _, path := range fs.Args() {
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		byLine := evidence.VerifyStream(data)
		lines := 0
		for _, l := range strings.Split(string(data), "\n") {
			if t := strings.TrimSpace(l); t != "" && !strings.HasPrefix(t, "#") {
				lines++
			}
		}
		fatal := 0
		nums := make([]int, 0, len(byLine))
		for n := range byLine {
			nums = append(nums, n)
		}
		sort.Ints(nums)
		for _, n := range nums {
			for _, p := range byLine[n] {
				if p.Fatal {
					fatal++
				}
				fmt.Printf("%s:%d  %s\n", path, n, p)
			}
		}
		if fatal > 0 {
			bad++
			fmt.Printf("%s: %d records, %d problems blocking\n", path, lines, fatal)
		} else if !*quiet {
			fmt.Printf("%s: %d records, safe to share\n", path, lines)
		}
	}
	if bad > 0 {
		os.Exit(1)
	}
	return nil
}

// cmdPromoteCheck answers whether a rule has earned its place in the core pack.
//
// Reads the shared evidence corpus by default, because promotion is a question
// about what many organisations have found, not about what one has. Falls back
// to the local store so a contributor can see their own standing.
func cmdPromoteCheck(args []string) error {
	fs := flag.NewFlagSet("promote-check", flag.ExitOnError)
	evDir := fs.String("evidence", "", "directory of submitted evidence files")
	dir := storeFlag(fs)
	rule := fs.String("rule", "", "check one rule")
	org := fs.String("org", "", "org label when reading the local store")
	strict := fs.Bool("strict", false, "exit non-zero unless every checked rule is promotable")
	asJSON := fs.Bool("json", false, "emit JSON")
	fs.Parse(args)

	var recs []evidence.Record
	source := ""
	switch {
	case *evDir != "":
		var errs []error
		recs, errs = evidence.LoadDir(*evDir)
		for _, e := range errs {
			fmt.Fprintf(os.Stderr, "warning: %v\n", e)
		}
		source = fmt.Sprintf("%d records from %s", len(recs), *evDir)
	default:
		s, err := store.Open(*dir)
		if err != nil {
			return err
		}
		rows, err := s.Precision()
		if err != nil {
			return err
		}
		o := *org
		if o == "" {
			o = "local"
		}
		// MinJudged 1 here: the point is to show standing, including for rules
		// that are nowhere near the bar yet.
		recs, _, err = evidence.Build(rows, evidence.Options{Org: o, MinJudged: 1})
		if err != nil {
			return err
		}
		source = fmt.Sprintf("local store %s (one organisation — the multi-org bar cannot be met from here)", *dir)
	}
	if len(recs) == 0 {
		fmt.Println("no evidence found")
		return nil
	}

	t := evidence.DefaultThresholds()
	corpora := evidence.Aggregate(recs)
	names := make([]string, 0, len(corpora))
	for n := range corpora {
		if *rule != "" && n != *rule {
			continue
		}
		names = append(names, n)
	}
	sort.Strings(names)

	var assessments []evidence.Assessment
	for _, n := range names {
		assessments = append(assessments, evidence.Assess(*corpora[n], t))
	}

	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(assessments)
	}

	fmt.Printf("source: %s\n", source)
	fmt.Printf("bar: >=%d orgs, >=%d judged, >=%d repos, precision >=%.2f\n\n",
		t.MinOrgs, t.MinJudged, t.MinRepos, t.MinPrecision)

	blocked := 0
	for _, a := range assessments {
		c := a.Corpus
		mark := map[evidence.Outcome]string{
			evidence.Promote:              "PROMOTE",
			evidence.PromoteInformational: "PROMOTE (informational)",
			evidence.NotYet:               "not yet",
			evidence.Reject:               "REJECT",
		}[a.Outcome]
		if a.Outcome != evidence.Promote && a.Outcome != evidence.PromoteInformational {
			blocked++
		}

		fmt.Printf("%-30s %s\n", trunc(c.Rule, 30), mark)
		fmt.Printf("%-30s   %d orgs · %d judged · %d repos · precision %.2f · carriage %.0f%%\n",
			"", len(c.Orgs), c.Judged, c.Repos, c.Precision, c.Carriage*100)
		for _, u := range a.Unmet {
			fmt.Printf("%-30s   ✗ %s\n", "", u)
		}
		for _, n := range a.Notes {
			fmt.Printf("%-30s   → %s\n", "", wrapAt(n, 74, 35))
		}
		fmt.Println()
	}
	if *strict && blocked > 0 {
		os.Exit(1)
	}
	return nil
}

// wrapAt keeps long guidance readable in a terminal without a dependency.
func wrapAt(s string, width, indent int) string {
	words := strings.Fields(s)
	var lines []string
	cur := ""
	for _, w := range words {
		if cur != "" && len(cur)+1+len(w) > width {
			lines = append(lines, cur)
			cur = w
			continue
		}
		if cur == "" {
			cur = w
		} else {
			cur += " " + w
		}
	}
	if cur != "" {
		lines = append(lines, cur)
	}
	return strings.Join(lines, "\n"+strings.Repeat(" ", indent)+"  ")
}
