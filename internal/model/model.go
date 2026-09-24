// Package model holds the wire types ratchet emits. The JSON schema is a
// contract: trend data is only useful if records written months apart stay
// readable, so SchemaVersion changes whenever a field's meaning changes.
package model

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"
)

// SchemaVersion is bumped on any breaking change to the emitted JSON.
const SchemaVersion = "1"

// Severity ranks a finding. Only Error participates in the ratchet gate;
// Warn and Info are reported so they can be watched without blocking anyone.
type Severity string

const (
	Error Severity = "error"
	Warn  Severity = "warn"
	Info  Severity = "info"
)

// Finding is one actionable violation. Every field exists to answer "what do I
// do about it": Rule says which check, File/Line says where, Suggest says how.
// A metric without these is a number nobody can act on.
type Finding struct {
	Rule        string   `json:"rule"`
	Severity    Severity `json:"severity"`
	File        string   `json:"file"`
	Line        int      `json:"line"`
	Col         int      `json:"col"`
	Func        string   `json:"func,omitempty"`
	Message     string   `json:"message"`
	Suggest     string   `json:"suggest,omitempty"`
	Fingerprint string   `json:"fingerprint"`

	// Verdict is a human judgement resolved from an in-code annotation or
	// project config. Empty means nobody has looked at it yet — which is
	// different from "accepted", and the distinction matters when computing
	// rule precision: an unjudged finding is not evidence either way.
	Verdict      string `json:"verdict,omitempty"`
	VerdictWhy   string `json:"verdictReason,omitempty"`
	VerdictUntil string `json:"verdictUntil,omitempty"`
	VerdictFrom  string `json:"verdictSource,omitempty"`
}

// Fingerprint identifies a finding across edits that move it around.
//
// Deliberately excludes the line number. If the fingerprint moved every time
// someone reformatted a file, every reformat would read as a wave of new
// violations and the team would switch the gate off inside a week. Keyed on
// file, rule, enclosing function and a whitespace-normalised snippet instead.
func Fingerprint(file, rule, fn, snippet string) string {
	h := sha256.New()
	h.Write([]byte(file + "\x00" + rule + "\x00" + fn + "\x00" + normalise(snippet)))
	return hex.EncodeToString(h.Sum(nil))[:16]
}

func normalise(s string) string { return strings.Join(strings.Fields(s), " ") }

// FuncMetrics is the per-function measurement record.
type FuncMetrics struct {
	Name       string `json:"name"`
	Recv       string `json:"recv,omitempty"`
	File       string `json:"file"`
	Line       int    `json:"line"`
	Exported   bool   `json:"exported"`
	Cyclomatic int    `json:"cyclomatic"`
	Cognitive  int    `json:"cognitive"`
	MaxNesting int    `json:"maxNesting"`
	Statements int    `json:"statements"`
	Params     int    `json:"params"`
	Results    int    `json:"results"`
}

// Key identifies a function stably enough to compare across commits. Line is
// excluded for the same reason as in Fingerprint.
func (f FuncMetrics) Key() string {
	if f.Recv != "" {
		return f.File + ":" + f.Recv + "." + f.Name
	}
	return f.File + ":" + f.Name
}

// Dist summarises a metric across functions. We carry the distribution rather
// than a mean because a mean hides exactly the tail we care about: one
// unmaintainable function in a thousand tidy ones barely moves an average.
type Dist struct {
	P50  int     `json:"p50"`
	P90  int     `json:"p90"`
	Max  int     `json:"max"`
	Sum  int     `json:"sum"`
	Mean float64 `json:"mean"`
}

// NewDist builds a distribution from raw values.
func NewDist(vals []int) Dist {
	if len(vals) == 0 {
		return Dist{}
	}
	s := append([]int(nil), vals...)
	sort.Ints(s)
	d := Dist{P50: pct(s, 0.50), P90: pct(s, 0.90), Max: s[len(s)-1]}
	for _, v := range s {
		d.Sum += v
	}
	d.Mean = float64(d.Sum) / float64(len(s))
	return d
}

func pct[T int | float64](sorted []T, p float64) T {
	if len(sorted) == 0 {
		var zero T
		return zero
	}
	i := int(p * float64(len(sorted)-1))
	return sorted[i]
}

// FloatDist is Dist for a metric that is not integer-valued, such as an imported maintainability index.
type FloatDist struct {
	P50  float64 `json:"p50"`
	P90  float64 `json:"p90"`
	Max  float64 `json:"max"`
	Mean float64 `json:"mean"`
}

// NewFloatDist builds a distribution with the same percentile rule as NewDist.
func NewFloatDist(vals []float64) FloatDist {
	if len(vals) == 0 {
		return FloatDist{}
	}
	s := append([]float64(nil), vals...)
	sort.Float64s(s)
	d := FloatDist{P50: pct(s, 0.50), P90: pct(s, 0.90), Max: s[len(s)-1]}
	sum := 0.0
	for _, v := range s {
		sum += v
	}
	d.Mean = sum / float64(len(s))
	return d
}

// Summary is the roll-up used for the trend series. These are the numbers that
// get plotted over time; the per-function records are for acting on a hotspot.
type Summary struct {
	Files          int            `json:"files"`
	Funcs          int            `json:"funcs"`
	Statements     int            `json:"statements"`
	Cyclomatic     Dist           `json:"cyclomatic"`
	Cognitive      Dist           `json:"cognitive"`
	MaxNesting     Dist           `json:"maxNesting"`
	FindingsByRule map[string]int `json:"findingsByRule"`
	FindingsTotal  int            `json:"findingsTotal"`
}

// Report is the top-level output of a scan.
type Report struct {
	SchemaVersion string        `json:"schemaVersion"`
	Root          string        `json:"root"`
	Commit        string        `json:"commit,omitempty"`
	Summary       Summary       `json:"summary"`
	Funcs         []FuncMetrics `json:"funcs,omitempty"`
	Findings      []Finding     `json:"findings"`
}

// Summarise computes the roll-up from the detail records.
func (r *Report) Summarise(files int) {
	cyc := make([]int, 0, len(r.Funcs))
	cog := make([]int, 0, len(r.Funcs))
	nest := make([]int, 0, len(r.Funcs))
	stmts := 0
	for _, f := range r.Funcs {
		cyc = append(cyc, f.Cyclomatic)
		cog = append(cog, f.Cognitive)
		nest = append(nest, f.MaxNesting)
		stmts += f.Statements
	}
	byRule := map[string]int{}
	for _, f := range r.Findings {
		byRule[f.Rule]++
	}
	r.SchemaVersion = SchemaVersion
	r.Summary = Summary{
		Files:          files,
		Funcs:          len(r.Funcs),
		Statements:     stmts,
		Cyclomatic:     NewDist(cyc),
		Cognitive:      NewDist(cog),
		MaxNesting:     NewDist(nest),
		FindingsByRule: byRule,
		FindingsTotal:  len(r.Findings),
	}
}
