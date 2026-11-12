---
name: grepvine-cli
description: Search, read, upload and sync documents in Grepvine indexes with the gv command-line tool. Use when the user mentions Grepvine, gv, or a Grepvine organization or index; asks to search a large indexed collection of text or code with regular expressions or literal strings; or asks to upload, sync, list, download or delete documents, or to manage Grepvine indexes, organizations, members or API keys.
---

# Grepvine with gv

Grepvine indexes text documents and searches every line of them with regular expressions or literal strings. `gv` is its command-line client. An index is named `ORGANIZATION/INDEX`, and a document in it is addressed by its key, a path such as `src/main.go`.

Run `gv <command> --help` whenever you need a flag that is not shown here.

## Before you start

1. Check that `gv` is installed and signed in: `gv whoami --json`.
   - If `gv` is missing, tell the user to install it from https://github.com/grepvine-ai/grepvine-cli.
   - If it says to sign in, ask the user to run `gv login`. It opens a browser where the user approves the device, so it needs them present. In CI or a container, the user sets `GREPVINE_API_KEY` instead.
2. Find the default organization, if one is set: `gv config get organization`. With a default, `docs` means `ORGANIZATION/docs`, and commands that take an organization can omit it. When none is set, always write `ORGANIZATION/INDEX`.

Never print, log, store or commit an API key, and never read the configuration file to look for one.

## Pick an index

```sh
gv org ls --json                          # organizations you belong to
gv index ls ORGANIZATION --json           # their indexes, with descriptions
gv index info ORGANIZATION/INDEX --json   # one index: description, tags, document count
```

Read an index's description before you search it: it says what the index holds and how its keys are laid out. An index kept in step with a git repository has `git.commit` and `git.branch` tags that tell you which commit it reflects.

## Search

```sh
gv search ORGANIZATION/INDEX 'func \w+Handler\(' --json --max-results 50
gv search ORGANIZATION/INDEX -F 'connection refused' -i --json --max-results 50
gv search ORGANIZATION/INDEX 'TODO' --filter "path STARTSWITH 'src/'" -C 2 --json --max-results 50
```

- Always pass `--json`, and always bound the output with `--max-results`. Without it, `gv` returns every match in the index. Raise the limit or narrow the pattern when you need more.
- The pattern is a regular expression in RE2 syntax: no lookarounds and no backreferences. Pass `-F` to search for a literal string; it is faster and needs no escaping.
- `-i` ignores case. `-w` matches whole words and requires `-F`. `-U` lets a match span lines. `-C`, `-B` and `-A` add up to 20 lines of context.
- `--filter` selects documents by metadata. Files uploaded with `gv` carry `path`, `size` and `mtime`.
- Exit status 1 with no output means nothing matched. That is a result, not an error.
- Put a pattern that starts with `-` after `--`: `gv search ORGANIZATION/INDEX --json -- '-v'`.

Each JSON line is one result: its document `key`, `line_number`, the matched `text` and its `matches`, and any context lines. See [references/search.md](references/search.md) for the fields, offsets, regular expression notes and the filter language.

## Read documents

```sh
gv list ORGANIZATION/INDEX --prefix src/ --delimiter / --json   # browse like a directory
gv get ORGANIZATION/INDEX src/main.go                           # print a document
gv get ORGANIZATION/INDEX big.log --range bytes=0-65535         # read part of a large one
```

Use search to find where something is, then `gv get` to read the whole document around it.

## Change an index

Writes are searchable as soon as the command returns.

```sh
gv index create ORGANIZATION/INDEX --description "What this index holds."
gv sync ./docs ORGANIZATION/INDEX --dry-run --json   # preview; then run it without --dry-run
gv upload ./notes --recursive --index ORGANIZATION/INDEX --json
```

- Use `gv sync` to keep an index in step with a directory or git repository. It deletes documents whose files are gone, so always run it with `--dry-run --json` first and show the user what it would delete.
- Use `gv upload` to add or replace documents without deleting any.

See [references/indexing.md](references/indexing.md) for uploads, sync, descriptions, tags and the JSON reports.

## Ask the user first

Get the user's explicit confirmation, naming the exact target, before any of these:

- `gv delete`, `gv index rm`, `gv org rm`, `gv member rm`, or a `gv sync` whose dry run shows deletions.
- `gv key create`, which prints a new secret, and `gv member add` or `gv member set-role`, which change who has access.

The `rm` commands ask for confirmation on a terminal. Pass `--force` to skip it only after the user has confirmed that exact command in the conversation.

For organizations, members, API keys and configuration, see [references/admin.md](references/admin.md).

## Errors

`gv` prints errors on standard error and exits with a non-zero status. The message says what to do: fix the input, sign in, slow down or retry. `gv upload` and `gv sync` already wait and retry when the service asks them to slow down; for other commands, wait a few seconds before you try again, and do not retry in a tight loop.
