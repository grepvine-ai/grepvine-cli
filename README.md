# gv: the Grepvine CLI

`gv` searches and manages your [Grepvine](https://grepvine.ai) organizations, indexes and documents from the terminal.

## Install

On macOS or Linux, install the latest release into `~/.local/bin`:

```sh
curl -fsSL https://grepvine-ai.github.io/grepvine-cli/install.sh | sh
```

The script downloads the archive for your platform from the [releases](https://github.com/grepvine-ai/grepvine-cli/releases) page and checks it against `checksums.txt` before installing. Pass `--version` to install a specific release, or `--bin-dir` to install somewhere else:

```sh
curl -fsSL https://grepvine-ai.github.io/grepvine-cli/install.sh | sh -s -- --version v0.1.1 --bin-dir ~/bin
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

`gv login` opens a browser so you can approve this device. If the browser doesn't open, copy the printed URL into one. The API key is saved in `~/.grepvine/config.yaml`, or in the file you pass with `--config`. You can also set `GREPVINE_API_KEY` instead, and `GREPVINE_API_ENDPOINT` to use a different endpoint. `gv whoami` shows who you are signed in as, and `gv logout` removes the saved key.

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

Search, list, download and delete documents:

```sh
gv search -F example/docs Grepvine
gv list example/docs --delimiter /
gv get example/docs README.md --output README.copy.md
gv get example/docs README.md --range bytes=0-127
gv delete example/docs README.md
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

Manage organizations, members and API keys with `gv org`, `gv member` and `gv key`. Add `--json` to listing commands for machine-readable output. Run `gv <command> --help` for every flag.

## Build from source

```sh
make build      # dist/gv
make verify     # lint, test and build
```

See [CONTRIBUTING.md](CONTRIBUTING.md).

## License

Apache License 2.0. See [LICENSE](LICENSE) and [NOTICE](NOTICE).
