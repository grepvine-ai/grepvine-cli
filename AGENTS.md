# Agent guide

`gv` is the command-line client for the Grepvine API. This repository is public.

- `cmd/gv`: the CLI.
- `internal/httpapi`: generated from `spec/openapi.json` by `make generate`. Don't edit by hand.
- `spec/`: the API snapshot and its provenance (`source.json`). It is updated from the Grepvine API, not edited here.

## Rules

- Code, comments, commit messages, tests, help text and errors describe only the public API. Never describe how the service is built or stores data.
- Errors say what the user can do: fix the input, sign in, slow down, retry.
- Before you finish, run `make verify` and `make verify-generated`.
