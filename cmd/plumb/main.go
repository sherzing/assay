// Command plumb checks a codebase against what ARCHITECTURE.md declares: which
// groups of packages may reach which (`forbid`, over the import graph) and which group owns
// which vocabulary (`owns`, over declared names). The first is reachability;
// the second is responsibility. Neither implies the other.
//
// A plumb line is the oldest conformance tool there is: you declare vertical,
// and the string tells you the truth. It does not have an opinion about where
// the wall should go.
package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/sherzing/assay/internal/arch"
	"github.com/sherzing/assay/internal/baseline"
	"github.com/sherzing/assay/internal/model"
	"github.com/sherzing/assay/internal/report"
	"github.com/sherzing/assay/pkg/schema"
)

const usage = `plumb — verify a codebase against the dependency and ownership rules it declares

  plumb check [dir]      fail on violations not in the baseline
  plumb scan  [dir]      report every violation, exit 0
  plumb baseline [dir]   record today's violations as tolerated
  plumb diff  [dir]      classify a declaration change as tightening or loosening
  plumb learn [dir]      draft owns lines from what the code declares

Flags
  --doc <path>       declaration (default ARCHITECTURE.md)
  --file <path>      baseline file (default .plumb-baseline.json)
  --emit findings    write JSONL to stdout for strata/docket
  --include-tests    follow imports from _test.go files
  --base <ref>       git ref to diff the declaration against (default origin/main)
  --force            overwrite an existing baseline
  --tighten          drop baseline entries that no longer reproduce
  --depth N          learn: directory depth that defines a context when no
                     declaration exists (default 2)
  --min N            learn: fewest declarations that must carry a term (default 3)
  --share F          learn: fraction of a term's uses in one context (default 0.75)

The declaration is a fenced ` + "```arch" + ` block inside the document:

    group orders    internal/orders
    group payments  internal/payments
    group shared    internal/shared
    forbid orders   -> payments
    forbid payments -> orders
    forbid shared   -> orders
    forbid shared   -> payments
    owns orders     order line-item checkout
    owns payments   payment refund charge

A group is a named set of directories; layer is accepted as a synonym. plumb
has no opinion on how many groups there are or which way dependencies point:
two groups and one rule is a complete declaration.

forbid governs which packages may REACH which, transitively: a direct-import
rule misses orders -> helper -> payments, which is the same dependency one hop
away and is what an ordinary refactor produces.

owns governs what a group may DECLARE. A type or function in orders whose name
carries payments' vocabulary is flagged; orders calling payments' API is not. A
group with no owns line is a consumer and is never checked. Only Go is
supported for forbid; owns reads Go, C#, Dart, TypeScript, Java, Kotlin and Python.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	cmd := os.Args[1]
	args := os.Args[2:]

	var err error
	switch cmd {
	case "check":
		err = run(args, true)
	case "scan":
		err = run(args, false)
	case "baseline":
		err = cmdBaseline(args)
	case "diff":
		err = cmdDiff(args)
	case "learn":
		err = cmdLearn(args)
	case "-h", "--help", "help":
		fmt.Print(usage)
		return
	default:
		fmt.Fprintf(os.Stderr, "plumb: unknown command %q\n\n%s", cmd, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "plumb:", err)
		os.Exit(1)
	}
}

type opts struct {
	dir, doc, file, emit, base   string
	includeTests, force, tighten bool
	depth, min, top              int
	share                        float64
}

// parseArgs accepts flags on either side of the positional argument. Go's flag
// package stops at the first non-flag, which silently drops every flag placed
// after the path — a failure that looks like the flag having no effect.
func parseArgs(args []string) (opts, error) {
	o := opts{doc: "ARCHITECTURE.md", file: ".plumb-baseline.json", base: "origin/main"}
	fs := flag.NewFlagSet("plumb", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.StringVar(&o.doc, "doc", o.doc, "architecture declaration")
	fs.StringVar(&o.file, "file", o.file, "baseline path")
	fs.StringVar(&o.emit, "emit", "", "emit JSONL: findings")
	fs.StringVar(&o.base, "base", o.base, "git ref to diff against")
	fs.BoolVar(&o.includeTests, "include-tests", false, "follow test imports")
	fs.BoolVar(&o.force, "force", false, "overwrite an existing baseline")
	fs.BoolVar(&o.tighten, "tighten", false, "drop entries that no longer reproduce")
	fs.IntVar(&o.depth, "depth", 2, "learn: directory depth defining a context")
	fs.IntVar(&o.min, "min", 3, "learn: fewest declarations carrying a term")
	fs.IntVar(&o.top, "top", 8, "learn: terms per context")
	fs.Float64Var(&o.share, "share", 0.75, "learn: fraction of a term's uses in one context")

	var positional []string
	rest := args
	for len(rest) > 0 {
		if err := fs.Parse(rest); err != nil {
			return o, err
		}
		rest = fs.Args()
		if len(rest) > 0 {
			positional = append(positional, rest[0])
			rest = rest[1:]
		}
	}
	if len(positional) > 1 {
		return o, fmt.Errorf("expected one directory, got %d: %s", len(positional), strings.Join(positional, " "))
	}
	o.dir = "."
	if len(positional) == 1 {
		o.dir = positional[0]
	}
	return o, nil
}

// loaded is everything a run needs. Graph is nil when the declaration has no
// forbid rules: an ownership-only declaration does not need a Go module.
type loaded struct {
	decl   *arch.Decl
	graph  *arch.Graph
	vs     []arch.Violation
	drifts []arch.Drift
	rep    *model.Report
}

func readDecl(o opts) (*arch.Decl, error) {
	docPath := o.doc
	if !filepath.IsAbs(docPath) {
		docPath = filepath.Join(o.dir, o.doc)
	}
	raw, err := os.ReadFile(docPath)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w\n\nWrite one first — see the preparation checklist in the README. "+
			"A tool that passes because there is nothing to check is worse than no tool", docPath, err)
	}
	d, err := arch.ParseDoc(string(raw))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", o.doc, err)
	}
	d.SHA = declSHA(o.dir, o.doc)
	return d, nil
}

// load reads the declaration and builds the report.
func load(o opts) (loaded, error) {
	d, err := readDecl(o)
	if err != nil {
		return loaded{}, err
	}
	l := loaded{decl: d}
	if len(d.Forbids) > 0 {
		l.graph, err = arch.LoadGoGraph(o.dir, o.includeTests)
		if err != nil {
			return loaded{}, err
		}
		l.vs = arch.Check(d, l.graph)
	}
	if len(d.Owns) > 0 {
		decls, err := arch.ScanDecls(o.dir, d, o.includeTests)
		if err != nil {
			return loaded{}, err
		}
		l.drifts = arch.CheckOwns(d, decls)
	}
	l.rep = arch.Report(d, l.graph, l.vs, l.drifts)
	return l, nil
}

func run(args []string, gate bool) error {
	o, err := parseArgs(args)
	if err != nil {
		return err
	}
	l, err := load(o)
	if err != nil {
		return err
	}
	d, rep := l.decl, l.rep

	if o.emit == "findings" {
		enc := schema.NewEncoder(os.Stdout)
		for i := range rep.Findings {
			f := rep.Findings[i]
			if err := enc.Write(&schema.Finding{
				Tool: "plumb", Rule: f.Rule, Severity: schema.SevError,
				File: f.File, Line: f.Line, Message: f.Message, Suggest: f.Suggest,
				Symbol: d.SHA, Fingerprint: f.Fingerprint,
			}); err != nil {
				return err
			}
		}
		return enc.Flush()
	}

	if l.graph != nil {
		for _, dead := range arch.DeadLayers(d, l.graph) {
			fmt.Fprintf(os.Stderr, "warning: group %q matches no package — a rule guarding a directory that does not exist protects nobody\n", dead)
		}
	}

	if !gate {
		pkgs := 0
		if l.graph != nil {
			pkgs = len(l.graph.Edges)
		}
		printViolations(l.vs, pkgs, len(d.Forbids))
		printDrift(l.drifts, len(d.Owns))
		return nil
	}

	b, err := baseline.Load(filepath.Join(o.dir, o.file))
	if err != nil {
		return fmt.Errorf("no baseline at %s: run `plumb baseline` first.\n"+
			"Adopting on an existing codebase means recording today's violations and failing only on new ones —\n"+
			"starting from zero is how a rule gets switched off in the first afternoon", o.file)
	}
	res := b.Check(rep, false)
	fmt.Printf("%d tolerated, %d new, %d fixed\n", res.Existing, len(res.New), len(res.Fixed))
	if len(res.Fixed) > 0 {
		fmt.Printf("\n%d no longer present — `plumb baseline --tighten` locks that in\n", len(res.Fixed))
	}
	if len(res.New) > 0 {
		fmt.Printf("\nNEW violations (these fail the build):\n\n")
		report.Findings(os.Stdout, res.New)
		return fmt.Errorf("%d new architecture violations", len(res.New))
	}
	return nil
}

func printViolations(vs []arch.Violation, pkgs, rules int) {
	fmt.Printf("%d packages, %d rules, %d violations\n", pkgs, rules, len(vs))
	if len(vs) == 0 {
		return
	}
	cur := ""
	for _, v := range vs {
		if v.Forbid.String() != cur {
			cur = v.Forbid.String()
			fmt.Printf("\nforbid %s\n", cur)
		}
		fmt.Printf("  %s\n", strings.Join(v.Chain, " → "))
		fmt.Printf("      %s\n", v.Suggest())
	}
}

func printDrift(ds []arch.Drift, owners int) {
	if owners == 0 {
		return
	}
	fmt.Printf("\n%d owning groups, %d responsibility drifts\n", owners, len(ds))
	cur := ""
	for _, d := range ds {
		if k := d.Layer + " → " + d.Owner; k != cur {
			cur = k
			fmt.Printf("\n%s declares %s's vocabulary\n", d.Layer, d.Owner)
		}
		fmt.Printf("  %-32s [%s]  %s:%d\n", d.Name, d.Term, d.File, d.Line)
	}
}

func cmdLearn(args []string) error {
	o, err := parseArgs(args)
	if err != nil {
		return err
	}
	// The declaration is optional here: learn is how you get one.
	d, err := readDecl(o)
	if err != nil {
		d = nil
	}
	lines, err := arch.Learn(o.dir, d, arch.LearnOptions{
		Depth: o.depth, Min: o.min, Share: o.share, Top: o.top, IncludeTests: o.includeTests,
	})
	if err != nil {
		return err
	}
	// The draft is a list of this codebase's own domain vocabulary, with usage
	// counts. That is internal information by nature — more revealing than any
	// finding, because it names the business concepts rather than a defect.
	// Warned on stderr so it survives `plumb learn . > draft.txt`.
	fmt.Fprint(os.Stderr, "note: this draft lists your internal domain vocabulary with counts.\n"+
		"      Review it before pasting anywhere public.\n\n")
	if d == nil {
		fmt.Printf("# no %s — contexts are directories at depth %d; replace them with group names\n", o.doc, o.depth)
	}
	fmt.Print(formatDraft(lines))
	fmt.Print("\n# This is a draft, not a finding.\n" +
		"#   1. Strike utility packages ENTIRELY — they own no vocabulary, only the\n" +
		"#      generic words mechanism is written in. Marked above where detected.\n" +
		"#   2. Strike generic verbs and adjectives: modify, applied, general, unknown,\n" +
		"#      where, access, content. A term that could name anything names nothing,\n" +
		"#      and owning one flags every use of it in the codebase.\n" +
		"#   3. Keep five to ten terms per group, paste the rest into the arch block,\n" +
		"#      then `plumb scan` and read every finding.\n")
	return nil
}

func cmdBaseline(args []string) error {
	o, err := parseArgs(args)
	if err != nil {
		return err
	}
	l, err := load(o)
	if err != nil {
		return err
	}
	rep := l.rep
	path := filepath.Join(o.dir, o.file)

	if existing, err := baseline.Load(path); err == nil {
		if o.tighten {
			n := existing.Tighten(rep)
			if err := existing.Save(path); err != nil {
				return err
			}
			fmt.Printf("dropped %d violations that no longer reproduce; %d remain tolerated\n", n, len(existing.Tolerated))
			return nil
		}
		if !o.force {
			// The guard that makes the ratchet mean anything: without it,
			// "fix the failure" becomes "rewrite the baseline".
			return fmt.Errorf("baseline already exists at %s\n"+
				"  regenerating it would silently forgive every current violation.\n"+
				"  use --tighten to drop only what is genuinely fixed, or --force to overwrite", o.file)
		}
	}
	b := baseline.From(rep, gitSHA(o.dir))
	if err := b.Save(path); err != nil {
		return err
	}
	fmt.Printf("%d tolerated violations\n\ncommit this file — it is the record of what we agreed to tolerate\n", len(b.Tolerated))
	return nil
}

func cmdDiff(args []string) error {
	o, err := parseArgs(args)
	if err != nil {
		return err
	}
	cur, err := os.ReadFile(filepath.Join(o.dir, o.doc))
	if err != nil {
		return err
	}
	newD, err := arch.ParseDoc(string(cur))
	if err != nil {
		return fmt.Errorf("current %s: %w", o.doc, err)
	}

	out, err := gitShow(o.dir, o.base+":"+o.doc)
	if err != nil {
		fmt.Printf("architecture declaration: new (no %s at %s)\n", o.doc, o.base)
		return nil
	}
	oldD, err := arch.ParseDoc(out)
	if err != nil {
		fmt.Printf("architecture declaration: previous version did not parse (%v); treating as new\n", err)
		return nil
	}

	c, detail := arch.Diff(oldD, newD)
	explained := arch.NewWhyEntries(out, string(cur))
	fmt.Print(formatDiff(c, detail, explained))
	if !arch.Accepted(c, explained) {
		return fmt.Errorf("declaration weakened without a Why entry")
	}
	return nil
}

func declSHA(dir, doc string) string {
	out, err := exec.Command("git", "-C", dir, "log", "-1", "--format=%H", "--", doc).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func gitSHA(dir string) string {
	out, err := exec.Command("git", "-C", dir, "rev-parse", "HEAD").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func gitShow(dir, ref string) (string, error) {
	out, err := exec.Command("git", "-C", dir, "show", ref).Output()
	return string(out), err
}
