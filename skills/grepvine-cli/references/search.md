# Searching with gv

## Command

```sh
gv search ORGANIZATION/INDEX PATTERN [flags]
gv search --index ORGANIZATION/INDEX PATTERN [flags]
```

| Flag | Meaning |
|---|---|
| `--json` | One JSON object per result. Use it whenever you read the output. |
| `--max-results N` | Stop after N results. Zero, the default, means all of them. |
| `-F`, `--fixed-strings` | Treat the pattern as a literal string. |
| `-i`, `--ignore-case` | Ignore case, with Unicode case folding. |
| `-w`, `--word-regexp` | Match whole words. Requires `-F`. |
| `-U`, `--multiline` | Let a match span lines. |
| `-C N`, `-B N`, `-A N` | Context lines around each result, up to 20. |
| `--filter EXPR` | Only search documents whose metadata matches EXPR. |
| `--amount N` | Results fetched per request, 1 to 200. Rarely needed. |

`gv search` exits with status 0 when something matched, 1 when nothing did, and 2 on an error.

## Patterns

Patterns use RE2 syntax, the syntax of Go's `regexp` package:

- No lookahead, lookbehind or backreferences. Match a wider pattern and filter the results yourself instead.
- `\d`, `\w`, `\s` and `\b` are ASCII. Use `\p{L}`, `\p{Lu}` and other Unicode classes for letters beyond ASCII.
- Flags can be set inline, for example `(?i)` for case-insensitive.
- Quote the pattern in single quotes so the shell leaves backslashes, `$` and `*` alone.

Without `-U`, a pattern matches within one line: `^` and `$` mark the start and end of a line, a character class never matches a line break, and a pattern that contains a line break is rejected. With `-U`, the pattern runs over the whole document: `^` and `$` still mark line edges, `\A` and `\z` mark the document's edges, and a match may contain line breaks.

Prefer `-F` for plain text such as error messages, identifiers and URLs: it needs no escaping and is faster. A pattern with a long literal part, such as `handleRequest\(`, is faster than one made of character classes, such as `\w+\(`.

## JSON results

Each line is one result: one match, or several matches that share lines, with the lines they cover.

| Field | Meaning |
|---|---|
| `key` | The document's key. |
| `revision` | The document revision the result came from. |
| `line_number`, `last_line_number` | First and last line covered, counting from 1. They differ only for a multiline match. |
| `text` | The covered lines, without the last line break. Cut to 64 KiB around the first match; `text_truncated` is then true. |
| `text_line_number` | The line `text` starts on. Differs from `line_number` only when `text` was cut at its start. |
| `matches` | Each match as `{"offset", "length"}`, in bytes from the start of the document. |
| `text_start_offset` | Byte offset of `text` in the document. A match starts at byte `offset - text_start_offset` of `text`. |
| `line_start_offset`, `line_end_offset` | Byte offsets of the covered lines in the document. |
| `matches_truncated` | True when some matches on these lines were left out. |
| `lines_before`, `lines_after` | Context lines, each `{"line_number", "text", "text_truncated"}`. Present only with `-B`, `-A` or `-C`. |

Offsets count bytes, not characters. To read more of a document around a result, use `gv get ORGANIZATION/INDEX KEY --range bytes=START-END` with offsets from the result.

## Filters

`--filter` takes an expression over document metadata. Files uploaded with `gv upload` or `gv sync` carry:

| Field | Type | Value |
|---|---|---|
| `path` | string | The document key. |
| `size` | integer | The file size in bytes. |
| `mtime` | timestamp | The file's modification time. |

Documents uploaded as records (`gv upload --records`) carry whatever metadata their records set.

```text
path = 'README.md'
path STARTSWITH 'src/' AND NOT path STARTSWITH 'src/vendor/'
size < 102400
mtime >= (CURRENT_DATE - INTERVAL '7 days')
(path STARTSWITH 'docs/' OR path STARTSWITH 'guides/') AND size > 0
owner IS NOT NULL
```

- Comparisons: `=`, `!=`, `<`, `<=`, `>`, `>=` and `STARTSWITH`. `ENDSWITH` and `CONTAINS` are not supported; to match a file extension, search with a pattern and check the `key` of each result, or use a path prefix.
- Combine with `AND`, `OR`, `NOT` and parentheses. Test for a missing field with `IS NULL` and `IS NOT NULL`.
- Literals: strings in single quotes, integers, numbers, `true` and `false`, and timestamps. `CURRENT_DATE - INTERVAL '3 days'` is a timestamp.
- A document whose field is missing or of another type never matches a comparison on it, and `NOT` does not change that.

## Finding things efficiently

1. Read `gv index info ORGANIZATION/INDEX --json` first: the description often says how keys are laid out.
2. Start with a specific literal (`-F`) and a small `--max-results`, then widen.
3. Narrow with `--filter "path STARTSWITH '...'"` once you know which part of the index matters.
4. Use `gv list ORGANIZATION/INDEX --prefix PREFIX --delimiter / --json` to see what lives under a path.
5. Use `gv get` to read a whole document once a result points to it.
