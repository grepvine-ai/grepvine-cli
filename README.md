# gv: the Grepvine CLI

`gv` searches and manages your [Grepvine](https://grepvine.ai) organizations, indexes and documents from the terminal.

## Install

On macOS or Linux, install the latest release into `~/.local/bin`:

```sh
curl -fsSL https://get.grepvine.ai/install.sh | sh
```

The script downloads the archive for your platform from the [releases](https://github.com/grepvine-ai/grepvine-cli/releases) page and checks it against `checksums.txt` before installing. Pass `--version` to install a specific release, or `--bin-dir` to install somewhere else:

```sh
curl -fsSL https://get.grepvine.ai/install.sh | sh -s -- --version v0.1.1 --bin-dir ~/bin
```

With Homebrew on macOS or Linux:

```sh
brew install grepvine-ai/tap/gv
```

With Go 1.27 or later:

```sh
go install github.com/grepvine-ai/grepvine-cli/cmd/gv@latest
```

Or download an archive for your platform from the [releases](https://github.com/grepvine-ai/grepvine-cli/releases) page, check it against `checksums.txt`, and put `gv` on your `PATH`.

## Sign in

```sh
gv login
```

`gv login` opens a browser so you can approve this device. If the browser doesn't open, copy the printed URL into one. The API key is saved in `~/.grepvine/config.yaml`, or in the file you pass with `--config` or set in `GREPVINE_CONFIG`. You can also set `GREPVINE_API_KEY` instead, and `GREPVINE_API_ENDPOINT` to use a different endpoint. `gv whoami` shows who you are signed in as, and `gv logout` removes the saved key.

`gv config` shows and changes the configuration file. `gv config ls` lists each setting with the value `gv` uses and where it comes from: an environment variable, the file or the default. API keys are shown only in part.

```sh
gv config ls
gv config get endpoint
gv config set endpoint https://api.grepvine.ai
gv config set api_key - < ci.key     # read the value from standard input
gv config unset endpoint
```

## Use

The examples use an organization `example` with an index `docs`.

Upload a file, or a directory with `--recursive`:

```sh
gv upload README.md --index example/docs
gv upload --recursive docs --index example/docs
```

The source can also be an S3 object or prefix. `gv` reads it with your AWS credentials:

```sh
gv upload --recursive s3://my-bucket/docs/ --index example/docs
```

Store one document from a file, or from standard input with `-`. Each `--metadata` flag sets one field: `NAME=VALUE` stores a string, and `NAME:TYPE=VALUE` a value of type `string`, `integer`, `number`, `boolean` or `timestamp`:

```sh
gv put example/docs notes/today.md today.md --metadata team=search --metadata pages:integer=3
echo "hello" | gv put example/docs hello.txt -
```

Search, list, download and delete documents. `delete` takes one or more keys:

```sh
gv search -F example/docs Grepvine
gv list example/docs --delimiter /
gv get example/docs README.md --output README.copy.md
gv get example/docs README.md --range bytes=0-127
gv delete example/docs README.md notes/today.md hello.txt
```

Keep an index in step with a directory or a git repository. After the first sync, a later sync in a git repository uploads only the files that changed since the last synced commit:

```sh
gv sync docs example/docs
```

Manage indexes, including their Markdown descriptions and key/value tags:

```sh
gv index ls example
gv index info example/docs
gv index create example/handbook --description "Every guide we publish."
gv index update example/docs --description-file DESCRIPTION.md
gv index tag set example/docs env=prod
gv index rm example/handbook
```

Manage organizations, members and API keys with `gv org`, `gv member` and `gv key`. Run `gv <command> --help` for every flag.

### Default organization

Set `organization` in `~/.grepvine/config.yaml`, or `GREPVINE_ORGANIZATION`, to omit the organization: `docs` then means `example/docs`, and `gv index ls`, `gv org info` and `gv member ls` need no argument. An explicit `ORGANIZATION/INDEX` still wins, and `GREPVINE_ORGANIZATION` wins over the file. `gv config set organization example` saves it for you.

```yaml
organization: example
```

```sh
gv search -F docs Grepvine
gv index ls
```

## JSON output

Add `--json` to print one JSON object per line, with fields as in the API:

- `search`: one result per line. `matches[].offset` counts bytes from the start of the document; subtract `text_start_offset` to find the match within `text`.
- `list`: one document per line; with `--delimiter`, each common prefix comes first as `{"prefix": ...}`.
- `whoami`, and the `org`, `member`, `key` and `index` commands: the objects they show.
- `put`: the stored document's `{"key", "revision"}`.
- `delete`: one result per key, in order: `{"key", "revision"}` when deleted, `{"key", "error"}` when not.
- `upload` and `sync`: each document the index refused, as the API reports it, then a last line of totals: `{"uploaded", "bytes", "deleted", "failed"}`. A refused upload stops `sync` before it deletes anything, so one run reports refused uploads or refused deletions, not both.
- `sync --dry-run`: each planned change, as `{"action": "upload"|"delete"|"tag", "key", ...}`. A tag change carries its new `value`, or `null` when the tag is removed.

## Shell completion

`gv` completes commands and flags in bash, zsh, fish and PowerShell. Load it in the current session:

```sh
source <(gv completion bash)                          # bash
source <(gv completion zsh)                           # zsh
gv completion fish | source                           # fish
gv completion powershell | Out-String | Invoke-Expression   # PowerShell
```

To load it in every new session, save the script once and start a new shell:

```sh
gv completion bash > $(brew --prefix)/etc/bash_completion.d/gv                 # bash, macOS
gv completion bash > /etc/bash_completion.d/gv                                 # bash, Linux
gv completion zsh > $(brew --prefix)/share/zsh/site-functions/_gv              # zsh, macOS
gv completion zsh > "${fpath[1]}/_gv"                                          # zsh, Linux
gv completion fish > ~/.config/fish/completions/gv.fish                        # fish
```

For PowerShell, add the `Invoke-Expression` line above to your profile. Run `gv completion <shell> --help` for details.

## Build from source

```sh
make build      # dist/gv
make verify     # lint, test and build
```

See [CONTRIBUTING.md](CONTRIBUTING.md).

## License

Apache License 2.0. See [LICENSE](LICENSE) and [NOTICE](NOTICE).
