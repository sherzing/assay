// Command judge asks a model whether each declaration belongs in the group it
// sits in, according to the prose of ARCHITECTURE.md.
//
// It is the probabilistic companion to plumb, and deliberately a separate
// binary: plumb is deterministic and gates, judge advises. Mixing them would
// make plumb's green tick mean two different things.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/sherzing/assay/internal/arch"
	"github.com/sherzing/assay/internal/judge"
	"github.com/sherzing/assay/pkg/schema"
)

const usage = `judge — ask a model whether declarations belong where they are

  judge scan [dir]      judge declarations changed since --base (default: all)
  judge cases [dir]     print the declarations scan would judge, as JSONL
  judge verify [dir]    verify answers made elsewhere (--answers FILE) and emit findings
  judge doctor          check the provider answers
  judge version

Flags
  --doc <path>       declaration (default ARCHITECTURE.md)
  --provider <name>  anthropic | gemini | openai | claude-code   (default $JUDGE_PROVIDER)
  --model <id>       model id                       (default $JUDGE_MODEL; anthropic defaults to claude-opus-5)
  --base-url <url>   override the provider endpoint (default $JUDGE_BASE_URL)
  --effort <level>   anthropic: low | medium | high (default low)
  --base <ref>       only declarations in files changed against this git ref
  --group <name>     only declarations in this group (--layer is a synonym)
  --context N        lines of source shown per declaration (default 60)
  --jobs N           parallel requests (default 4)
  --cache <dir>      response cache (default .assay/judge; "-" disables)
  --emit findings    write JSONL to stdout for ratchet, strata and docket
  --include-tests    judge declarations in test files too
  --answers <file>   verify: JSONL of answers, one per declaration

Two ways to pay. anthropic, gemini and openai bill per token against an API
key (ANTHROPIC_API_KEY, GEMINI_API_KEY, OPENAI_API_KEY; anthropic also honours
an "ant auth login" profile). claude-code runs the Claude Code CLI headless, so
a Claude subscription covers it; it batches several declarations per call
because each call carries Claude Code's own context. The judge skill is the
third path: Claude Code reads the code itself and hands its answers to
judge verify, which applies the same checks.

Every finding cites a sentence that exists verbatim in the document; answers
that cannot are dropped and counted. Findings are warnings: nothing gates on
them until precision has been measured and someone chooses to.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "scan":
		err = cmdScan(os.Args[2:])
	case "cases":
		err = cmdCases(os.Args[2:])
	case "verify":
		err = cmdVerify(os.Args[2:])
	case "doctor":
		err = cmdDoctor(os.Args[2:])
	case "version", "-v", "--version":
		fmt.Println("judge", judge.Rule)
	case "-h", "--help", "help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "judge: unknown command %q\n\n%s", os.Args[1], usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "judge:", err)
		os.Exit(1)
	}
}

type opts struct {
	dir, doc, base, layer, emit, cache, answers string
	cfg                                         judge.Config
	context, jobs                               int
	includeTests                                bool
}

func parseArgs(args []string) (opts, error) {
	o := opts{doc: "ARCHITECTURE.md", cache: ".assay/judge", context: 60, jobs: 4}
	o.cfg = judge.Config{
		Provider: os.Getenv("JUDGE_PROVIDER"), Model: os.Getenv("JUDGE_MODEL"),
		BaseURL: os.Getenv("JUDGE_BASE_URL"), Effort: "low",
	}
	fs := flag.NewFlagSet("judge", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.StringVar(&o.doc, "doc", o.doc, "architecture declaration")
	fs.StringVar(&o.cfg.Provider, "provider", o.cfg.Provider, "anthropic|gemini|openai")
	fs.StringVar(&o.cfg.Model, "model", o.cfg.Model, "model id")
	fs.StringVar(&o.cfg.BaseURL, "base-url", o.cfg.BaseURL, "endpoint override")
	fs.StringVar(&o.cfg.Effort, "effort", o.cfg.Effort, "anthropic effort")
	fs.StringVar(&o.base, "base", "", "git ref to diff against")
	fs.StringVar(&o.layer, "group", "", "only this group")
	fs.StringVar(&o.layer, "layer", "", "synonym of --group")
	fs.StringVar(&o.emit, "emit", "", "emit JSONL: findings")
	fs.StringVar(&o.cache, "cache", o.cache, "response cache dir")
	fs.IntVar(&o.context, "context", o.context, "source lines per declaration")
	fs.IntVar(&o.jobs, "jobs", o.jobs, "parallel requests")
	fs.BoolVar(&o.includeTests, "include-tests", false, "judge test files too")
	fs.StringVar(&o.answers, "answers", "", "verify: answers JSONL")

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
		return o, fmt.Errorf("expected one directory, got %d", len(positional))
	}
	o.dir = "."
	if len(positional) == 1 {
		o.dir = positional[0]
	}
	return o, nil
}

// gather reads the document and collects the declarations to judge.
func gather(o opts) (doc string, d *arch.Decl, cases []judge.Case, err error) {
	docPath := o.doc
	if !filepath.IsAbs(docPath) {
		docPath = filepath.Join(o.dir, o.doc)
	}
	raw, err := os.ReadFile(docPath)
	if err != nil {
		return "", nil, nil, fmt.Errorf("reading %s: %w\n\nThe judge reads intent from the document; without one there is nothing to judge against", docPath, err)
	}
	d, err = arch.ParseDoc(string(raw))
	if err != nil {
		return "", nil, nil, fmt.Errorf("%s: %w", o.doc, err)
	}
	decls, err := arch.ScanDecls(o.dir, d, o.includeTests)
	if err != nil {
		return "", nil, nil, err
	}
	if o.base != "" {
		changed, err := changedFiles(o.dir, o.base)
		if err != nil {
			return "", nil, nil, err
		}
		decls = keep(decls, func(x arch.Declaration) bool { return changed[x.File] })
	}
	if o.layer != "" {
		decls = keep(decls, func(x arch.Declaration) bool { return x.Layer == o.layer })
	}
	for _, dc := range decls {
		ex, err := judge.Excerpt(o.dir, dc, o.context)
		if err != nil {
			return "", nil, nil, err
		}
		cases = append(cases, judge.Case{Decl: dc, Excerpt: ex})
	}
	return string(raw), d, cases, nil
}

func cmdCases(args []string) error {
	o, err := parseArgs(args)
	if err != nil {
		return err
	}
	_, _, cases, err := gather(o)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(os.Stdout)
	for _, c := range cases {
		if err := enc.Encode(c); err != nil {
			return err
		}
	}
	fmt.Fprintf(os.Stderr, "%d cases\n", len(cases))
	return nil
}

func cmdVerify(args []string) error {
	o, err := parseArgs(args)
	if err != nil {
		return err
	}
	if o.answers == "" {
		return fmt.Errorf("verify needs --answers FILE (JSONL, one object per declaration: file, line, name, in, belongs, layer, reason, citation)")
	}
	doc, d, cases, err := gather(o)
	if err != nil {
		return err
	}
	f, err := os.Open(o.answers)
	if err != nil {
		return err
	}
	defer f.Close()
	var answered []judge.Answered
	dec := json.NewDecoder(f)
	for dec.More() {
		var a judge.Answered
		if err := dec.Decode(&a); err != nil {
			return fmt.Errorf("%s: %w", o.answers, err)
		}
		answered = append(answered, a)
	}
	res, unjudged := judge.Verify(judge.Options{Doc: doc, Decl: d}, cases, answered)
	p := external{o.cfg.Model}
	if unjudged > 0 {
		fmt.Fprintf(os.Stderr, "%d declarations had no answer\n", unjudged)
	}
	return report(o, res, p, gitLog1(o.dir, o.doc))
}

// external names the source of answers that did not come from a provider, so
// the finding records still say who judged.
type external struct{ label string }

func (e external) Name() string {
	if e.label == "" {
		return "external/unspecified"
	}
	return "external/" + e.label
}
func (e external) Ask(context.Context, string, string) (json.RawMessage, judge.Usage, error) {
	return nil, judge.Usage{}, fmt.Errorf("external answers cannot be asked")
}

func cmdScan(args []string) error {
	o, err := parseArgs(args)
	if err != nil {
		return err
	}
	doc, d, cases, err := gather(o)
	if err != nil {
		return err
	}
	raw := doc
	p, err := judge.New(o.cfg)
	if err != nil {
		return err
	}
	if len(cases) == 0 {
		fmt.Fprintln(os.Stderr, "nothing to judge")
		return nil
	}

	cache := o.cache
	if cache == "-" {
		cache = ""
	} else if !filepath.IsAbs(cache) {
		cache = filepath.Join(o.dir, cache)
	}
	fmt.Fprintf(os.Stderr, "judging %d declarations with %s\n", len(cases), p.Name())
	res, err := judge.Run(context.Background(), p, judge.Options{
		Doc: string(raw), Decl: d, CacheDir: cache, Jobs: o.jobs,
	}, cases)
	if err != nil {
		return err
	}
	return report(o, res, p, gitLog1(o.dir, o.doc))
}

func report(o opts, res *judge.Result, p judge.Provider, declSHA string) error {
	if o.emit == "findings" {
		enc := schema.NewEncoder(os.Stdout)
		for _, f := range res.ModelFindings(p, declSHA) {
			if err := enc.Write(&schema.Finding{
				Tool: "judge/" + p.Name(), Rule: f.Rule, Severity: schema.SevWarn,
				File: f.File, Line: f.Line, Message: f.Message, Suggest: f.Suggest,
				Symbol: declSHA, Fingerprint: f.Fingerprint,
			}); err != nil {
				return err
			}
		}
		if err := enc.Flush(); err != nil {
			return err
		}
		printSummary(res)
		return nil
	}

	for _, f := range res.Findings {
		fmt.Printf("%s:%d  %s\n", f.Decl.File, f.Decl.Line, f.Decl.Name)
		fmt.Printf("    %s → %s: %s\n", f.Decl.Layer, f.Layer, strings.TrimSuffix(f.Reason, "."))
		fmt.Printf("    doc: %q\n\n", strings.Join(strings.Fields(f.Citation), " "))
	}
	for _, dr := range res.Dropped {
		fmt.Printf("dropped %s (%s): %s\n", dr.Case.Decl.Name, dr.Case.Decl.File, dr.Why)
	}
	printSummary(res)
	return nil
}

func printSummary(r *judge.Result) {
	fmt.Fprintf(os.Stderr, "%d judged, %d belong, %d findings, %d dropped, %d from cache; %d in / %d out tokens\n",
		r.Judged, r.Belongs, len(r.Findings), len(r.Dropped), r.Cached, r.Usage.Input, r.Usage.Output)
}

func cmdDoctor(args []string) error {
	o, err := parseArgs(args)
	if err != nil {
		return err
	}
	p, err := judge.New(o.cfg)
	if err != nil {
		return err
	}
	d := &arch.Decl{Layers: map[string][]string{"a": {"a"}, "b": {"b"}}, Order: []string{"a", "b"}, Owns: map[string][]string{}}
	doc := "# Architecture\n\nGroup a holds apples. Group b holds bananas.\n"
	c := judge.Case{Decl: arch.Declaration{Layer: "a", Name: "PeelBanana", File: "a/x.go", Line: 1}, Excerpt: "func PeelBanana() {}"}
	raw, usage, err := p.Ask(context.Background(), judge.System(doc), judge.Request(d, c))
	if err != nil {
		return err
	}
	fmt.Printf("%s answered (%d in / %d out tokens):\n%s\n", p.Name(), usage.Input, usage.Output, string(raw))
	return nil
}

func keep(xs []arch.Declaration, f func(arch.Declaration) bool) []arch.Declaration {
	out := xs[:0]
	for _, x := range xs {
		if f(x) {
			out = append(out, x)
		}
	}
	return out
}

func changedFiles(dir, base string) (map[string]bool, error) {
	out, err := exec.Command("git", "-C", dir, "diff", "--name-only", base).Output()
	if err != nil {
		return nil, fmt.Errorf("git diff against %s: %w", base, err)
	}
	m := map[string]bool{}
	for _, l := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if l != "" {
			m[l] = true
		}
	}
	return m, nil
}

func gitLog1(dir, doc string) string {
	out, err := exec.Command("git", "-C", dir, "log", "-1", "--format=%H", "--", doc).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
