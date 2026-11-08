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
gv key ls --json
gv key create NAME          # prints the new key once
gv key rm ID                # revoke it
```

`gv key create` prints a secret. Create one only when the user asks, and hand it to them as-is: don't save it to a file, repeat it in a summary or commit it. `gv login` creates a key for the device it runs on; `gv logout` removes the saved key.

## Configuration

```sh
gv config ls                                # each setting, its value and where it comes from
gv config get organization
gv config set organization ORGANIZATION     # the default organization
gv config unset organization
```

Settings live in `~/.grepvine/config.yaml`, or the file given with `--config` or `GREPVINE_CONFIG`. The environment variables `GREPVINE_API_KEY`, `GREPVINE_API_ENDPOINT` and `GREPVINE_ORGANIZATION` override the file. Don't read or edit the file directly, and don't print the API key.
