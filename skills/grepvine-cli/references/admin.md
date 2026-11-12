# Organizations, members, API keys and configuration

Every command in this file changes who can do what, or prints secrets. Run the read-only `ls` and `info` commands freely; ask the user before anything else.

## Organizations

```sh
gv org ls --json
gv org info ORGANIZATION --json
gv org rm ORGANIZATION   # deletes every index and document in it; cannot be undone
```

Create an organization and transfer its ownership in the console; `gv` cannot.

## Members

Roles, from most to least access: `owner`, `admin`, `developer`, `reader`. An organization has one owner; change it in the console.

```sh
gv member ls ORGANIZATION --json
gv member add ORGANIZATION USER --role developer
gv member set-role ORGANIZATION USER reader
gv member rm ORGANIZATION USER
```

## API keys

```sh
gv key create NAME --org ORGANIZATION --index INDEX --permission search --expires 30d   # prints the new key once; --expires never for no expiry
```

`gv key create` asks for the key in a browser, like `gv login`, so the user must be present to approve it; without flags it asks to search and read every index for 90 days. A key reaches only what it was approved for and never more than the user's role. Listing and revoking keys happen in the console, not in `gv`.

`gv key create` prints a secret. Create one only when the user asks, and hand it to them as-is: don't save it to a file, repeat it in a summary or commit it. `gv login` creates a key for the device it runs on; `gv logout` removes the saved key.

## Configuration

```sh
gv config ls                                # each setting, its value and where it comes from
gv config get organization
gv config set organization ORGANIZATION     # the default organization
gv config unset organization
```

Settings live in `~/.grepvine/config.yaml`, or the file given with `--config` or `GREPVINE_CONFIG`. The environment variables `GREPVINE_API_KEY`, `GREPVINE_API_ENDPOINT` and `GREPVINE_ORGANIZATION` override the file. Don't read or edit the file directly, and don't print the API key.
