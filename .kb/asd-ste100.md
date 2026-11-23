# Preface

ASD-STE100, Simplified Technical English: the writing rules
and the controlled dictionary, distilled to what an agent needs
to reproduce the style without the copyrighted standard. A
request for "STE100" or "Simplified Technical English" means
this document.

IMPORTANT: Read the `.kb/agents.md` file.


# Laws

## §1 - Approved words only

Write each word only in its approved part of speech and its
approved sense. One word keeps one meaning. Where a word is not
approved, its approved alternative goes in.

## §2 - No phrasal verbs or idioms

Write "install", "remove", "examine", "obey" - never "set up",
"take off", "check out", "go through".

## §3 - Active voice, present tense

Use the person as the subject where possible. Procedures use
the imperative. Use the passive voice only when the actor is
unknown or irrelevant.

## §4 - One sentence, one idea

Keep sentences under 20 words. A sentence holds one instruction
or one statement. Split longer thoughts into more sentences or
a vertical list of single-step instructions.

## §5 - Concrete language

Use the base verb form over its noun: "to install"
("installation"), "to test" ("testing"). Stack at most three
nouns. Keep articles - dropped articles do not simplify.

# Concepts

## The standard

ASD-STE100 Specification, Simplified Technical English, by the
Aerospace, Security and Defence Industries Association of
Europe. Origin: 1980s aerospace maintenance manuals, written for
non-native readers and translation. Current form: Issue 8,
January 2025 - about 53 writing rules and a controlled
dictionary of about 900 approved words.

## The writing rules

- Approved words only: the dictionary gives each word an
  approved part of speech and an approved meaning. A word
  approved as a noun stays a noun - "start" the noun is fine,
  "start" the verb is not, so you "make the engine operate".
  A word may keep its technical name status: TLS, proxy, inlet.
- No phrasal verbs: one approved single verb replaces each
  multi-word verb.
- Sentence length: at most 20 words in procedures, at most 25
  in descriptive text.
- Shortest sentence form: imperative verb, subject, verb,
  object - an instruction says who does what, never stacks.
- Vertical lists: a procedure with more than one step becomes a
  numbered list of single instructions.
- Active over passive: the passive voice is allowed when the
  actor is unknown or irrelevant.
- Articles are never dropped and never shortened to save words.
- Consistency: the same thing keeps the same word everywhere.
  No synonyms.
- Countable measures for quantities - "three bolts", not
  "several bolts".

## The words, by example

The dictionary keeps verb senses separate to kill one whole
class of ambiguity:

| Standard word | Standard sense or use | Not used for |
|---------------|----------------------|--------------|
| follow        | come after           | obey |
| enter         | to come or go in; to put data in | to join |
| make sure     | to verify, to confirm | (replaces ensure) |
| do a check    | the action           | check as a verb |
| turn          | to move around an axis; to change a control position | turn on/off |
| operate       | to do the function of | to manage |

A word outside the dictionary, or a plain technical term of the
domain, is acceptable when the dictionary has no entry for it -
the rule is against ambiguous and over-rich English, not
against domain vocabulary.

## Applying it in this repository

A request for "STE100" applies the style to comments and docs;
`hacking.md` §1 still holds. The style wants short sentences,
no phrasal verbs, and no ambiguous pronouns. Example from the
tls work: "each pinned leaf beside the config name it went in
under" becomes "Each pin stores the config name of its leaf".
The subject-verb-object form is preferred; the colon-dash
chains of the house style survive unless they hide a pronoun
without a clear referent. Keep the terms the code owns - inlet,
outlet, midlet, flow, sniplet - as they are.
