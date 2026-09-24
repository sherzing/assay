// Command ratchet measures Go code quality and holds a line against regression.
//
//	ratchet scan     ./...        measure, and report findings
//	ratchet baseline ./...        record today's state as tolerated
//	ratchet check    ./...        fail only if something got worse
//	ratchet history  ./...        emit a metric series over git history
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sherzing/assay/internal/analyze"
	"github.com/sherzing/assay/internal/baseline"
	"github.com/sherzing/assay/internal/learn"
	"github.com/sherzing/assay/internal/model"
	"github.com/sherzing/assay/internal/report"
	"github.com/sherzing/assay/internal/verdict"
	"github.com/sherzing/assay/pkg/schema"
)

const version = "0.1.0"

const defaultBaselineFile = ".ratchet-baseline.json"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "scan":
		err = cmdScan(os.Args[2:])
	case "baseline":
		err = cmdBaseline(os.Args[2:])
	case "check":
		err = cmdCheck(os.Args[2:])
	case "history":
		err = cmdHistory(os.Args[2:])
	case "import":
		err = cmdImport(os.Args[2:])
	case "rules":
		cmdRules()
	case "exceptions":
		err = cmdExceptions(os.Args[2:])
	case "learn":
		err = cmdLearn(os.Args[2:])
	case "version", "-v", "--version":
		fmt.Println("ratchet", version)
	case "help", "-h", "--help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "ratchet: unknown command %q\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "ratchet:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `ratchet — measure Go code quality, and hold the line against regression

usage:
  ratchet scan     [flags] [path]   measure and report
  ratchet baseline [flags] [path]   record current state as tolerated
  ratchet check    [flags] [path]   exit non-zero only on regression
  ratchet history  [flags] [path]   metric series over git history
  ratchet import   [flags] <file>   ingest SARIF from any linter or DCM JSON for Dart, then baseline/check
  ratchet exceptions [path]         everything currently tolerated, and why
  ratchet learn <repo>...           derive candidate rules from codebases you trust
  ratchet rules                     list rules
  ratchet version

run "ratchet <command> -h" for flags
`)
}

// commonFlags are shared by the scanning commands.
type commonFlags struct {
	format        string
	rules         string
	skipDirs      string
	includeTests  bool
	includeVendor bool
	detail        bool
	top           int
}

func bindCommon(fs *flag.FlagSet, c *commonFlags) {
	fs.StringVar(&c.format, "format", "text", "output: text|json|csv|sarif")
	fs.StringVar(&c.rules, "rules", "", "comma-separated rule IDs to enable (default: all)")
	fs.StringVar(&c.skipDirs, "skip-dirs", "", "comma-separated extra directory names to skip")
	fs.BoolVar(&c.includeTests, "include-tests", false, "analyse _test.go files")
	fs.BoolVar(&c.includeVendor, "include-vendor", false, "analyse vendor/")
	fs.BoolVar(&c.detail, "detail", true, "include per-function records")
	fs.IntVar(&c.top, "top", 10, "hotspots to list in text output")
}

// loadCfg attaches project-level verdicts. A missing config is not an error —
// most repos will not have one, and the in-code annotations stand alone.
func loadCfg(root string, opt analyze.Options) (analyze.Options, error) {
	cfg, err := analyze.LoadConfig(root)
	if err != nil {
		return opt, err
	}
	opt.Config = cfg
	return opt, nil
}

func (c commonFlags) options() analyze.Options {
	opt := analyze.Options{
		IncludeTests:  c.includeTests,
		IncludeVendor: c.includeVendor,
		Detail:        c.detail,
	}
	if c.rules != "" {
		opt.Enabled = map[string]bool{}
		for _, r := range strings.Split(c.rules, ",") {
			opt.Enabled[strings.TrimSpace(r)] = true
		}
	}
	for _, d := range strings.Split(c.skipDirs, ",") {
		if d = strings.TrimSpace(d); d != "" {
			opt.SkipDirs = append(opt.SkipDirs, d)
		}
	}
	return opt
}

// parseArgs parses flags that appear before OR after the path argument.
//
// Go's flag package stops parsing at the first non-flag argument, so
// `ratchet scan . --format json` silently ignores --format and emits text.
// Everyone types the path first, so accepting only the other order is a trap —
// and one this tool's own README fell into. We loop: parse, pull off the
// positional the parser stopped on, parse the remainder, repeat.
func parseArgs(fs *flag.FlagSet, args []string) string {
	var positional []string
	for {
		if err := fs.Parse(args); err != nil {
			return "."
		}
		rest := fs.Args()
		if len(rest) == 0 {
			break
		}
		positional = append(positional, rest[0])
		args = rest[1:]
	}
	if len(positional) == 0 {
		return "."
	}
	// Accept the ./... idiom people type by reflex, but we walk the tree
	// ourselves rather than resolving package patterns.
	return strings.TrimSuffix(strings.TrimSuffix(positional[0], "..."), "/")
}

func cmdScan(args []string) error {
	fs := flag.NewFlagSet("scan", flag.ExitOnError)
	var c commonFlags
	bindCommon(fs, &c)
	failOn := fs.String("fail-on", "", "exit non-zero if any finding at or above this severity: error|warn|info")
	emit := fs.String("emit", "", "emit assay JSONL instead of a report: measures|findings")
	repoName := fs.String("repo", "", "repo name to stamp on emitted records")
	noVerdicts := fs.Bool("no-verdicts", false,
		"do not emit verdict records alongside findings (they are emitted by default)")
	org := fs.String("org", "",
		"organisation to attribute evidence to (else .quality.yaml org:, else inferred from git email)")
	root := parseArgs(fs, args)
	opt, err := loadCfg(root, c.options())
	if err != nil {
		return err
	}
	rep, err := analyze.Scan(root, opt)
	if err != nil {
		return err
	}
	if *emit != "" {
		// Feed strata directly: ratchet scan . --emit measures | strata append
		name := *repoName
		if name == "" {
			name = filepath.Base(mustAbs(root))
		}
		orgName := resolveOrg(*org, opt.Config.Org, root)
		if err := emitAssay(rep, *emit, name, gitCommit(root), orgName, !*noVerdicts, time.Now().UTC(), nativeTool); err != nil {
			return err
		}
	} else if err := emitReport(rep, c); err != nil {
		return err
	}
	if *failOn != "" && exceeds(rep, model.Severity(*failOn)) {
		os.Exit(1)
	}
	return nil
}

func mustAbs(p string) string {
	a, err := filepath.Abs(p)
	if err != nil {
		return p
	}
	return a
}

// resolveOrg picks the organisation evidence is attributed to, and says so when inferred. "-" means none.
func resolveOrg(flagVal, cfgOrg, root string) string {
	if flagVal == "-" {
		return ""
	}
	name := verdict.ResolveOrg(flagVal, cfgOrg, gitEmail(root))
	if name != "" && flagVal == "" && cfgOrg == "" {
		fmt.Fprintf(os.Stderr,
			"note: attributing evidence to org %q, inferred from git email. "+
				"Use --org or set org: in .quality.yaml to be explicit, or --org=- for none.\n", name)
	}
	return name
}

// emitAssay writes assay JSONL records so ratchet composes with strata.
//
// Split into per-kind helpers after assay flagged this at cognitive 28 — second
// worst in its own codebase, and freshly written. Dogfooding works.
func emitAssay(rep *model.Report, kind, repo, commit, org string, withVerdicts bool, now time.Time, toolOf func(model.Finding) string) error {
	enc := schema.NewEncoder(os.Stdout)
	defer enc.Flush()

	switch kind {
	case "measures":
		return emitMeasures(enc, rep, repo, commit, now)
	case "findings":
		if err := emitFindings(enc, rep, repo, commit, now, toolOf); err != nil {
			return err
		}
		// Verdicts are emitted BY DEFAULT. A judgement that only lives in a
		// comment is invisible to every other tool and never accumulates into
		// the precision dataset — which is the whole point of recording it. If
		// harvesting is the right thing, it should not need asking for.
		if withVerdicts {
			return emitVerdicts(enc, rep, repo, org, now)
		}
		return nil
	}
	return fmt.Errorf("unknown --emit %q (want measures|findings)", kind)
}

// projectMetrics flattens the summary into the metric names strata will key on.
func projectMetrics(s model.Summary) map[string]float64 {
	return map[string]float64{
		"files": float64(s.Files), "funcs": float64(s.Funcs), "statements": float64(s.Statements),
		"cyclomatic.p50": float64(s.Cyclomatic.P50), "cyclomatic.p90": float64(s.Cyclomatic.P90),
		"cyclomatic.max": float64(s.Cyclomatic.Max), "cyclomatic.mean": s.Cyclomatic.Mean,
		"cognitive.p50": float64(s.Cognitive.P50), "cognitive.p90": float64(s.Cognitive.P90),
		"cognitive.max": float64(s.Cognitive.Max), "cognitive.mean": s.Cognitive.Mean,
		"nesting.max":    float64(s.MaxNesting.Max),
		"findings.total": float64(s.FindingsTotal),
	}
}

// funcMetricValues is the per-function set. Named so adding a metric is a
// one-line change in one place.
func funcMetricValues(f model.FuncMetrics) map[string]float64 {
	return map[string]float64{
		"cyclomatic": float64(f.Cyclomatic),
		"cognitive":  float64(f.Cognitive),
		"nesting":    float64(f.MaxNesting),
		"statements": float64(f.Statements),
	}
}

func emitMeasures(enc *schema.Encoder, rep *model.Report, repo, commit string, now time.Time) error {
	put := func(scope schema.Scope, path, metric string, v float64) error {
		return enc.Write(&schema.Measure{
			Repo: repo, Commit: commit, TS: now,
			Scope: scope, Path: path, Metric: metric, Value: v,
		})
	}

	proj := projectMetrics(rep.Summary)
	for _, k := range sortedKeys(proj) {
		if err := put(schema.ScopeProject, "", k, proj[k]); err != nil {
			return err
		}
	}
	for _, rule := range sortedCountKeys(rep.Summary.FindingsByRule) {
		if err := put(schema.ScopeProject, "", "findings.rule."+rule,
			float64(rep.Summary.FindingsByRule[rule])); err != nil {
			return err
		}
	}
	for _, f := range rep.Funcs {
		path := f.File + ":" + qualified(f)
		m := funcMetricValues(f)
		for _, k := range sortedKeys(m) {
			if err := put(schema.ScopeFunction, path, k, m[k]); err != nil {
				return err
			}
		}
	}
	return nil
}

// nativeTool names the producer of a scan's own findings.
func nativeTool(model.Finding) string { return "ratchet" }

func emitFindings(enc *schema.Encoder, rep *model.Report, repo, commit string, now time.Time, toolOf func(model.Finding) string) error {
	for _, f := range rep.Findings {
		if err := enc.Write(&schema.Finding{
			Repo: repo, Commit: commit, TS: now, Tool: toolOf(f),
			Rule: f.Rule, Severity: schema.Severity(f.Severity),
			File: f.File, Line: f.Line, Col: f.Col, Symbol: f.Func,
			Message: f.Message, Suggest: f.Suggest, Fingerprint: f.Fingerprint,
			Verdict: schema.Judgement(f.Verdict), VerdictWhy: f.VerdictWhy,
			VerdictUntil: f.VerdictUntil, VerdictFrom: f.VerdictFrom,
		}); err != nil {
			return err
		}
	}
	return nil
}

// emitVerdicts turns resolved judgements into records the store can accumulate.
//
// By is deliberately left empty. The annotation's author is recoverable with git
// blame, but running blame per finding would dominate a scan, and a wrong
// attribution is worse than none.
func emitVerdicts(enc *schema.Encoder, rep *model.Report, repo, org string, now time.Time) error {
	for _, f := range rep.Findings {
		if f.Verdict == "" {
			continue // unjudged is not a verdict; it is the absence of one
		}
		if err := enc.Write(&schema.Verdict{
			Fingerprint: f.Fingerprint,
			Rule:        f.Rule,
			Repo:        repo,
			Org:         org,
			Verdict:     schema.Judgement(f.Verdict),
			Reason:      f.VerdictWhy,
			TS:          now,
		}); err != nil {
			return err
		}
	}
	return nil
}

func qualified(f model.FuncMetrics) string {
	if f.Recv != "" {
		return f.Recv + "." + f.Name
	}
	return f.Name
}

func sortedCountKeys(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedKeys(m map[string]float64) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func exceeds(rep *model.Report, min model.Severity) bool {
	rank := map[model.Severity]int{model.Info: 1, model.Warn: 2, model.Error: 3}
	for _, f := range rep.Findings {
		if rank[f.Severity] >= rank[min] {
			return true
		}
	}
	return false
}

func emitReport(rep *model.Report, c commonFlags) error {
	out := bufio.NewWriter(os.Stdout)
	defer out.Flush()
	switch c.format {
	case "json":
		return report.JSON(out, rep)
	case "csv":
		return report.CSV(out, rep)
	case "sarif":
		docs := map[string]string{}
		for id, r := range analyze.Rules {
			docs[id] = r.Doc
		}
		return report.SARIF(out, rep, docs)
	case "text":
		if err := report.Text(out, rep, c.top); err != nil {
			return err
		}
		report.Findings(out, rep.Findings)
		return nil
	}
	return fmt.Errorf("unknown format %q", c.format)
}

func cmdBaseline(args []string) error {
	fs := flag.NewFlagSet("baseline", flag.ExitOnError)
	var c commonFlags
	bindCommon(fs, &c)
	file := fs.String("file", defaultBaselineFile, "baseline path")
	force := fs.Bool("force", false, "overwrite an existing baseline")
	tighten := fs.Bool("tighten", false, "drop entries that no longer reproduce, keep the rest")
	root := parseArgs(fs, args)
	opt, err := loadCfg(root, c.options())
	if err != nil {
		return err
	}
	rep, err := analyze.Scan(root, opt)
	if err != nil {
		return err
	}
	path := *file
	if !filepath.IsAbs(path) {
		path = filepath.Join(root, path)
	}

	if _, err := os.Stat(path); err == nil && !*force && !*tighten {
		// The whole point of a ratchet is that you cannot quietly reset it.
		// Requiring an explicit flag means a rewrite shows up in review.
		return fmt.Errorf("baseline already exists at %s\n"+
			"  regenerating it would silently forgive every current violation.\n"+
			"  use --tighten to drop only what is genuinely fixed, or --force to overwrite", path)
	}

	commit := gitCommit(root)
	if *tighten {
		b, err := baseline.Load(path)
		if err != nil {
			return err
		}
		n := b.Tighten(rep)
		b.Commit = commit
		if err := b.Save(path); err != nil {
			return err
		}
		fmt.Printf("tightened %s: removed %d fixed entries, %d remain\n", path, n, len(b.Tolerated))
		return nil
	}

	b := baseline.From(rep, commit)
	if err := b.Save(path); err != nil {
		return err
	}
	fmt.Printf("wrote %s\n  %d tolerated findings\n  caps: cyclomatic<=%d cognitive<=%d nesting<=%d\n",
		path, len(b.Tolerated), b.Caps.MaxCyclomatic, b.Caps.MaxCognitive, b.Caps.MaxNesting)
	fmt.Println("\ncommit this file — it is the record of what we agreed to tolerate")
	return nil
}

func cmdCheck(args []string) error {
	fs := flag.NewFlagSet("check", flag.ExitOnError)
	var c commonFlags
	bindCommon(fs, &c)
	file := fs.String("file", defaultBaselineFile, "baseline path")
	strictCaps := fs.Bool("strict-caps", false, "also fail if max complexity exceeds the baseline")
	asJSON := fs.Bool("json", false, "emit the result as JSON")
	root := parseArgs(fs, args)
	path := *file
	if !filepath.IsAbs(path) {
		path = filepath.Join(root, path)
	}
	b, err := baseline.Load(path)
	if err != nil {
		return fmt.Errorf("%w\n  run `ratchet baseline` first", err)
	}
	opt, err := loadCfg(root, c.options())
	if err != nil {
		return err
	}
	rep, err := analyze.Scan(root, opt)
	if err != nil {
		return err
	}
	res := b.Check(rep, *strictCaps)

	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(res); err != nil {
			return err
		}
	} else {
		fmt.Printf("%d tolerated, %d new, %d fixed\n", res.Existing, len(res.New), len(res.Fixed))
		if len(res.Fixed) > 0 {
			fmt.Printf("\n%d findings no longer present — run `ratchet baseline --tighten` to lock that in\n", len(res.Fixed))
		}
		if len(res.New) > 0 {
			fmt.Printf("\nNEW findings (these fail the build):\n")
			report.Findings(os.Stdout, res.New)
		}
		for _, cb := range res.CapBreak {
			fmt.Printf("\ncap breach: %s\n", cb)
		}
	}
	if res.Regressed() {
		os.Exit(1)
	}
	return nil
}

// cmdHistory replays the metric summary over git history.
//
// This one genuinely needs a checkout per sample: per-function complexity
// requires a parsed tree, which a diff cannot give us. Checkout dominates the
// cost (~0.7s vs ~15ms for diff-only work), so we sample rather than replay
// every commit, and we use `git archive` into a temp dir instead of mutating the
// working tree — the latter would be unusable on a machine someone is working on.
func cmdHistory(args []string) error {
	fs := flag.NewFlagSet("history", flag.ExitOnError)
	var c commonFlags
	bindCommon(fs, &c)
	since := fs.String("since", "", "git --since value (empty = from the first commit)")
	interval := fs.String("interval", "1 week", "sampling interval: all|1 day|1 week|1 month")
	maxN := fs.Int("max", 0, "maximum samples (0 = unlimited)")
	firstParent := fs.Bool("first-parent", true,
		"follow only the merge timeline. Feature-branch intermediate commits are work in progress, not states the codebase was ever really in")
	jobs := fs.Int("jobs", 4, "parallel workers. Checkout dominates the cost, so this scales well")
	root := parseArgs(fs, args)
	c.detail = false // only the summary is kept for a series

	commits, err := sampleCommits(root, *since, *interval, *maxN, *firstParent)
	if err != nil {
		return err
	}
	if len(commits) == 0 {
		return fmt.Errorf("no commits found in range")
	}
	fmt.Fprintf(os.Stderr, "sampling %d commits with %d workers\n", len(commits), *jobs)

	// Each worker gets its own temp checkout. Results are collected then
	// emitted in commit order, because a series that arrives out of order is
	// useless for a trend and callers should not have to sort it.
	type slot struct {
		rep *model.Report
		err error
	}
	out := make([]slot, len(commits))
	opts := c.options()

	sem := make(chan struct{}, max(1, *jobs))
	var wg sync.WaitGroup
	var done int64

	for i, sha := range commits {
		wg.Add(1)
		go func(i int, sha string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			dir, err := os.MkdirTemp("", "ratchet-*")
			if err != nil {
				out[i] = slot{err: err}
				return
			}
			defer os.RemoveAll(dir)

			if err := gitArchive(root, sha, dir); err != nil {
				out[i] = slot{err: err}
				return
			}
			rep, err := analyze.Scan(dir, opts)
			if err != nil {
				out[i] = slot{err: err}
				return
			}
			rep.Commit = sha
			rep.Root = ""
			out[i] = slot{rep: rep}

			if n := atomic.AddInt64(&done, 1); n%25 == 0 || int(n) == len(commits) {
				fmt.Fprintf(os.Stderr, "  %d/%d\n", n, len(commits))
			}
		}(i, sha)
	}
	wg.Wait()

	enc := json.NewEncoder(os.Stdout)
	skipped := 0
	for i, s := range out {
		if s.rep == nil {
			skipped++
			fmt.Fprintf(os.Stderr, "  skipped %s: %v\n", commits[i][:8], s.err)
			continue
		}
		if err := enc.Encode(s.rep); err != nil {
			return err
		}
	}
	// Silent truncation reads as "covered everything" when it did not.
	fmt.Fprintf(os.Stderr, "emitted %d records, skipped %d\n", len(commits)-skipped, skipped)
	return nil
}

// sampleCommits picks commits to measure. interval "all" takes every commit;
// otherwise one per bucket. git's own --since/--until bucketing is awkward here,
// so we take the log and thin it by date ourselves.
func sampleCommits(root, since, interval string, maxN int, firstParent bool) ([]string, error) {
	logArgs := []string{"log"}
	if firstParent {
		logArgs = append(logArgs, "--first-parent")
	}
	if since != "" {
		logArgs = append(logArgs, "--since="+since)
	}
	logArgs = append(logArgs, "--date=format:%Y-%m-%d", "--pretty=format:%H %ad")
	out, err := gitOut(root, logArgs...)
	if err != nil {
		return nil, err
	}
	bucket := func(date string, idx int) string {
		switch interval {
		case "all":
			return fmt.Sprint(idx) // unique per commit: nothing is thinned
		case "1 day":
			return date
		case "1 month":
			return date[:7]
		default: // 1 week — ISO-ish bucketing by day-of-month block
			return date[:7] + "-w" + fmt.Sprint((atoi(date[8:10])-1)/7)
		}
	}
	seen := map[string]bool{}
	var picked []string
	for idx, line := range strings.Split(strings.TrimSpace(out), "\n") {
		parts := strings.Fields(line)
		if len(parts) < 2 {
			continue
		}
		b := bucket(parts[1], idx)
		if seen[b] {
			continue
		}
		seen[b] = true
		picked = append(picked, parts[0])
		if maxN > 0 && len(picked) >= maxN {
			break
		}
	}
	// Oldest first, so the emitted series reads forward in time.
	for i, j := 0, len(picked)-1; i < j; i, j = i+1, j-1 {
		picked[i], picked[j] = picked[j], picked[i]
	}
	return picked, nil
}

func atoi(s string) int {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return n
		}
		n = n*10 + int(c-'0')
	}
	return n
}

func gitArchive(root, sha, dest string) error {
	cmd := exec.Command("git", "-C", root, "archive", sha)
	tar := exec.Command("tar", "-x", "-C", dest)
	pipe, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	tar.Stdin = pipe
	if err := cmd.Start(); err != nil {
		return err
	}
	if err := tar.Run(); err != nil {
		return err
	}
	return cmd.Wait()
}

func gitOut(root string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
	b, err := cmd.Output()
	return string(b), err
}

// gitEmail reads the committer identity, used only as the last-resort source
// for org attribution.
func gitEmail(root string) string {
	out, err := gitOut(root, "config", "user.email")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

func gitCommit(root string) string {
	out, err := gitOut(root, "rev-parse", "HEAD")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

func cmdRules() {
	ids := make([]string, 0, len(analyze.Rules))
	for id := range analyze.Rules {
		ids = append(ids, id)
	}
	for i := range ids {
		for j := i + 1; j < len(ids); j++ {
			if ids[j] < ids[i] {
				ids[i], ids[j] = ids[j], ids[i]
			}
		}
	}
	for _, id := range ids {
		r := analyze.Rules[id]
		fmt.Printf("%-28s [%s]\n  %s\n\n", id, r.Severity, r.Doc)
	}
}

// cmdExceptions lists everything currently tolerated.
//
// The ratchet stops debt growing; nothing makes it shrink. Without a way to see
// the whole set, a baseline quietly becomes permanent and nobody revisits the
// decision. This is the "what have we agreed to live with" report — including
// the entries nobody put an expiry on.
func cmdExceptions(args []string) error {
	fs := flag.NewFlagSet("exceptions", flag.ExitOnError)
	var c commonFlags
	bindCommon(fs, &c)
	expiredOnly := fs.Bool("expired", false, "only entries past their review-by date")
	asJSON := fs.Bool("json", false, "emit JSON")
	root := parseArgs(fs, args)

	opt, err := loadCfg(root, c.options())
	if err != nil {
		return err
	}
	rep, err := analyze.Scan(root, opt)
	if err != nil {
		return err
	}

	type row struct {
		F       model.Finding
		Expired bool
	}
	var rows []row
	counts := map[string]int{}
	now := time.Now()
	for _, f := range rep.Findings {
		if f.Verdict == "" {
			counts["unjudged"]++
			continue
		}
		exp := false
		if f.VerdictUntil != "" {
			if t, perr := time.Parse("2006-01-02", f.VerdictUntil); perr == nil && now.After(t) {
				exp = true
			}
		}
		if *expiredOnly && !exp {
			continue
		}
		counts[f.Verdict]++
		if exp {
			counts["expired"]++
		}
		rows = append(rows, row{f, exp})
	}

	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(rows)
	}

	if len(rows) == 0 {
		if *expiredOnly {
			fmt.Println("no expired exceptions")
		} else {
			fmt.Printf("no exceptions recorded (%d findings are unjudged)\n", counts["unjudged"])
		}
		return nil
	}

	fmt.Printf("%-16s %-11s %-30s %s\n", "verdict", "review-by", "location", "reason")
	fmt.Println(strings.Repeat("─", 100))
	for _, r := range rows {
		until := r.F.VerdictUntil
		if until == "" {
			until = "permanent"
		}
		if r.Expired {
			until += " ⚠"
		}
		loc := fmt.Sprintf("%s:%d", r.F.File, r.F.Line)
		fmt.Printf("%-16s %-11s %-30s %s\n", r.F.Verdict, until, truncTail(loc, 30), truncHead(r.F.VerdictWhy, 44))
	}

	fmt.Printf("\n%d accepted, %d false-positive, %d wont-fix",
		counts[string(schema.Accepted)], counts[string(schema.FalsePositive)], counts[string(schema.WontFix)])
	if counts["unjudged"] > 0 {
		fmt.Printf(", %d unjudged", counts["unjudged"])
	}
	fmt.Println()
	if n := counts["expired"]; n > 0 {
		fmt.Printf("\n⚠  %d past their review-by date. Still passing — but nobody has looked.\n", n)
		fmt.Printf("   Renew the date, or drop it to make the exception permanent.\n")
	}
	return nil
}

// truncHead keeps the start — for a reason, the ticket reference and the first
// words carry the meaning. truncTail keeps the end, because for a path the
// filename matters more than the directory.
func truncHead(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

func truncTail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n+1:]
}

// cmdLearn probes codebases you already trust and reports which conventions
// they hold.
//
// The rejections matter as much as the acceptances: a pattern both codebases use
// constantly is evidence NOT to ship that rule, whatever the style guides say.
// parseArgsMulti is parseArgs for commands taking several positionals. Go's
// flag package stops at the first non-flag argument, so `learn a b --emit`
// would otherwise treat --emit as a third repository.
func parseArgsMulti(fs *flag.FlagSet, args []string) []string {
	var positional []string
	for {
		if err := fs.Parse(args); err != nil {
			return positional
		}
		rest := fs.Args()
		if len(rest) == 0 {
			return positional
		}
		positional = append(positional, rest[0])
		args = rest[1:]
	}
}

func cmdLearn(args []string) error {
	fs := flag.NewFlagSet("learn", flag.ExitOnError)
	emit := fs.Bool("emit", false, "print draft semgrep rules for the held candidates")
	heldBelow := fs.Float64("held-below", 0.15, "per-kLOC rate under which a pattern counts as avoided")
	repos := parseArgsMulti(fs, args)
	if len(repos) == 0 {
		return fmt.Errorf("usage: ratchet learn <repo> [repo...]\n" +
			"  two or more codebases you have independent reason to trust.\n" +
			"  one repo can only tell you what that repo does, not what is worth enforcing")
	}
	if len(repos) == 1 {
		fmt.Fprintln(os.Stderr,
			"warning: one repository cannot distinguish a shared convention from local habit.\n"+
				"         treat everything below as weaker than it looks.")
	}

	t := learn.DefaultThresholds()
	t.HeldBelow = *heldBelow
	results, err := learn.Probe(repos, learn.Catalogue(), t)
	if err != nil {
		return err
	}

	names := make([]string, 0, len(results[0].Rates))
	for k := range results[0].Rates {
		names = append(names, k)
	}
	sort.Strings(names)

	fmt.Printf("%-28s", "candidate")
	for _, n := range names {
		fmt.Printf("%12s", truncTail(n, 11))
	}
	fmt.Printf("  verdict\n")
	fmt.Println(strings.Repeat("─", 28+12*len(names)+22))

	var held, notHeld, divergent int
	for _, r := range results {
		fmt.Printf("%-28s", truncHead(r.Candidate.ID, 27))
		for _, n := range names {
			fmt.Printf("%12.2f", r.Rates[n])
		}
		switch r.Verdict {
		case learn.Held:
			held++
			fmt.Printf("  held — ship it\n")
		case learn.Divergent:
			divergent++
			fmt.Printf("  divergent — org rule\n")
		default:
			notHeld++
			fmt.Printf("  NOT held — reject\n")
		}
	}

	fmt.Printf("\n%d held, %d divergent, %d not held\n\n", held, divergent, notHeld)
	fmt.Printf("held      these codebases avoid it. Candidate for a rule.\n")
	fmt.Printf("divergent one avoids it, another does not — taste, age or local context.\n")
	fmt.Printf("          An org rule at most, never core.\n")
	fmt.Printf("NOT held  they do it routinely. Shipping this would generate noise at scale,\n")
	fmt.Printf("          however many style guides recommend it.\n")

	fmt.Printf("\nThis finds CONVENTIONS, not defects. A codebase can consistently do\n")
	fmt.Printf("something bad. Run each held candidate against real code and read every\n")
	fmt.Printf("finding before enabling it — three of this tool's own five original rules\n")
	fmt.Printf("passed statistical tests and were still noise on contact with production.\n")

	if *emit {
		fmt.Printf("\n%s\n", strings.Repeat("─", 60))
		for _, r := range results {
			if r.Verdict != learn.Held {
				continue
			}
			fmt.Printf("\n# rules/local/%s.yaml\n%s", r.Candidate.ID, learn.DraftRule(r))
		}
	}
	return nil
}
