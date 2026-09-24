package model

import (
	"math"
	"testing"
)

// The percentile method is the most consequential twelve lines in the project:
// every p90 quoted in every trend report comes out of pct(). Changing the
// method would silently invalidate comparison against every number recorded
// before the change, so these tests pin the EXACT definition rather than a
// vague "is it about right".
//
// The definition is nearest-rank, lower, zero-indexed:
//
//	index = trunc(p × (n − 1))
//
// which is the same as numpy's `interpolation="lower"` and differs from the
// interpolating default of numpy, Excel and most spreadsheets. Quoting a p90
// from here against a p90 from a spreadsheet is not a like-for-like comparison.

func TestPercentileDefinitionIsPinned(t *testing.T) {
	// 1..10, so the expected index is readable directly off the value.
	ten := []int{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}
	d := NewDist(ten)
	if d.P50 != 5 {
		t.Errorf("P50 = %d, want 5 (index trunc(0.5×9)=4)", d.P50)
	}
	if d.P90 != 9 {
		t.Errorf("P90 = %d, want 9 (index trunc(0.9×9)=8)", d.P90)
	}
	if d.Max != 10 {
		t.Errorf("Max = %d, want 10", d.Max)
	}
}

func TestDistIsIndependentOfInputOrder(t *testing.T) {
	asc := NewDist([]int{1, 2, 3, 4, 5, 6, 7, 8, 9, 10})
	desc := NewDist([]int{10, 9, 8, 7, 6, 5, 4, 3, 2, 1})
	shuf := NewDist([]int{7, 2, 10, 4, 1, 9, 3, 8, 5, 6})
	if asc != desc || asc != shuf {
		t.Errorf("distribution depends on input order:\n asc=%+v\ndesc=%+v\nshuf=%+v", asc, desc, shuf)
	}
}

// NewDist must not reorder the caller's slice. The scan reuses those slices,
// and a sort in place would scramble the per-function records that the
// hotspot report reads.
func TestNewDistDoesNotMutateItsInput(t *testing.T) {
	in := []int{5, 1, 4, 2, 3}
	before := append([]int(nil), in...)
	NewDist(in)
	for i := range in {
		if in[i] != before[i] {
			t.Fatalf("input was sorted in place: got %v, want %v", in, before)
		}
	}
}

// KNOWN LIMITATION, pinned deliberately.
//
// With the lower method, p90 of a two-element sample is the SMALLER value:
// trunc(0.9 × 1) = 0. The p90 of a tiny sample understates, and it converges to
// the true p90 only as n grows.
//
// This is fine for a service with thousands of functions and misleading for a
// module with five. It is why p90 figures from very small repositories should
// not be compared against large ones — a real caveat that bit a cross-repo
// comparison of sub-1k-line codebases.
//
// Do not "fix" this without migrating every stored measure: the numbers would
// stop being comparable with everything recorded to date.
func TestSmallSamplesUnderstateP90(t *testing.T) {
	if got := NewDist([]int{1, 100}).P90; got != 1 {
		t.Errorf("P90 of [1 100] = %d, want 1 — the lower method on n=2 picks index 0", got)
	}
	if got := NewDist([]int{1, 2, 3}).P90; got != 2 {
		t.Errorf("P90 of [1 2 3] = %d, want 2 — index trunc(0.9×2)=1, i.e. the median", got)
	}
	// It takes 11 samples before p90 reaches the top value at all.
	if got := NewDist([]int{1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 100}).P90; got != 1 {
		t.Errorf("P90 = %d, want 1: one outlier in eleven does not move the lower p90", got)
	}
}

// Truncating a float product invites an off-by-one when p×(n−1) lands a hair
// below a whole number. Sweep a wide range of sizes and assert the index is
// always the mathematically intended one.
func TestPercentileHasNoFloatingPointOffByOne(t *testing.T) {
	for n := 1; n <= 500; n++ {
		vals := make([]int, n)
		for i := range vals {
			vals[i] = i // value == index, so the result names its own index
		}
		d := NewDist(vals)

		wantP50 := int(math.Trunc(0.50 * float64(n-1)))
		wantP90 := int(math.Trunc(0.90 * float64(n-1)))
		if d.P50 != wantP50 {
			t.Errorf("n=%d: P50 index = %d, want %d", n, d.P50, wantP50)
		}
		if d.P90 != wantP90 {
			t.Errorf("n=%d: P90 index = %d, want %d", n, d.P90, wantP90)
		}
		if d.P50 > d.P90 || d.P90 > d.Max {
			t.Errorf("n=%d: percentiles not monotonic: p50=%d p90=%d max=%d", n, d.P50, d.P90, d.Max)
		}
	}
}

func TestEmptyDistIsZeroNotNaN(t *testing.T) {
	d := NewDist(nil)
	if d != (Dist{}) {
		t.Errorf("NewDist(nil) = %+v, want the zero value", d)
	}
	if math.IsNaN(d.Mean) {
		t.Error("Mean is NaN: 0/0 escaped, and NaN serialises to invalid JSON")
	}
}

func TestSingleValueDist(t *testing.T) {
	d := NewDist([]int{7})
	if d.P50 != 7 || d.P90 != 7 || d.Max != 7 || d.Mean != 7 || d.Sum != 7 {
		t.Errorf("NewDist([7]) = %+v, want every statistic to be 7", d)
	}
}

func TestMeanAndSum(t *testing.T) {
	d := NewDist([]int{1, 2, 3, 4})
	if d.Sum != 10 {
		t.Errorf("Sum = %d, want 10", d.Sum)
	}
	if d.Mean != 2.5 {
		t.Errorf("Mean = %v, want 2.5", d.Mean)
	}
}

// All-identical values are common in practice — a package of trivial getters —
// and must not produce a degenerate distribution.
func TestAllIdenticalValues(t *testing.T) {
	d := NewDist([]int{3, 3, 3, 3, 3})
	if d.P50 != 3 || d.P90 != 3 || d.Max != 3 || d.Mean != 3 {
		t.Errorf("got %+v, want every statistic to be 3", d)
	}
}

func TestSummariseCountsAndBuckets(t *testing.T) {
	r := &Report{
		Funcs: []FuncMetrics{
			{Cyclomatic: 1, Cognitive: 0, MaxNesting: 1, Statements: 5},
			{Cyclomatic: 9, Cognitive: 12, MaxNesting: 4, Statements: 40},
		},
		Findings: []Finding{
			{Rule: "a"}, {Rule: "a"}, {Rule: "b"},
		},
	}
	r.Summarise(7)

	if r.Summary.Files != 7 {
		t.Errorf("Files = %d, want the 7 passed in", r.Summary.Files)
	}
	if r.Summary.Funcs != 2 {
		t.Errorf("Funcs = %d, want 2", r.Summary.Funcs)
	}
	if r.Summary.Statements != 45 {
		t.Errorf("Statements = %d, want 45", r.Summary.Statements)
	}
	if r.Summary.FindingsTotal != 3 {
		t.Errorf("FindingsTotal = %d, want 3", r.Summary.FindingsTotal)
	}
	if r.Summary.FindingsByRule["a"] != 2 || r.Summary.FindingsByRule["b"] != 1 {
		t.Errorf("FindingsByRule = %v, want a:2 b:1", r.Summary.FindingsByRule)
	}
	if r.Summary.Cyclomatic.Max != 9 || r.Summary.Cognitive.Max != 12 {
		t.Errorf("distributions not built: %+v", r.Summary)
	}
	if r.SchemaVersion != SchemaVersion {
		t.Errorf("SchemaVersion = %q, want %q", r.SchemaVersion, SchemaVersion)
	}
}

// Cognitive and cyclomatic complexity measure different things — Campbell's
// model penalises nesting and charges a flat 1 for `else`. A report where they
// are always equal means one of them is not being computed.
func TestSummariseKeepsTheTwoComplexitiesSeparate(t *testing.T) {
	r := &Report{Funcs: []FuncMetrics{
		{Cyclomatic: 4, Cognitive: 9, MaxNesting: 3},
		{Cyclomatic: 6, Cognitive: 2, MaxNesting: 1},
	}}
	r.Summarise(1)
	if r.Summary.Cyclomatic == r.Summary.Cognitive {
		t.Error("cyclomatic and cognitive distributions are identical; they measure different things")
	}
}

// A repository with no Go in it at all must summarise to zeroes rather than
// panicking — history replay hits plenty of early commits like this.
func TestSummariseOnAnEmptyReport(t *testing.T) {
	r := &Report{}
	r.Summarise(0)
	if r.Summary.Funcs != 0 || r.Summary.FindingsTotal != 0 {
		t.Errorf("empty report summarised to %+v", r.Summary)
	}
	if r.Summary.Cyclomatic != (Dist{}) {
		t.Errorf("Cyclomatic = %+v, want the zero value", r.Summary.Cyclomatic)
	}
}

func TestFloatDistMatchesDistOnIntegers(t *testing.T) {
	ints := []int{7, 1, 4, 9, 2, 8, 3}
	floats := make([]float64, len(ints))
	for i, v := range ints {
		floats[i] = float64(v)
	}
	d, f := NewDist(ints), NewFloatDist(floats)
	if float64(d.P50) != f.P50 || float64(d.P90) != f.P90 || float64(d.Max) != f.Max || d.Mean != f.Mean {
		t.Errorf("Dist %+v and FloatDist %+v disagree on the same values", d, f)
	}
	if got := NewFloatDist(nil); got != (FloatDist{}) {
		t.Errorf("empty = %+v, want zero", got)
	}
	if got := NewFloatDist([]float64{0.98, 0.5}); got.Max != 0.98 || got.P50 != 0.5 {
		t.Errorf("fractional values = %+v", got)
	}
}
