# Contributing

Thanks for helping improve `gv`.

## Development

You need Go (the version in `go.mod`) and `make`.

```sh
make test       # run the tests
make lint       # run golangci-lint
make build      # build dist/gv
make verify     # all of the above
```

`internal/httpapi` is generated from the API description in `spec/openapi.json`. Don't edit either one by hand. The snapshot is updated from the Grepvine API, and `make generate` regenerates the client from it. CI fails when the generated code is out of date (`make verify-generated`).

## Pull requests

- Keep each change focused, and add or update tests for behavior you change.
- `make verify` must pass.
- Error messages and help text should tell the user what to do next: fix the input, sign in, retry.

## Reporting bugs

Open an issue with the `gv --version` output, the command you ran and what happened. Report security issues privately, as described in [SECURITY.md](SECURITY.md).
