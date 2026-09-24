// Command docket turns findings into tickets, and keeps them honest.
//
//	docket plan     what would be created, and why
//	docket create   create them (dry-run unless --yes)
//	docket sync     update progress, close what is fixed
//	docket status   what is open, closed, and how far along
//
// Reads findings as JSONL on stdin:
//
//	ratchet scan . --emit findings | docket plan
//	ratchet scan . --emit findings | docket create --provider linear --team ENG --yes
package main

import (
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/sherzing/assay/internal/docket"
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
	case "plan":
		err = cmdPlan(os.Args[2:])
	case "create":
		err = cmdCreate(os.Args[2:])
	case "sync":
		err = cmdSync(os.Args[2:])
	case "status":
		err = cmdStatus(os.Args[2:])
	case "version", "-v", "--version":
		fmt.Println("docket", version)
	case "help", "-h", "--help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "docket: unknown command %q\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "docket:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `docket — turn findings into tickets, and keep them honest

usage:
  … | docket plan   [--group-by theme|file|finding] [--max N] [--min-size N]
  … | docket create [--provider linear] [--team ENG] [--label X] [--yes]
  … | docket sync   [--provider linear] [--yes]
  docket status     [--store DIR]

  --yes is required to touch an external tracker. Without it everything is a
  preview, because creating tickets in someone else's workspace should never be
  the default behaviour of a first run.

findings come in on stdin:
  ratchet scan . --emit findings --repo myservice | docket plan
  ratchet import roslyn.sarif --emit findings --repo myservice | docket plan
`)
}

func readFindings() ([]schema.Finding, error) {
	st, _ := os.Stdin.Stat()
	if st != nil && st.Mode()&os.ModeCharDevice != 0 {
		return nil, fmt.Errorf("no input: pipe findings JSONL in\n" +
			"  try: ratchet scan . --emit findings --repo NAME | docket plan")
	}
	var out []schema.Finding
	err := schema.Decode(os.Stdin, func(rec schema.Record) error {
		if rec.Finding != nil {
			out = append(out, *rec.Finding)
		}
		return nil
	}, nil)
	return out, err
}

type common struct {
	dir     *string
	groupBy *string
	max     *int
	minSize *int
	sev     *string
}

func bind(fs *flag.FlagSet) common {
	return common{
		dir:     fs.String("store", ".assay", "store directory"),
		groupBy: fs.String("group-by", "theme", "theme|file|finding"),
		max:     fs.Int("max", 20, "cap on tickets per run (0 = no cap)"),
		minSize: fs.Int("min-size", 1, "skip cohorts smaller than this"),
		sev:     fs.String("severity", "", "minimum severity: error|warn|info"),
	}
}

func (c common) options() docket.Options {
	return docket.Options{
		GroupBy:  docket.GroupBy(*c.groupBy),
		Max:      *c.max,
		MinSize:  *c.minSize,
		Severity: schema.Severity(*c.sev),
	}
}

// load reads verdicts and existing tickets so a plan can exclude noise and
// avoid duplicating work.
func load(dir string) (map[string]schema.Verdict, []schema.Ticket, error) {
	s, err := store.Open(dir)
	if err != nil {
		return nil, nil, err
	}
	vs, err := s.Verdicts()
	if err != nil {
		return nil, nil, err
	}
	ts, err := s.Tickets()
	if err != nil {
		return nil, nil, err
	}
	return vs, docket.Latest(ts), nil
}

func printPlan(p docket.Plan, groupBy string) {
	if p.SkippedFP > 0 {
		fmt.Printf("skipped %d findings marked false-positive", p.SkippedFP)
		if len(p.SkippedRules) > 0 {
			fmt.Printf(" (%s)", strings.Join(p.SkippedRules, ", "))
		}
		fmt.Println(" — fix the rule, do not file work")
	}
	if p.SkippedTicket > 0 {
		fmt.Printf("skipped %d findings already covered by a ticket\n", p.SkippedTicket)
	}
	if len(p.Groups) == 0 {
		fmt.Println("nothing to ticket")
		return
	}
	total := 0
	for _, g := range p.Groups {
		total += len(g.Fingerprints)
	}
	fmt.Printf("\n%d tickets covering %d findings (grouped by %s)\n\n", len(p.Groups), total, groupBy)
	for i, g := range p.Groups {
		fmt.Printf("%2d. %-64s %3d findings\n", i+1, trunc(g.Title, 64), len(g.Fingerprints))
	}
}

func cmdPlan(args []string) error {
	fs := flag.NewFlagSet("plan", flag.ExitOnError)
	c := bind(fs)
	verbose := fs.Bool("v", false, "print each ticket body")
	fs.Parse(args)

	findings, err := readFindings()
	if err != nil {
		return err
	}
	verdicts, existing, err := load(*c.dir)
	if err != nil {
		return err
	}
	p := docket.BuildPlan(findings, verdicts, existing, c.options())
	printPlan(p, *c.groupBy)
	if *verbose {
		for _, g := range p.Groups {
			fmt.Printf("\n─── %s ───\n%s\n", g.Title, g.Body)
		}
	}
	return nil
}

func cmdCreate(args []string) error {
	fs := flag.NewFlagSet("create", flag.ExitOnError)
	c := bind(fs)
	providerName := fs.String("provider", "linear", "linear|file")
	team := fs.String("team", "", "Linear team key, e.g. ENG")
	token := fs.String("token", "", "API token (default $LINEAR_API_KEY)")
	labels := fs.String("label", "tech-debt", "comma-separated labels")
	yes := fs.Bool("yes", false, "actually create — without this it is a preview")
	fs.Parse(args)

	findings, err := readFindings()
	if err != nil {
		return err
	}
	verdicts, existing, err := load(*c.dir)
	if err != nil {
		return err
	}
	p := docket.BuildPlan(findings, verdicts, existing, c.options())
	printPlan(p, *c.groupBy)
	if len(p.Groups) == 0 {
		return nil
	}

	var prov docket.Provider = docket.DryRun{W: os.Stdout}
	if *yes {
		switch *providerName {
		case "linear":
			prov, err = docket.NewLinear(*token, *team)
			if err != nil {
				return err
			}
		case "file":
			prov = &docket.Files{Dir: *c.dir + "/tickets/md"}
		default:
			return fmt.Errorf("unknown provider %q", *providerName)
		}
	} else {
		fmt.Printf("\nPREVIEW — nothing will be created. Add --yes to create for real.\n")
	}

	var labelList []string
	for _, l := range strings.Split(*labels, ",") {
		if l = strings.TrimSpace(l); l != "" {
			labelList = append(labelList, l)
		}
	}

	s, err := store.Open(*c.dir)
	if err != nil {
		return err
	}
	created := 0
	for _, g := range p.Groups {
		id, url, err := prov.Create(g.Title, g.Body, labelList)
		if err != nil {
			// Stop rather than continue: a partial run with an auth or rate
			// error would otherwise spray half a backlog and leave the mapping
			// inconsistent.
			return fmt.Errorf("after creating %d: %w", created, err)
		}
		created++
		if !*yes {
			continue
		}
		t := docket.ToTicket(g, prov.Name(), id, url, docket.GroupBy(*c.groupBy))
		if err := s.AppendTicket(t); err != nil {
			return fmt.Errorf("created %s but failed to record it: %w", id, err)
		}
		fmt.Printf("created %-14s %s\n", id, trunc(g.Title, 60))
	}
	if *yes {
		fmt.Printf("\ncreated %d tickets, recorded in %s\n", created, *c.dir)
	}
	return nil
}

func cmdSync(args []string) error {
	fs := flag.NewFlagSet("sync", flag.ExitOnError)
	dir := fs.String("store", ".assay", "store directory")
	providerName := fs.String("provider", "linear", "linear|file")
	team := fs.String("team", "", "Linear team key")
	token := fs.String("token", "", "API token (default $LINEAR_API_KEY)")
	yes := fs.Bool("yes", false, "actually close resolved tickets")
	fs.Parse(args)

	findings, err := readFindings()
	if err != nil {
		return err
	}
	s, err := store.Open(*dir)
	if err != nil {
		return err
	}
	all, err := s.Tickets()
	if err != nil {
		return err
	}
	open := []schema.Ticket{}
	for _, t := range docket.Latest(all) {
		if t.State != "closed" {
			open = append(open, t)
		}
	}
	if len(open) == 0 {
		fmt.Println("no open tickets")
		return nil
	}

	progress := docket.Sync(open, findings)
	var done []docket.Progress
	fmt.Printf("%-14s %-52s %s\n", "ticket", "title", "progress")
	for _, p := range progress {
		bar := fmt.Sprintf("%d/%d", p.Resolved, p.Total)
		mark := ""
		if p.Done {
			mark = "  ✓ ready to close"
			done = append(done, p)
		}
		fmt.Printf("%-14s %-52s %s%s\n", p.Ticket.ID, trunc(p.Ticket.Title, 52), bar, mark)
	}
	if len(done) == 0 {
		fmt.Println("\nnothing fully resolved yet")
		return nil
	}

	if !*yes {
		fmt.Printf("\n%d tickets are fully resolved. Add --yes to close them.\n", len(done))
		return nil
	}
	var prov docket.Provider
	switch *providerName {
	case "linear":
		prov, err = docket.NewLinear(*token, *team)
		if err != nil {
			return err
		}
	case "file":
		prov = &docket.Files{Dir: *dir + "/tickets/md"}
	default:
		return fmt.Errorf("unknown provider %q", *providerName)
	}
	for _, p := range done {
		msg := fmt.Sprintf("All %d findings in this cohort no longer reproduce. Closed by assay docket.", p.Total)
		if err := prov.Close(p.Ticket.ID, msg); err != nil {
			return fmt.Errorf("close %s: %w", p.Ticket.ID, err)
		}
		t := p.Ticket
		t.State, t.TS = "closed", time.Now().UTC()
		t.Resolved = t.Fingerprints
		if err := s.AppendTicket(t); err != nil {
			return err
		}
		fmt.Printf("closed %s\n", t.ID)
	}
	return nil
}

func cmdStatus(args []string) error {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	dir := fs.String("store", ".assay", "store directory")
	fs.Parse(args)

	s, err := store.Open(*dir)
	if err != nil {
		return err
	}
	all, err := s.Tickets()
	if err != nil {
		return err
	}
	latest := docket.Latest(all)
	if len(latest) == 0 {
		fmt.Println("no tickets recorded")
		return nil
	}
	var open, closed, covered int
	fmt.Printf("%-14s %-8s %-52s %s\n", "ticket", "state", "title", "findings")
	for _, t := range latest {
		if t.State == "closed" {
			closed++
		} else {
			open++
		}
		covered += len(t.Fingerprints)
		fmt.Printf("%-14s %-8s %-52s %d\n", t.ID, t.State, trunc(t.Title, 52), len(t.Fingerprints))
	}
	fmt.Printf("\n%d open, %d closed, %d findings covered\n", open, closed, covered)
	return nil
}

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}
