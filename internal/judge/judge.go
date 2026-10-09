// Package judge asks a language model whether a declaration belongs where it
// is, according to what ARCHITECTURE.md says each part is for.
//
// Plumb's two rules are deterministic: forbid over the import graph, owns over
// declared names. Both are blind to a function named ApplyDiscount in the cart
// group that actually averages ratings. A judge that reads the body and the
// document is not. It is also probabilistic, so it is built as one more linter
// under the same discipline as every other rule, not as an oracle:
//
//   - It emits ordinary findings. ratchet, strata and docket compose over them
//     without knowing a model was involved.
//   - Its rule id carries the prompt version. Precision is a property of the
//     prompt, and a reworded prompt is a new rule with its own record.
//   - Every finding must cite a sentence that exists verbatim in the document.
//     A citation that is not there is a hallucination, and the finding is
//     dropped before anyone sees it.
//   - The fingerprint is the symbol, not the explanation. The model's wording
//     varies from run to run; the thing it is talking about does not.
//   - Findings are warnings. Nothing gates on them until a team has measured
//     the precision and chosen to.
package judge

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/sherzing/assay/internal/arch"
	"github.com/sherzing/assay/internal/model"
)

//go:embed prompts/intent-drift.v2.md
var prompt string

// Rule is the rule id. The suffix is the prompt version: bump it when the
// prompt changes, so precision data never mixes two prompts.
const Rule = "intent-drift@2"

// answerProperties is the shape of one answer; Schema and the batch schema are
// both built from it so the two can never drift apart.
func answerProperties() (map[string]any, []string) {
	return map[string]any{
		"belongs":  map[string]any{"type": "boolean", "description": "true if the declaration belongs in its group"},
		"layer":    map[string]any{"type": "string", "description": "the group it belongs in; the current group when belongs is true"},
		"reason":   map[string]any{"type": "string", "description": "one sentence naming the concept that is out of place"},
		"citation": map[string]any{"type": "string", "description": "one sentence copied verbatim from the architecture document"},
	}, []string{"belongs", "layer", "reason", "citation"}
}

// Schema is the JSON shape every provider must return.
var Schema = func() map[string]any {
	props, required := answerProperties()
	return map[string]any{
		"type":                 "object",
		"properties":           props,
		"required":             required,
		"additionalProperties": false,
	}
}()

// Answer is a provider's parsed reply.
type Answer struct {
	Belongs  bool   `json:"belongs"`
	Layer    string `json:"layer"`
	Reason   string `json:"reason"`
	Citation string `json:"citation"`
}

// Usage is what a call cost, for the summary line.
type Usage struct {
	Input, Output int
}

// Provider is one model behind one wire protocol.
type Provider interface {
	// Name identifies provider and model, e.g. anthropic/claude-opus-5.
	Name() string
	// Ask sends the system prompt and the request and returns the raw JSON
	// object the model produced, which the caller validates.
	Ask(ctx context.Context, system, user string) (json.RawMessage, Usage, error)
}

// Batcher is a provider that can judge several cases in one call. Claude Code
// loads its own context on every invocation, so one call per declaration would
// spend most of a plan's window on overhead; batching is what makes it usable.
type Batcher interface {
	Provider
	// BatchSize is the most cases one call should carry.
	BatchSize() int
	// AskBatch returns one raw answer per request, in order.
	AskBatch(ctx context.Context, system string, users []string) ([]json.RawMessage, Usage, error)
}

// Case is one declaration to judge, with the code around it.
type Case struct {
	Decl    arch.Declaration `json:"decl"`
	Excerpt string           `json:"excerpt"`
}

// Answered pairs an answer with the declaration it is about, so a judgement
// made outside this process — by a person, or by Claude Code running the
// skill — can be verified here on the same terms as one made by a provider.
type Answered struct {
	File string `json:"file"`
	Line int    `json:"line"`
	Name string `json:"name"`
	In   string `json:"in"` // the group it is declared in
	Answer
}

// Finding is a judged drift with everything needed to act on it.
type Finding struct {
	Case
	Answer
}

// Fingerprint hashes the symbol, not the explanation.
func (f Finding) Fingerprint() string {
	h := sha256.Sum256([]byte(strings.Join([]string{Rule, f.Decl.Layer, f.Decl.Name}, "\x00")))
	return hex.EncodeToString(h[:8])
}

// Options controls a run.
type Options struct {
	Doc      string // the whole ARCHITECTURE.md
	Decl     *arch.Decl
	CacheDir string // "" disables the cache
	Jobs     int
}

// Result is what a run produced.
type Result struct {
	Findings []Finding
	Judged   int
	Belongs  int
	Dropped  []Dropped // answers that failed verification
	Cached   int
	Usage    Usage
}

// Dropped records why an answer was not turned into a finding, because "the
// model said something and we threw it away" should be visible, not silent.
type Dropped struct {
	Case   Case
	Answer Answer
	Why    string
}

// System builds the prompt prefix: instructions, then the document. Identical
// for every case in a run and across runs until the document changes, which is
// what makes it worth caching at the provider.
func System(doc string) string {
	return prompt + doc
}

// Request renders one case.
func Request(d *arch.Decl, c Case) string {
	var b strings.Builder
	b.WriteString("Groups, with the directories each covers:\n")
	for _, name := range d.Order {
		fmt.Fprintf(&b, "  %-12s %s\n", name, strings.Join(d.Layers[name], " "))
	}
	if terms, ok := d.Owns[c.Decl.Layer]; ok {
		fmt.Fprintf(&b, "\nDeclared vocabulary of %s: %s\n", c.Decl.Layer, strings.Join(terms, " "))
	}
	fmt.Fprintf(&b, "\nDeclaration under review:\n  group: %s\n  name:  %s\n  file:  %s:%d\n\n```\n%s\n```\n",
		c.Decl.Layer, c.Decl.Name, c.Decl.File, c.Decl.Line, c.Excerpt)
	return b.String()
}

// Run judges every case, with the cache in front of the provider, then
// verifies every answer.
func Run(ctx context.Context, p Provider, o Options, cases []Case) (*Result, error) {
	if o.Jobs <= 0 {
		o.Jobs = 4
	}
	system := System(o.Doc)

	type slot struct {
		raw    json.RawMessage
		cached bool
		usage  Usage
		err    error
	}
	out := make([]slot, len(cases))
	users := make([]string, len(cases))
	for i, c := range cases {
		users[i] = Request(o.Decl, c)
	}

	// Cache first, so a batch only carries what is genuinely unanswered.
	var pending []int
	for i := range cases {
		if raw, ok := cached(o, p, system, users[i]); ok {
			out[i] = slot{raw: raw, cached: true}
			continue
		}
		pending = append(pending, i)
	}

	// Group into calls: one per case, or BatchSize per call for a Batcher.
	size := 1
	b, batching := p.(Batcher)
	if batching && b.BatchSize() > 1 {
		size = b.BatchSize()
	}
	var groups [][]int
	for len(pending) > 0 {
		n := min(size, len(pending))
		groups = append(groups, pending[:n])
		pending = pending[n:]
	}

	sem := make(chan struct{}, o.Jobs)
	var wg sync.WaitGroup
	for _, g := range groups {
		wg.Add(1)
		go func(g []int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			var raws []json.RawMessage
			var usage Usage
			var err error
			if len(g) == 1 && !batching {
				var raw json.RawMessage
				raw, usage, err = p.Ask(ctx, system, users[g[0]])
				raws = []json.RawMessage{raw}
			} else {
				us := make([]string, len(g))
				for j, i := range g {
					us[j] = users[i]
				}
				raws, usage, err = b.AskBatch(ctx, system, us)
				if err == nil && len(raws) != len(g) {
					err = fmt.Errorf("provider answered %d of %d cases", len(raws), len(g))
				}
			}
			if err != nil {
				out[g[0]] = slot{err: fmt.Errorf("%s: %w", cases[g[0]].Decl.Name, err)}
				return
			}
			for j, i := range g {
				out[i] = slot{raw: raws[j]}
				if err := store(o, p, system, users[i], raws[j]); err != nil {
					out[i] = slot{err: err}
				}
			}
			out[g[0]].usage = usage // attribute the call's cost once
		}(g)
	}
	wg.Wait()

	res := &Result{}
	answers := make([]Answer, len(cases))
	for i, s := range out {
		if s.err != nil {
			return res, s.err
		}
		if err := json.Unmarshal(s.raw, &answers[i]); err != nil {
			return res, fmt.Errorf("%s: model returned invalid JSON: %w", cases[i].Decl.Name, err)
		}
		res.Usage.Input += s.usage.Input
		res.Usage.Output += s.usage.Output
		if s.cached {
			res.Cached++
		}
	}
	verify(res, o, cases, answers)
	return res, nil
}

// Verify checks answers produced elsewhere — by a person, or by Claude Code
// running the judge skill — on exactly the terms a provider's answers get.
// Answers are matched to declarations by file, line and name; an answer for a
// declaration that is not in the scan is ignored, and a declaration with no
// answer is counted as unjudged rather than as belonging.
func Verify(o Options, cases []Case, answered []Answered) (*Result, int) {
	byKey := map[string]Answer{}
	for _, a := range answered {
		byKey[fmt.Sprintf("%s:%d:%s", a.File, a.Line, a.Name)] = a.Answer
	}
	var have []Case
	var answers []Answer
	unjudged := 0
	for _, c := range cases {
		a, ok := byKey[fmt.Sprintf("%s:%d:%s", c.Decl.File, c.Decl.Line, c.Decl.Name)]
		if !ok {
			unjudged++
			continue
		}
		have = append(have, c)
		answers = append(answers, a)
	}
	res := &Result{}
	verify(res, o, have, answers)
	return res, unjudged
}

func verify(res *Result, o Options, cases []Case, answers []Answer) {
	docNorm := normalise(o.Doc)
	layers := map[string]bool{}
	for _, l := range o.Decl.Order {
		layers[l] = true
	}
	for i, c := range cases {
		ans := answers[i]
		res.Judged++
		if ans.Belongs {
			res.Belongs++
			continue
		}
		// Verification. Each of these is a way for a plausible answer to be
		// wrong in a way a reader could not tell from the text.
		switch {
		case !layers[ans.Layer]:
			res.Dropped = append(res.Dropped, Dropped{c, ans, fmt.Sprintf("names a group that is not declared: %q", ans.Layer)})
		case ans.Layer == c.Decl.Layer:
			res.Dropped = append(res.Dropped, Dropped{c, ans, "says it does not belong but names the same group"})
		case strings.TrimSpace(ans.Citation) == "":
			res.Dropped = append(res.Dropped, Dropped{c, ans, "no citation"})
		case !strings.Contains(docNorm, normalise(ans.Citation)):
			res.Dropped = append(res.Dropped, Dropped{c, ans, "citation is not in the document"})
		default:
			res.Findings = append(res.Findings, Finding{c, ans})
		}
	}
	sort.Slice(res.Findings, func(i, j int) bool {
		a, b := res.Findings[i].Decl, res.Findings[j].Decl
		if a.File != b.File {
			return a.File < b.File
		}
		return a.Line < b.Line
	})
}

func cacheKey(o Options, p Provider, system, user string) string {
	if o.CacheDir == "" {
		return ""
	}
	h := sha256.Sum256([]byte(strings.Join([]string{Rule, p.Name(), system, user}, "\x00")))
	return filepath.Join(o.CacheDir, hex.EncodeToString(h[:16])+".json")
}

func cached(o Options, p Provider, system, user string) (json.RawMessage, bool) {
	key := cacheKey(o, p, system, user)
	if key == "" {
		return nil, false
	}
	raw, err := os.ReadFile(key)
	return raw, err == nil
}

func store(o Options, p Provider, system, user string, raw json.RawMessage) error {
	key := cacheKey(o, p, system, user)
	if key == "" {
		return nil
	}
	if err := os.MkdirAll(o.CacheDir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(key, raw, 0o644)
}

// normalise collapses whitespace so a citation survives line wrapping in the
// markdown. It does not lower-case: a quote is a quote.
func normalise(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// ModelFindings converts to the shared finding type.
func (r *Result) ModelFindings(p Provider, declSHA string) []model.Finding {
	out := make([]model.Finding, 0, len(r.Findings))
	for _, f := range r.Findings {
		out = append(out, model.Finding{
			Rule:        Rule,
			Severity:    model.Warn,
			File:        f.Decl.File,
			Line:        f.Decl.Line,
			Func:        declSHA,
			Message:     fmt.Sprintf("%s declares %s: %s (belongs in %s)", f.Decl.Layer, f.Decl.Name, strings.TrimSuffix(f.Reason, "."), f.Layer),
			Suggest:     fmt.Sprintf("move %s into %s — the document says: %q", f.Decl.Name, f.Layer, normalise(f.Citation)),
			Fingerprint: f.Fingerprint(),
		})
	}
	return out
}

// Excerpt returns the source from the declaration's line for up to maxLines,
// stopping at the next top-level declaration so the model sees one thing.
func Excerpt(root string, d arch.Declaration, maxLines int) (string, error) {
	src, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(d.File)))
	if err != nil {
		return "", err
	}
	lines := strings.Split(string(src), "\n")
	if d.Line < 1 || d.Line > len(lines) {
		return "", fmt.Errorf("%s:%d: line out of range", d.File, d.Line)
	}
	start := d.Line - 1
	end := start + 1
	for end < len(lines) && end-start < maxLines {
		l := lines[end]
		// A new unindented declaration means we have left this one.
		if len(l) > 0 && l[0] != ' ' && l[0] != '\t' && l[0] != '}' && l[0] != ')' && !strings.HasPrefix(l, "//") && !strings.HasPrefix(l, "#") && !strings.HasPrefix(l, "@") {
			if end > start+1 {
				break
			}
		}
		end++
	}
	return strings.Join(lines[start:end], "\n"), nil
}
