# Creating and updating indexes with gv

## Create and describe an index

```sh
gv index create ORGANIZATION/INDEX --description "API reference for the payments service, one Markdown file per endpoint."
gv index create ORGANIZATION/INDEX --description-file DESCRIPTION.md --tag team=payments --tag env=prod
gv index update ORGANIZATION/INDEX --description-file DESCRIPTION.md
gv index update ORGANIZATION/INDEX --description ""        # clear it
```

An index name starts with a letter and holds letters, digits and hyphens. Creating an index requires the admin or owner role in its organization.

Write the description for the people and agents who will search the index: what it holds, where it comes from, how keys are laid out, and how often it is updated. It is Markdown.

Tags are `KEY=VALUE` pairs on the index:

```sh
gv index tag ls ORGANIZATION/INDEX --json
gv index tag set ORGANIZATION/INDEX env=prod owner=search
gv index tag rm ORGANIZATION/INDEX owner
```

`gv sync` maintains the `git.commit` and `git.branch` tags itself; don't change them by hand.

## Keep an index in step with a directory: gv sync

```sh
gv sync DIRECTORY ORGANIZATION/INDEX --dry-run --json
gv sync DIRECTORY ORGANIZATION/INDEX --json
```

`gv sync` makes the index mirror the directory. It uploads files that are new or changed and **deletes documents that have no file**.

- In a git work tree, it uploads the tracked files that changed since the commit recorded in the index's `git.commit` tag, deletes the documents of files removed since, and then records the new commit. Tracked files with uncommitted changes stop the sync before anything is written: ask the user to commit or stash them.
- Outside git, on the first sync, or when the recorded commit is not in the repository, it compares the whole directory with the whole index.

Always run `--dry-run --json` first. Each line is a planned change: `{"action": "upload"|"delete"|"tag", "key", ...}`. If any line is a `delete`, show the user how many documents and which keys would be deleted, and sync only after they confirm. Pointing sync at the wrong directory or index can delete most of an index.

## Add or replace documents: gv upload

```sh
gv upload FILE --index ORGANIZATION/INDEX --json
gv upload DIRECTORY --recursive --index ORGANIZATION/INDEX --json
gv upload DIRECTORY --recursive --index ORGANIZATION/INDEX PREFIX/ --json   # keys start with PREFIX/
gv upload s3://BUCKET/PREFIX/ --recursive --index ORGANIZATION/INDEX --json
gv upload archive.zip --zip --index ORGANIZATION/INDEX --json
gv upload records.ndjson --records --index ORGANIZATION/INDEX --json
```

- A document's key is the file's path relative to the directory uploaded, after the optional prefix. Uploading a key that exists replaces that document.
- `gv upload` never deletes documents. Use it when the index collects documents from several places, and `gv sync` when it should mirror one directory.
- An `s3://` source is read with the user's AWS credentials.
- `--records` reads NDJSON files of `{"key", "content", "metadata"}` objects, one per line, optionally gzip-compressed. A metadata value is a plain string, boolean or integer, or a tagged `{"type", "value"}` object; numbers that are not integers and timestamps are always tagged, for example `{"team": "payments", "due": {"type": "timestamp", "value": "2026-03-04T05:06:07Z"}}`. The types are `string`, `integer`, `number`, `boolean` and `timestamp`.
- Directory uploads skip what `.gitignore` excludes. Content must be UTF-8 text to be searchable; other files are stored but never match a search.

## Reading the JSON report

With `--json`, `gv upload` and `gv sync` print each document the index refused, with the error the service gave, and then one last line of totals:

```json
{"uploaded": 120, "bytes": 482113, "deleted": 3, "failed": 0}
```

A non-zero `failed`, or a non-zero exit status, means some documents were not stored or not deleted. Report the refused keys and their errors to the user. A refused upload stops `gv sync` before it deletes anything.

## Delete documents and indexes

Ask the user first, naming the exact target.

```sh
gv delete ORGANIZATION/INDEX KEY
gv index rm ORGANIZATION/INDEX      # deletes the index and all of its documents; cannot be undone
```

`gv index rm` asks the user to type the index name on a terminal. Pass `--force` only after the user has confirmed that exact command in the conversation.
