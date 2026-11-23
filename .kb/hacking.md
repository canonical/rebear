# Preface

How to hack on this repository: the rules that keep the codebase
consistent while changing it.

IMPORTANT: Read the `.kb/agents.md` file.


# Laws

## §1 - Writing and updating code comments

**Keep code comments short and worthwhile.** A comment states the
rule or the trap at that site, in one to three lines, and
exceptionally more if really important.

**Do not write comments that just read the code in prose.** The code
speaks for itself and must be clear and simple by design. Only add
comments when adding value.

**Do not write comments out of place.** Document behavior of the
logic that is seen at the commented site, not of functions and logic
living elsewhere.

**Write simple and clear grammar.** Avoid unusual constructions.

**Use `-` if you must, never `—`.**

## §2 - Naming inlets, outlets, and midlets

Do not name inlet, outlet, and midlet variables `in`, `out`, and
`mid`: the proper terms `inlet`, `outlet`, and `midlet` are short
and clear already. The same holds for `up` as short for `upstream`
- say `remote` and the word in full.

## §3 - Test code indentation

No code inside a test function should ever sit at column 1 - indent
it appropriately, and leverage the `reindent()` function and pattern
if necessary.

## §4 - Compact slice representation

Use the compact form for slice literals, in general code and in
tests alike: the first element's brace hugs the type, and consecutive
elements close and open back to back, as in:
```
var testCases = []testType{{
    Field1: value,
}, {
    Field2: value,
}}
```
Depending on the complexity of the layout, a closing brace may
sit alone in a line to actually respect the compact form.

Very short structs, listed consistently with the same fields,
may stay inline as single-element lines - that is more compact
still, with no junctions between elements:
```
testCases := []struct {
    body string
    kind snippets.Kind
}{
    {"", snippets.NoKind},
    {"null", snippets.NullKind},
}
```

## §5 - Test tables before the loop

Never build the table inside the `for` statement. Always create the
table first as a named variable, and then spell out the loop. The
table variable is named `testCases` and the loop variable `tc`.

## §6 - Log message formatting

Log messages land in double-quoted strings: every string field of a
record in the JSONL log line, starting with the request record's
`error` field. Format their values with `%s`, never `%q`: the
quoting sits at the JSON level, not in the message. The same holds
for client-facing bodies behind the `rebear:` prefix: they spell out
the record's error verbatim, so a message formatted for one is
right for both.
