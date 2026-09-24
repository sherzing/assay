# assay — agent guide

**You are an AI agent reading this to help a human set up assay.** Read it fully
before acting, then walk them through step by step. Do not dump this document at
them; use it.

---

## What assay is, in one paragraph you can say out loud

assay measures code quality and stops it getting worse. It is three small
command-line tools that pass JSON Lines between them: `ratchet` measures and
gates CI, `strata` keeps the history, `lens` makes it readable. There is no
server, no database, and no account. The distinctive idea is the **ratchet**:
instead of demanding a clean codebase, it records what is wrong today and fails
the build only when something *new* appears.

## What it is not

Say this early — it prevents an hour of wrong expectations.

- **Not a replacement for your linter.** It consumes your linter's output.
- **Not a security scanner.**
- **Not a dashboard.** `lens` is a terminal tool. Charts mean Grafana or a static
  page from the JSONL.
- **Not multi-language on its own.** Native analysis is Go only; every other
  language arrives via SARIF from that language's own tooling.

---

## Decide before you type

Ask the user these three things first. The answers change everything.

**1. What language is the codebase?**

| | path |
|---|---|
| Go | `ratchet scan` works natively. Start there. |
| Dart / Flutter | DCM metrics via `ratchet import`; lint findings via `dart_code_linter` and a SARIF shim — see "Dart and Flutter" |
| C#, TS, Python, anything | you need a SARIF producer first — see "Other languages" |

**2. What do they actually want?**

| goal | do this | skip |
|---|---|---|
| stop quality regressing | `ratchet` only | strata, lens |
| understand a codebase they inherited | `ratchet scan` + `lens top` | strata |
| prove a trend over time | all three, plus `ratchet history` | — |

**Most people only need the first.** Do not set up storage because it exists.

**3. Is the codebase old or new?**

- **New**: baseline will be near zero. Set the gate strict from day one.
- **Old**: expect hundreds of findings. That is normal and it is exactly what the
  ratchet is for. Reassure them — they are not expected to fix any of it.

---

## Step 1 — install and look

```sh
go install github.com/sherzing/assay/cmd/ratchet@latest
ratchet scan .
```

Read the output *with* them. It looks like this:

```
scanned 11 files, 119 functions, 1433 statements

metric          p50    p90    max     mean
cyclomatic        4     11     17     5.18
cognitive         3     14     27     5.08
nesting           1      3      3     1.38

3 findings:
  error-swallowed              2
  any-in-exported-signature    1
```

**How to explain it:**

- **p50 is the typical function**, p90 is the difficult end, max is the single
  worst. Read p50 and p90. Max is usually one outlier.
- **Cognitive ≠ cyclomatic.** Cyclomatic counts branches; cognitive models what a
  person must hold in their head — it penalises nesting and charges `else` a flat
  1. A flat twenty-case switch is high cyclomatic, low cognitive. Point people at
  the cognitive ones.
- **Rough bands for cognitive:** under 5 fine, under 15 readable, 15–25 worth a
  look, over 25 go fix it. Say these are conventions, not laws.

If p50 is low and only max is high, tell them the codebase is fine and there is
one bad function. That is a very different conversation from a high p50.

## Step 2 — the ratchet

```sh
ratchet baseline .
git add .ratchet-baseline.json && git commit -m "chore: assay baseline"
```

**Explain why the file is committed:** it is the record of what the team agreed
to tolerate. In review, a change to it is visible. That is the point.

Then prove it works, in front of them — this is the moment the tool clicks:

```sh
ratchet check .          # passes
# add a deliberate violation, e.g. a naked type assertion
ratchet check .          # fails, naming the new finding and a suggested fix
```

Wire it up:

```yaml
- run: ratchet check .
```

**Warn them about one thing:** `ratchet baseline` refuses to overwrite an
existing baseline. That is deliberate. If someone hits the error, the answer is
`--tighten` (drop only what is genuinely fixed), never `--force` on a red build.

## Step 3 — other languages, via SARIF

Only if they are not on Go.

### C#

Roslyn analyzers are free and MIT, and most .NET repos have none enabled. That is
usually the real gap — nothing is checking anything.

```xml
<!-- Directory.Build.props at the repo root -->
<Project>
  <PropertyGroup>
    <AnalysisLevel>latest</AnalysisLevel>
    <EnableNETAnalyzers>true</EnableNETAnalyzers>
    <AnalysisMode>Recommended</AnalysisMode>
    <ErrorLog>$(MSBuildThisFileDirectory)artifacts/roslyn.sarif%2cversion=2.1</ErrorLog>
  </PropertyGroup>
</Project>
```

```sh
dotnet build
ratchet import artifacts/roslyn.sarif --root . --mode baseline
ratchet import artifacts/roslyn.sarif --root . --mode check
```

Note the `%2cversion=2.1` — that is an escaped comma and it is required to get
SARIF 2.1 rather than 1.0. Without it the import will be wrong.

Do **not** set `TreatWarningsAsErrors` as well. The ratchet is the gate; two
gates fighting each other is how people end up disabling both.

### Dart and Flutter

Two tools, two jobs. For metrics, use [DCM](https://dcm.dev): commercial, the
CI key is a Teams feature, and it does not resolve dependencies, so `pub get`
comes first.

```sh
dcm init metrics-preview --format=analysis_options lib   # once: prints a dcm: metrics: block for analysis_options.yaml
flutter pub get
dcm run --metrics --report-all --no-fatal-found --reporter=json --output-to=dcm.json lib
ratchet import dcm.json --emit measures --repo myapp | strata append
lens top --store .assay --metric cyclomatic --n 10
```

DCM measures only what `analysis_options.yaml` configures: without the `dcm:
metrics:` block the report is empty, and the import fails rather than storing
zeros. `ratchet import` reads DCM's JSON directly; there is no converter to
own. Say `--report-all` out loud, or the metrics are only the breaches, and
`--no-fatal-found`, or DCM fails the build before ratchet gets a say. Lint
findings are not imported yet, so a DCM-only gate is `--mode check
--strict-caps`, which holds peak complexity.

For lint findings and the gate, the open-source `dart_code_linter` still works:

```yaml
# pubspec.yaml
dev_dependencies:
  dart_code_linter: ^4.4.0
```

```sh
dart run dart_code_linter:metrics analyze lib --reporter=json > dcl.json
# dart_code_linter has no SARIF reporter; convert its JSON, then:
ratchet import dcl.sarif --root . --mode check
```

Tell them honestly: the converter is a small script they will own. Roughly 40
lines mapping `records[].issues[]` to SARIF `results[]`.

### Anything semgrep supports

```sh
semgrep --config p/default --sarif > semgrep.sarif
ratchet import semgrep.sarif --root . --mode check
```

**The trap to warn about:** `semgrep scan` exits 0 even when it finds things,
unless you pass `--error`. People ship gates that never fire. Always verify by
planting a violation.

### Go, with golangci-lint

```sh
golangci-lint run --out-format sarif | ratchet import - --mode check
```

Use this *in addition to* `ratchet scan` — different rules, no overlap.

## Step 4 — history (only if they want a trend)

```sh
ratchet history . --interval week --first-parent --jobs 8 > history.jsonl
ratchet scan . --emit measures --repo myservice | strata append
```

**Always pass `--first-parent`.** It walks the merge timeline. Without it you
measure feature-branch commits, which are work in progress, not states the
codebase was ever really in — and the series gets spurious jumps.

Then:

```sh
lens top --store .assay --n 10
lens trend --store .assay --metric cognitive.p90 --period month
lens diff --store .assay --since 2026-01-01
```

`lens` prints an interpretation line under each result. Read it to them; that is
what it is for.

---

## What to do with what you find

This is where people stall. Be concrete.

**A high `lens top` entry** → open that function. The `↳` line says whether the
problem is branching or nesting, because the fix differs: many branches means
extract named functions; deep nesting means invert conditions and return early.
Fix one, run `ratchet baseline --tighten`, and the gain is locked in.

**A `worsening` trend** → look at what changed in that window. A rising p90 means
the typical difficult function got harder, which is real. A rising max only means
one function got worse, or the codebase grew.

**A `REVERSAL` verdict** → the most actionable output assay produces. Something
changed recently that undid a long improvement. Worth a conversation, not just a
ticket.

**Hundreds of findings on an old codebase** → correct and expected. Baseline
them. Do not start a cleanup project.

---

## Mistakes to steer them away from

- **Gating on an aggregate score.** Any metric used as a gate on a probabilistic
  optimiser gets gamed — gate on complexity and an agent will split functions
  arbitrarily to satisfy it. Gate on *new findings*; keep aggregates advisory.
- **Comparing complexity across languages.** Branch-keyword density is a language
  trait. Go's `if err != nil` inflates counts against C#'s exceptions. Compare a
  repo to itself over time.
- **Enabling a rule without reading its findings.** Three of assay's own five
  original rules were noise against a real service. Run a new rule, read every
  hit, then enable it.
- **Regenerating the baseline to fix a red build.** That is how the gate stops
  meaning anything. `--tighten`, never `--force`.
- **Treating max as the headline.** It is the least informative number on the
  page.

---

## Verify the setup worked

Run these with them. If all four pass, they are done.

```sh
ratchet check .                       # exits 0 on a clean tree
# plant a violation, then:
ratchet check .                       # exits 1 and names it
git show HEAD --stat | grep ratchet   # the baseline is committed
ratchet scan . --top 5                # they can read the output unaided
```

The last one is the real test. If they cannot explain their own p90 back to you,
go over Step 1 again — an unread metric is worse than no metric, because it
carries false confidence.

---

## Reference

| | |
|---|---|
| Full usage | `docs/USAGE.md` |
| Rule authoring and tiers | `docs/USAGE.md`, "Where rules live" |
| `ratchet rules` | what each built-in rule catches and why |
| `<tool> help` | every command has usage |

Everything on disk is JSONL. If a tool is in the way, `grep` and `jq` will
answer the same questions — that is deliberate, and worth telling them, because
it means adopting assay does not trap their data.
