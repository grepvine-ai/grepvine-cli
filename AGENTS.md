# Agent guide

`gv` is the command-line client for the Grepvine API. This repository is public.

- `cmd/gv`: the CLI.
- `internal/httpapi`: generated from `spec/openapi.json` by `make generate`. Don't edit by hand.
- `skills/grepvine-cli`: the agent skill `gv skills install` copies into an agent's skills directory; `skills/skills.go` embeds it in `gv`. When you change a command or flag it shows, update it in the same change: `TestSkillCommandsExist` checks every `gv` command line in it.
- `.claude-plugin/`: lists the same skill as a Claude Code plugin.
- `spec/`: the API snapshot and its provenance (`source.json`). It is updated from the Grepvine API, not edited here.

## Rules

- Code, comments, commit messages, tests, help text and errors describe only the public API. Never describe how the service is built or stores data.
- Errors say what the user can do: fix the input, sign in, slow down, retry.
- Before you finish, run `make verify` and `make verify-generated`.
