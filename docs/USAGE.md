# Using assay

Three small tools that compose over JSON Lines. Nothing here needs a server, a
database, or an account.

```
ratchet   measure code, gate CI          scan · import · check · baseline · history
strata    keep the history               append · query · rollup · verdicts · stat
lens      make a stream readable         top · trend · diff · compare
```

---

## Install

```sh
go install github.com/sherzing/assay/cmd/ratchet@latest
go install github.com/sherzing/assay/cmd/strata@latest
go install github.com/sherzing/assay/cmd/lens@latest
```

One binary per tool — install only what you need. They share Go packages, but
the contract between them is the JSONL format, so you can replace any one of
them with your own implementation.

---

## Five minutes

```sh
# What does this repo look like?
ratchet scan .

# Record today as tolerated. Commit the file.
ratchet baseline .
git add .ratchet-baseline.json

# In CI: fail only if something got worse.
ratchet check .
```

That is the whole loop. Everything below is optional.

---

## Gating CI

`ratchet check` exits non-zero **only when a new finding appears.** Existing
violations are reported and tolerated.

This matters more than it sounds. Turn a linter on across a mature codebase and
you get four thousand violations, and the team switches it off that afternoon.
A ratchet lets the code improve or hold, never regress, without anyone stopping
to fix four thousand things first.

```yaml
# .github/workflows/quality.yml
- run: ratchet check . --strict-caps
```

| flag | effect |
|---|---|
| `--strict-caps` | also fail if peak complexity exceeds the baseline |
| `--rules a,b` | run only these rules |
| `--include-tests` | analyse `_test.go` too (off by default — test code has different norms) |
| `--json` | machine-readable result |

**The baseline cannot be silently regenerated.** `ratchet baseline` refuses to
overwrite an existing file; you need `--force` or `--tighten`. That is
deliberate — otherwise "fix the failing build" quietly becomes "rewrite the
baseline" and the gate stops meaning anything.

When you fix things, lock the improvement in:

```sh
ratchet baseline . --tighten     # drops only entries that no longer reproduce
```

---

## Any language, via SARIF

`ratchet` only parses Go. For everything else, use the language's own linter and
pipe its SARIF in — better fidelity than anything we would write, and no parsers
for us to maintain.

```sh
# Go
golangci-lint run --out-format sarif | ratchet import - --mode check

# C# — Roslyn analyzers. Put the ErrorLog in Directory.Build.props, not on the
# command line: a command-line property is a LITERAL, so $(MSBuildProjectName)
# would end up as a filename with brackets in it.
#
#   <ErrorLog>$(MSBuildThisFileDirectory)artifacts/$(MSBuildProjectName).sarif%2cversion=2.1</ErrorLog>
#
# Then, and each of these fails silently if you skip it:
#   %2c               a literal comma is eaten by MSBuild and you get SARIF 1.0,
#                     which is a different document shape entirely
#   per-project name  one shared path has each project overwrite the last
#   --no-incremental  an up-to-date build runs no analyzers and writes no SARIF,
#                     leaving the previous one in place to be imported again
#   artifacts/*.sarif import MERGES several files; passing one gives you a
#                     baseline covering one project and a green check over the rest
dotnet build --no-incremental -p:AnalysisMode=All
ratchet import artifacts/*.sarif --root . --mode baseline

# Dart — dart_code_linter (open source): its JSON needs a small shim to SARIF
dart_code_linter analyze lib --reporter=json | your-shim | ratchet import -

# Dart / Flutter — DCM (commercial): ratchet reads its metrics directly. DCM measures only
# what analysis_options.yaml configures, so add a dcm: metrics: block first
# (dcm init metrics-preview --format=analysis_options lib prints one) and run pub get
dcm run --metrics --report-all --no-fatal-found --reporter=json --output-to=dcm.json lib
ratchet import dcm.json --emit measures --repo myapp | strata append

# Anything semgrep covers
semgrep --config rules/ --sarif | ratchet import - --mode check
```

Three behaviours worth knowing:

- **Rule IDs are namespaced by tool.** Two linters both emitting `unused` will
  not collide in one baseline and silently excuse each other.
- **SARIF `suppressions` are honoured**, like `//nolint`. A tool that cannot be
  told no gets switched off entirely.
- **Fingerprints exclude line numbers.** A reformat must not read as a wave of
  new violations. The producer's own `partialFingerprints` are used when present.
- **Imported findings are first-class.** `.quality.yaml` verdicts apply to them,
  and `ratchet import … --emit findings|measures` writes the same records a scan
  does, so they reach `strata` and `docket`. `--repo`, `--commit` and `--ts`
  stamp the records; `--ts` lets a history backfill land on its commit's day.
  Findings carry `tool: sarif/<driver>`, and the counts are named per producer,
  `findings.<driver>.total`, so they never overwrite the scan's own
  `findings.total`; a clean run records its zero for every driver the document
  names. As with `scan`, an invalid `.quality.yaml` fails the import.

---

## Where rules live, and how to customise them

### Built-in Go rules

```sh
ratchet rules      # what it checks and why
```

| rule | severity |
|---|---|
| `naked-type-assertion` | error |
| `error-swallowed` | error |
| `any-in-exported-signature` | warn |
| `panic-in-library` | warn |
| `else-after-return` | info |

Turn them on and off individually:

```sh
ratchet scan . --rules naked-type-assertion,error-swallowed
```

**Every rule must be individually switchable.** Some will be wrong for your
codebase, and a rule you cannot disable is a rule that gets the whole tool
disabled.

### Judging a finding

Three verdicts, and the distinction is the point. A single "tolerated" bucket
conflates debt with noise.

| verdict | meaning | is it debt? | counts toward rule precision? |
|---|---|---|---|
| `accepted` | real, we carry it for now | **yes** | no |
| `false-positive` | the rule is wrong here | no | **yes** |
| `wont-fix` | real, deliberately not fixing | no | no |

All three stop `ratchet check` failing. They differ in what else happens:
`docket` refuses to ticket a false positive, and false-positive rate per rule is
what tells you which rules are noise.

**In code, for a specific instance:**

```go
// quality:false-positive this API is genuinely polymorphic by design
func Store(key string, value any) error

// quality:accepted until=2026-12-31 DEBT-412 — untangling needs the v2 migration
func (s *Service) recomputeOrderTotals(...)

// quality:wont-fix panics by design, this is a must* helper in all but name
func MustParse(s string) Config
```

The marker is **`quality:`, not `assay:`** — deliberately. A tool-branded prefix
says "this belongs to one vendor" and makes every other analyser ignore it. This
is meant to be a convention other tools can read, not a moat.

The reason sits next to the code it excuses, so a reviewer sees both in the same
diff. That is a far stronger review path than a hash in a JSON file.

**In project config, for a pattern:**

Some judgements are blanket, and annotating them would mean 97 comments.
`.quality.yaml` at the repo root:

```yaml
verdicts:
  - rule: any-in-exported-signature
    path: "**/generated/**"
    verdict: false-positive
    reason: generated code follows the generator's conventions, not ours
```

First match wins, read top to bottom. An in-code annotation beats config —
it is more specific, and someone wrote it while looking at that exact code.
Every entry **must** carry a reason: an unexplained exception is
indistinguishable from an oversight.

`//nolint` still works and still suppresses entirely.

### Review-by dates, and seeing what you have agreed to carry

`until=YYYY-MM-DD` makes an exception expire. Past the date, `ratchet` warns but
does not fail — nothing breaks, but the staleness is visible. **Removing the date
is itself the decision to make it permanent**, which is fine as long as someone
chose it.

```sh
ratchet exceptions .            # everything tolerated, and why
ratchet exceptions . --expired  # only the stale ones
```

```
verdict          review-by   location                       reason
────────────────────────────────────────────────────────────────────────────────
false-positive   permanent   pool.go:4                      genuinely polymorphic by design
accepted         2026-12-31  service.go:1289                DEBT-412 — needs the v2 migration
accepted         2026-01-31 ⚠ legacy.go:88                   DEBT-101 — meant to be done last quarter

2 accepted, 1 false-positive, 1 wont-fix, 5 unjudged

⚠  1 past their review-by date. Still passing — but nobody has looked.
```

The ratchet stops debt growing; nothing makes it shrink. This is the report that
stops a baseline quietly becoming permanent.

**Unjudged is counted separately from accepted, on purpose.** A finding nobody
has looked at is not evidence either way when computing rule precision.

### Your own rules

Custom rules are **semgrep or ast-grep rule packs** — we do not reinvent a rule
language. Three tiers, most specific wins:

```
rules/local/      this project only, lives in the repo being analysed
rules/org/        your company's pack
rules/core/       shipped with assay, has published precision data
```

A rule is a YAML file:

```yaml
# rules/org/myorg/no-direct-db-in-handler.yaml
rules:
  - id: no-direct-db-in-handler
    languages: [go]
    severity: ERROR
    message: HTTP handlers must not talk to the database directly
    paths:
      include: ["internal/httpapi/**"]
    pattern: $DB.Query(...)
```

```sh
semgrep --config rules/org --sarif | ratchet import - --mode check
```

> **Trap:** `semgrep scan` exits 0 *even with findings* unless you pass
> `--error`. Always assert the exit code, and test a new gate by planting a
> violation to confirm it actually fails.

### Writing rules that are worth having

AI will write you a syntactically valid rule in seconds. It cannot tell you
whether the rule is *right* for your codebase — that still takes judgement and
real code.

Three of assay's own five original rules turned out to be pure noise when first
run against a production service: variadic `...any` is the SQL-driver idiom, not
a smell; `panic` inside `must*` is the standard library's own convention. We only
found out by running them.

So: **write the rule, run it against a real repository, and read every finding
before you enable it.** If more than a few are wrong, fix the rule or drop it.

---

## Keeping history

```sh
ratchet scan . --emit measures --repo myservice | strata append
```

That is one point in time. For a trend, replay git history:

```sh
ratchet history . --interval week --first-parent --jobs 8 > history.jsonl
```

| flag | note |
|---|---|
| `--interval all\|day\|week\|month` | `all` measures every commit |
| `--first-parent` | **use this.** Walks the merge timeline — a feature-branch commit is work in progress, not a state the codebase was ever really in |
| `--jobs N` | checkout dominates the cost, so this scales nearly linearly |

Then query it:

```sh
strata stat
strata query --repo myservice --metric cognitive.p90 --since 2026-01-01 --format csv
strata rollup --period month --metric cognitive.p90
```

### What is on disk

```
.assay/
  measures/2026/09/19.jsonl      date-partitioned, append-only
  findings/2026/09/19.jsonl
  verdicts/verdicts.jsonl        append-only log, last write wins
  tickets/tickets.jsonl          append-only log of ticket events
  rollup/                        derived cache — safe to delete and rebuild
```

Raw data is the truth; rollups are a cache. Partitioning by date makes a range
query a directory listing, so there is no index to corrupt and no database to
run. Roughly 12,500 records is 3 MB.

Commit `.assay/` to git for a small repo, or sync it to object storage for an
org-wide history. The tools take a path and do not care which.

---

## Reading the results

`grep` and `jq` work — that is a deliberate property, not a substitute for
tooling. But for actually looking at something, use `lens`.

```sh
lens top --store .assay --n 10
```
```
worst 10 by cognitive (function scope)

     41 ██████████████████████  assay internal/analyze/analyze.go:Scan
     37 ███████████████████···  …helper.go:ValidationActionHelper.UpdateModelWithIncentiveMeta
     30 ████████████████······  …common/order_response_mapper.go:CreateTGOrderFailResponseModel
```

```sh
lens trend --store .assay --metric cplx.per_kloc --period month
```
```
service-c     ▅▇▇█▇▇▆▇▇▇▆▆▆▆▆▅▅▄▄▄▄▄▄▄▃▃▂▂▃▂▁▂▁▁▁▁▁▁▁▁▂▁▂▂▂▂▁▁▁▂▁▂▂▁▂▂▂▃▄▄▅▅▅  49.37 → 49.00  ▼ -1%
               2021-05-01 → 2026-07-01  (63 points)
```

The sparkline is the point: five years in one line, and the tail turning back up
is visible at a glance in a way no table makes obvious.

```sh
lens diff --store .assay --repo myservice --since 2026-01-01    # what moved
lens compare --store .assay --scope project                 # repos side by side
```

`lens` reads stdin too, so it works on any conforming stream:

```sh
ratchet scan . --emit measures | lens top --metric cognitive
strata query --repo myservice --metric cognitive | lens top --n 20
```

---

## Using external tools for more insight

assay deliberately does not reimplement things that already exist.

### Change coupling and hotspots — `scc`

The metric the research says actually predicts change- and error-proneness is
**co-change**, not structure. `scc` (MIT) does it across every language:

```sh
scc --coupling --depth 800              # files that change together
scc --coupling-for internal/order.go    # blast radius for one file
scc --by-file --cognitive --format json # complexity, any language
```

**Filter the output before believing it.** `degree = shared / union`, so two
files that each changed twice in the same two commits score 100%. Require at
least five shared commits, drop test↔subject pairs, and ignore pairs inside the
same module — those are cohesion, not shotgun surgery.

### Complexity for other languages

| language | tool | licence |
|---|---|---|
| many | `lizard` | MIT |
| Python | `wily` (per-commit history built in) | Apache-2.0 |
| Dart | `dart_code_linter` | MIT |
| Dart / Flutter | DCM, read natively by `ratchet import` | commercial, free tier |
| C# | Roslyn analyzers via `ErrorLog=*.sarif%2cversion=2.1` | MIT |

### Duplication

`jscpd` (MIT) or PMD CPD (BSD). Neither is built in.

### Dependency conformance

Declare your layering once, enforce it forever:

| language | tool |
|---|---|
| Go | `go-arch-lint`, or `depguard` inside golangci-lint |
| TS/JS | `dependency-cruiser` |
| Python | `tach`, `import-linter` |
| Java/Kotlin | ArchUnit — and use `FreezingArchRule`, which is the same ratchet idea |
| Dart | `import_rules` + `lakos` |

> Go forbids import cycles at the compiler, so a "no cycles" rule there is
> vacuous. Use a declared layering rule instead.

### Charts

Point Grafana at `.assay/rollup/` via CSV, or generate a static page from the
JSONL. There is deliberately no web UI here.

---

## Interpreting the numbers

### What is a normal value?

**Bands are percentiles of real code, not folklore.** The Go defaults come from
**3,053 functions across two production services** — one 5 years old, one
3 months old. The two distributions agree closely, which is what makes
them usable as a default:

| | p50 | p90 | p99 | max |
|---|---|---|---|---|
| cognitive | 1 | 5 | 15 | 37 |
| cyclomatic | 2 | 6 | 12 | 31 |
| nesting | 1 | 2 | 3 | 6 |

So "elevated" starts at p90 and "outlier" at p99 — a function above that line is
in the worst 1% of code we have measured, which is a defensible reason to send
someone to look at it.

**Calibrate against your own corpus instead.** One command, and strictly better
than anything shipped here:

```sh
lens calibrate --store .assay --lang go
```

It prints your percentiles and a ready-to-paste band definition. If p90 equals
p99, your corpus is too small or too uniform to calibrate from.

Languages without a calibrated set fall back to conventional, deliberately
looser thresholds — guessing tight is worse than guessing loose, because a noisy
band trains people to ignore the column.

| cognitive | meaning | what to do |
|---|---|---|
| < 5 | fine | nothing |
| 5–15 | readable | nothing |
| 15–25 | worth a look | read it; if you cannot hold it in your head, split it |
| > 25 | go fix it | extract the nested branches into named functions |

| cyclomatic | meaning | what to do |
|---|---|---|
| < 10 | fine | nothing |
| 10–20 | busy | check the tests cover each branch |
| > 20 | go fix it | too many paths to test honestly; decompose |

| nesting | meaning | what to do |
|---|---|---|
| ≤ 3 | fine | nothing |
| 4–5 | deep | invert conditions and return early |
| > 5 | go fix it | same, urgently |

`lens` applies these automatically and prints the verdict under each entry.

### Which number to read

**p50 and p90. Not max.**

- **p50 moved** → the typical function changed. This is the real signal.
- **p90 moved** → the difficult end of the codebase changed.
- **only max moved** → probably one function, or just sampling. Adding functions
  reaches further into the tail whether or not anything decayed.

A codebase with p50 = 3 and max = 40 has one bad function. A codebase with
p50 = 12 has a culture problem. Those need completely different responses, and
the max looks similar in both.

### Cognitive vs cyclomatic — they disagree on purpose

Cyclomatic counts independent paths. Cognitive models what a person has to hold
in their head: it penalises nesting, charges `else` a flat 1, and treats
`a && b && c` as one idea rather than three.

- **Flat twenty-case switch** → high cyclomatic, low cognitive. Many paths,
  nothing to remember. Usually fine.
- **Three nested ifs** → the reverse. Fewer paths, much harder to read.

**Send people to fix the cognitive ones.** Use cyclomatic to judge whether the
tests are honest.

### Diagnosing from the pair

| cyclomatic | nesting | the problem | the fix |
|---|---|---|---|
| high | low | too many branches | extract named functions |
| low | high | deep nesting | invert conditions, return early |
| high | high | both | decompose before anything else |

### Trends

**Watch the derivative, not the level.** Nobody agrees complexity 12 is bad.
Everybody agrees 12 → 40 in four months is bad.

**A reversal is the most actionable signal** — a long improving trend that turns
upward recently means something changed. `lens trend` flags these explicitly.

**Normalise for growth.** Raw finding counts rise simply because the codebase
grew. Use per-1,000-statements, or compare the *marginal* rate: findings added ÷
statements added over a window, against the codebase average. If the marginal
rate is below the average, the new code is cleaner than what was already there.

### Three ways to fool yourself

**Gating on an aggregate.** Any metric used as a gate on a probabilistic
optimiser gets gamed — gate on complexity and an agent will split functions
arbitrarily to satisfy it. Gate on new findings; keep aggregates advisory.

**Comparing across languages.** Branch-keyword density is a language trait: Go's
explicit `if err != nil` inflates counts against C#'s exceptions. Cross-language
comparison measures the language, not the team. Compare a repo to itself.

**Believing a rule you have not audited.** Published research found 63% of
detected "hard-severity" smells were expert-judged false positives. Three of
assay's own five original rules were noise against a real service. Read the
findings before you trust the count.

---

## Integrating external tools

### C# — Roslyn analyzers

Free, MIT, and usually absent. Most .NET repos have no analyzers enabled at all,
which is the actual gap — nothing is checking anything.

```xml
<!-- Directory.Build.props at the repo root -->
<Project>
  <PropertyGroup>
    <AnalysisLevel>latest</AnalysisLevel>
    <EnableNETAnalyzers>true</EnableNETAnalyzers>
    <AnalysisMode>Recommended</AnalysisMode>
    <!-- Per PROJECT. One shared filename has each project overwrite the last,
         so a solution-wide baseline would cover only whichever built last. -->
    <ErrorLog>$(MSBuildThisFileDirectory)artifacts/$(MSBuildProjectName).sarif%2cversion=2.1</ErrorLog>
  </PropertyGroup>
</Project>
```

```sh
dotnet build --no-incremental          # analyzers do not run on an up-to-date build
ratchet import artifacts/*.sarif --root . --mode baseline
ratchet import artifacts/*.sarif --root . --mode check
```

`%2cversion=2.1` is an escaped comma and is **required** — without it MSBuild
emits SARIF 1.0, which this importer does not read.

Add StyleCop or Roslynator as PackageReferences for more rules. Do **not** also
set `TreatWarningsAsErrors`: two gates fighting each other is how teams end up
disabling both.

### semgrep — custom rules, any language

```sh
semgrep --config rules/org --sarif > semgrep.sarif
ratchet import semgrep.sarif --root . --mode check
```

> `semgrep scan` exits **0 even with findings** unless you pass `--error`.
> `semgrep ci` fails by default. Test every new gate by planting a violation.

The OSS engine is single-file only; cross-file analysis is the paid tier. That is
fine for local patterns (casts, swallowed errors, banned imports) and useless for
anything needing call-graph reasoning.

### Go — golangci-lint

```sh
golangci-lint run --out-format sarif | ratchet import - --mode check
```

Use alongside `ratchet scan`, not instead of it — different rules, no overlap.

### Dart — dart_code_linter

```yaml
dev_dependencies:
  dart_code_linter: ^4.4.0
```

```sh
dart run dart_code_linter:metrics analyze lib --reporter=json > dcl.json
```

No SARIF reporter, so you own a small converter (~40 lines mapping
`records[].issues[]` to SARIF `results[]`). Add `lakos` for cycle detection.

### Dart and Flutter — DCM

[DCM](https://dcm.dev) is the commercial successor of the tool above, and its
metrics are the reason to run it: cyclomatic complexity, nesting, widget nesting
and widgets per build method, class cohesion and coupling. It has no SARIF
reporter, and SARIF would drop the metrics anyway, so `ratchet import` reads
DCM's own JSON. The format is sniffed from the document; `--format dcm` forces
it.

DCM measures only what `analysis_options.yaml` configures. With no `dcm:
metrics:` block, `metricResults` is empty and the import fails rather than
storing zeros, so the block comes first. Let DCM write it from what it sees in
your code, or start from a few metrics:

```sh
dcm init metrics-preview --format=analysis_options lib   # prints every metric, with thresholds
```

```yaml
# analysis_options.yaml
dcm:
  metrics:
    cyclomatic-complexity: 20
    maximum-nesting-level: 5
    number-of-parameters: 4
    source-lines-of-code: 50
```

```sh
flutter pub get   # DCM does not resolve dependencies itself
dcm run --metrics --report-all --no-fatal-found --reporter=json --output-to=dcm.json lib
ratchet import dcm.json --emit measures --repo myapp | strata append
lens top --store .assay --metric cyclomatic --n 10
lens calibrate --store .assay --lang dart      # bands from your own corpus; pasted under dart:, they apply to .dart paths
```

`--report-all` makes DCM report every metric value rather than only threshold
breaches; without it the distributions are meaningless. `--no-fatal-found`
stops DCM failing the build by itself. Without `pub get` the syntactic metrics
are still right, but widget metrics disappear rather than read zero and
`depth-of-inheritance-tree` resets at every `package:` import, and nothing
downstream can tell that from a genuine zero.

**What each DCM plan gives you.** DCM is commercial, and the plan decides
which `dcm run` flags produce anything. The importer does not care: a section
your plan does not emit simply imports nothing. As of late 2026, per
[dcm.dev/pricing](https://dcm.dev/pricing/):

| plan | what it adds for this pipeline |
|---|---|
| Free — one seat, no account or card | `--metrics` with 22 metrics, capped at 50k analysed lines; `--analyze` with a fixed set of ~100 rules; `--unused-files`. No rule configuration, no presets, no CI key. |
| Pro — per seat | the full rule set with configuration and presets, `--unused-code`, `--code-duplication`, widgets and assets analysis |
| Teams and up | unlimited lines, dashboards, and the `--ci-key` that running in CI requires |

Two operational details that cost us an afternoon: the Free plan still needs
`dcm activate --license-key=…`, and an expired paid licence left on a machine
blocks every command, free ones included, until another key is activated.

**What lands in the store.** Measures use the Go scan's names, `cyclomatic`,
`nesting`, `params`, `sloc`, plus DCM's own such as `widgets.nesting` and
`cohesion`, at function, file and `class` scope, where a class is any class,
mixin, extension or enum. `ratchet` derives the project series, `cyclomatic.p90`
and the rest, the way it does for a scan, so one `lens trend` query serves a
Dart repo and a Go repo alike. Declarations keep the names DCM gives them, with
the collisions resolved: a setter is `Box.value=`, a local function is
`Box.work.inner`, an unnamed extension's method is `Unnamed.triple`. The
declarations DCM computed cyclomatic complexity for also fill the per-function
records, so `ratchet import dcm.json --json` reads like a native scan and
`--mode check --strict-caps` holds peak cyclomatic complexity and nesting; a
bodiless declaration has no complexity and gets no record. Cognitive complexity
stays 0: DCM has no such metric. Generated code is skipped, as the Go scan
skips it: `.g.dart`, `.freezed.dart`, `.pb.dart` and friends, `generated/`
directories, and any file whose header says a tool wrote it.

Three things to know before trusting a number:

- `maintainability`, `cohesion` and `weight` are higher-is-better. `lens` ranks
  and judges every metric as higher-is-worse, so on these `lens top` lists the
  best code first and a falling trend reads as improving.
- DCM writes paths relative to the directory it ran in. Run it from the
  repository root, or import every package's report in one invocation as
  `report=prefix` pairs, `ratchet import packages/app/dcm.json=packages/app
  packages/lib/dcm.json=packages/lib --root .`, so the declarations key like the
  rest of the repository and the project series is rolled up once; `--prefix`
  is the default for a report without one. Two imports under one `--repo` would
  each write their own series. Two reports disagreeing about one declaration is
  an error, not a second value, and an import none of whose files is under
  `--root` says so: that is the forgotten prefix.
- Lint findings (`--analyze`, unused code, duplication) are not imported yet;
  the import says how many it skipped. `--mode check` on DCM input needs
  `--strict-caps`, because nothing else could fail.

Backfilling history costs a `pub get` per sampled commit: sample monthly, and
pass `--ts` and `--commit` so each sample lands on its own day in the store.
Backfill with `--detail=false`, which keeps the project series and drops the
per-declaration rows, as `ratchet history` does: a store full of functions that
no longer exist misleads `lens top` and `lens diff`. `lakos` still covers cycle
detection.

### Anything else

If it emits SARIF, `ratchet import` reads it: CodeQL, Trivy, ESLint
(`-f @microsoft/eslint-formatter-sarif`), PMD, Bandit.

---

## Turning findings into work

```sh
ratchet scan . --emit findings --repo myservice > findings.jsonl

docket plan   < findings.jsonl                       # what would be created
docket create < findings.jsonl --provider file --yes # markdown, no tracker
docket create < findings.jsonl --provider linear --team ENG --label tech-debt --yes
docket sync   < findings.jsonl --provider linear --yes   # progress, close what is done
docket status
```

### Group by theme, not by finding

`--group-by theme` (the default) collapses a repetitive backlog into something a
team will actually clear. On a real 5-year service: **208 findings → 5 tickets**,
because 97 of them were the same rule and 90 were another.

`--group-by file` when several different rules fire in one place. `--group-by
finding` exists and is almost always wrong.

### How you know what is still open

Each ticket stores its cohort of fingerprints. `docket sync` re-scans and does
set arithmetic:

```
T-002   Fix 90 × naked-type-assertion    47/90
T-004   Fix 3 × panic-in-library          3/3  ✓ ready to close
```

Better than one ticket per finding, because you get progress rather than a
binary — and the fingerprints already survive reformatting, so the count is
honest.

**The cohort is fixed at creation.** A ticket for "all X" would normally never
close; `ratchet check` blocks new instances from landing, so it has a finish
line. Later findings of the same theme are *not* absorbed.

### Safety

| | |
|---|---|
| `--yes` | required to touch a tracker. Without it, preview only |
| `--max N` | caps a run (default 20) |
| `--min-size N` | skip cohorts smaller than N |
| `--provider file` | markdown on disk, no network, no token |

It **refuses to ticket a `false-positive`** — that is noise reaching a human,
which is the failure this project exists to prevent. Fix the rule instead; the
plan names which rule produced them. It also never re-files a finding whose
ticket someone closed, because closing it was a decision.

### Letting agents do the fixing

The ticket body lists every location and ends with the verification command. An
agent can work a whole cohort, then gate itself:

```sh
ratchet check .    # before opening the PR
```

That matters: measured failure rates for automated remediation are real — 10.5%
of automated cycle fixes create new cycles, and the most aggressive repair agent
in one 2026 study introduced 140 new smells while fixing others. Running
`ratchet check` before the PR means a fix that makes something else worse never
reaches a reviewer.

**Coverage decides whether review can be automated.** For a refactor the
acceptance criterion is "behaviour did not change", which tests can verify — but
only where coverage exists, and hotspot functions are typically the worst
covered. If coverage is thin, the first ticket should be "write characterisation
tests", which is a safer agent task and makes the refactor reviewable afterwards.

## For agents

`docs/agent-guide.md` is written for an AI agent to read before helping someone
set this up. It covers the decision tree, what to explain, verification, and the
mistakes to steer people away from.

```
Help me understand and set up assay. Read docs/agent-guide.md first,
then walk me through it step by step.
```
