# Organizations, members, agents, API keys and configuration

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

## Agents

An agent is a named identity for automation, such as a CI job, named `ORGANIZATION/AGENT`. Its ceiling (indexes and permissions: `search`, `read`, `write`, `create_index`, `manage_index`) bounds what it does. Changing agents needs a key with `manage_agents`: `gv login --org ORGANIZATION --permission manage_agents`.

```sh
gv agent ls ORGANIZATION --json
gv agent info ORGANIZATION/AGENT --json
gv agent create ORGANIZATION/AGENT --permission search --permission read --index INDEX
gv agent update ORGANIZATION/AGENT --permission search
gv agent disable ORGANIZATION/AGENT
gv agent enable ORGANIZATION/AGENT
gv agent manager add ORGANIZATION/AGENT USER
gv agent manager rm ORGANIZATION/AGENT USER
gv agent rm ORGANIZATION/AGENT
```

An agent marked `needs review` keeps working; `gv agent update ORGANIZATION/AGENT` with no flags saves it as it is and clears the mark, once the user has checked it.

```sh
gv agent secret create ORGANIZATION/AGENT --expires 90d   # approved in a browser; prints the secret once, and the client ID on stderr
gv agent secret ls ORGANIZATION/AGENT --json
gv agent secret rm ORGANIZATION/AGENT PREFIX
```

A job uses the secret through `GREPVINE_AGENT_SECRET`. Treat it like an API key: hand it to the user as-is, never save, repeat or commit it. `gv login --org ORGANIZATION --agent AGENT` signs in with a key that acts as the agent for the user.

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

Settings live in `~/.grepvine/config.yaml`, or the file given with `--config` or `GREPVINE_CONFIG`. The environment variables `GREPVINE_API_KEY`, `GREPVINE_AGENT_SECRET`, `GREPVINE_API_ENDPOINT` and `GREPVINE_ORGANIZATION` override the file. Don't read or edit the file directly, and don't print the API key or agent secret.
