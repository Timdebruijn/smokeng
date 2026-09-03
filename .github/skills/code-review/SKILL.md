---
name: code-review
description: How to review a change to smokeng. Use for every pull request in this repository. Explains the one premise the project is built on, the defect classes that actually recur here, and how to tell a test that checks something from a test that agrees with itself.
---

smokeng keeps the full round-trip-time distribution per interval, forever, at
full resolution, and draws it honestly. Everything below follows from that.

## The premise, and what it makes a bug

**A measurement must never claim to know more than it does.** A visible gap is
always better than a fabricated value. Judge every change against this before
anything else, because it turns things that look cosmetic into defects:

- A number recorded that was not measured. Real example: a server timestamping
  at its midpoint made `ServerProcessingTime()` return a computable `0` instead
  of "unavailable", so a distribution of fabricated zeros was stored for ever
  under a heading that said how long the far end held each packet.
- Loss reported that did not happen. Real example: probes a session never
  scheduled were counted as attempted and lost, so an interval read 19 of 20
  with a quality flag when nothing had failed.
- Absence rendered as a value, or a value rendered as absence. "Not measured",
  "measured and there was nothing to compare", and "measured, and it was zero"
  are three different facts. Watch for any layer that collapses them: a `nil`
  turning into an empty slice, an empty list turning into a null column, a
  `len(x) == 0` standing in for "absent".
- Data whose provenance is weakened but unflagged. Userspace timestamps, a
  clock step, a truncated interval and dropped replies each already have a
  flag. A new way for a number to be less trustworthy needs one too — and the
  flags byte is **full**, so a change that needs a ninth bit has to widen it in
  the store *and* both wire formats, which is a much larger change than it
  looks.

## Where the same mistakes keep being made

**Comments and commit messages that describe something the code does not do.**
This is by far the most common real finding here, and it matters because the
tests get written from the comment. Seen in one week: a commit that credited a
`recover()` for catching a panic that a library already caught one frame
earlier; a doc comment promising a series is "absent, never present-and-empty"
on the same branch that deliberately made it present-and-empty; operations
documentation stating that an older master refuses an unknown column when it
ignores it. When a comment makes a factual claim about behaviour, check the
behaviour, not the grammar.

**Tests that agree with themselves.** A test that restates a production
constant and then asserts the restatement passes whatever the constant becomes.
A test that hands a value straight to the function under test, bypassing the
selection logic that is the actual change. A test whose fixture is built so the
condition it guards is zero by construction. The way to tell is to break the
code and see whether the test notices — if you can describe a one-line mutation
that would keep it green, say so and name the mutation.

**Absence as an instruction.** The TOML importer is declarative: a target,
setting or alert rule missing from the file is *disabled*, not ignored. So a
setting reachable from the API or the UI but not writable in TOML gets reverted
by the next Ansible run, silently. Any new inheritable setting must be plumbed
through `tree.Settings`, `tree.Resolved`, the root-completeness check,
`config.Values`, `valuesFrom`, `overlayValues`, the SQLite column, scan and
upsert, and the API's JSON in both directions. Use `retention_s` as the
reference and diff the two lists.

**Both wire formats, and both directions.** Measurements cross agent→master
(`internal/ingest`) and master→browser (`internal/api/measurements.go`). A
column added to either must be **optional**: a master resolves columns by name
and ignores one it does not know, and every column so far is nullable, so the
two halves may be upgraded in either order. Making a column required breaks
that, and an agent discards a batch the master calls undecodable — so the cost
is the buffered measurements, not a delay. Also check Arrow builders indexed by
*position*: inserting a column silently shifts every later one, and the failure
surfaces as an interface conversion several frames away.

**Anything that can stop an agent delivering for ever.** An agent keeps a
rejected batch and retries it unchanged. Any new way for the master to refuse a
whole batch — a stricter schema, a validation that fails the transaction — is a
permanent outage for that agent, not a hiccup. This has been introduced three
separate times: prefer normalising or skipping the offending row over failing
the write.

## How to check, not guess

- Read the vendored library rather than assuming its behaviour. Two real bugs
  came from `github.com/heistp/irtt` doing something its function names did not
  suggest: `NewDefaultCompTimer()` returns a fresh timer around a *shared*
  averager, and the client's sender is bounded by a duration rather than a
  packet count.
- Derive arithmetic from the source it models. A margin was wrong because it
  assumed a skipped scheduling slot cost half an interval when it costs a whole
  one.
- Say which findings you verified and which you inferred. A confident wrong
  finding costs more than a hedged right one.

## Do not report these

- `make([]T, 0, n)` where `n` is any integer type. The Go spec allows the size
  arguments to `make` to be of any integer type, not only `int`. This has been
  reported as a compile error on code that builds and passes CI.
- Committed files under `web/dist/`. They are the embedded frontend build and
  are meant to be in the tree; a change there without a matching change under
  `web/src/` is worth a note, the reverse is not.
- Dutch in comments or commit messages. There is none, and the repository's
  convention is English for everything in the tree.
