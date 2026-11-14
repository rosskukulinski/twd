# twd

**twd is a self-hosted, tailnet-native runtime for Cloudflare-style Workers and
Durable Objects.** You run it on your own machines (macOS or Linux). It runs
your Workers code on [workerd](https://github.com/cloudflare/workerd). Every
workload, and **every Durable Object instance**, becomes its own node on your
Tailscale tailnet, with its own MagicDNS name, tags and grants:
- callers are identified by WireGuard identity (`getCaller`), not tokens;
- outbound `fetch()` leaves from the object's own node, so your tailnet
  policy governs what each object can reach;
- `twd kill` cuts an object off in under a second.

It runs Cloudflare Agents SDK agents, MCP servers and Vercel AI SDK agents
unmodified (see [docs/COMPAT.md](docs/COMPAT.md)).

## Quick start: deploy your first Durable Object

You need:
- a Mac (Apple silicon) or a Linux machine (arm64 or amd64);
- a Tailscale tailnet where you're an admin;
- Go ≥ 1.27 and Node.js ≥ 20, to build the release (the installed twd needs
  neither).

**1. Build and install** (no sudo; everything goes into `~/.twd`):

```sh
git clone https://github.com/rosskukulinski/twd && cd twd
make release RELEASE_TARGETS=$(go env GOOS)-$(go env GOARCH)   # → dist/twd-<version>-<os>-<arch>.tar.gz
mkdir -p /tmp/twd && tar -xzf dist/twd-*-$(go env GOOS)-$(go env GOARCH).tar.gz -C /tmp/twd
/tmp/twd/twd-*/install.sh
export PATH="$HOME/.twd/bin:$PATH"             # add this to your shell profile too
twd doctor                                     # every check prints a fix if it fails
```

**2. Let twd mint keys on your tailnet.** In the admin console:
- add this to your **tailnet policy**:
  ```jsonc
  "tagOwners": {
    "tag:twd":      ["autogroup:admin"],
    "tag:twd-mgmt": ["autogroup:admin", "tag:twd"],
    "tag:twd-host": ["autogroup:admin", "tag:twd"],
  },
  "grants": [
    {"src": ["tag:twd-host"], "dst": ["tag:twd-mgmt"], "ip": ["tcp:443", "tcp:80"]},
    {"src": ["tag:twd-mgmt"], "dst": ["tag:twd-host"], "ip": ["tcp:7474"]},
    {"src": ["autogroup:admin"], "dst": ["tag:twd-mgmt"], "ip": ["tcp:443", "tcp:80"], "app": {"twd.dev/cap/admin": [{}]}},
  ],
  ```
- then generate an **OAuth client**: Settings › OAuth clients, **Auth Keys ›
  Write** with tag `tag:twd`. Optionally add **Devices › Core › Write** (so
  kills also delete the device) and **Policy File › Read** (so deploys
  preflight your grants).

Save the client where only you can read it:

```sh
mkdir -p ~/.config/twd
printf 'TS_OAUTH_CLIENT_ID=%s\nTS_OAUTH_CLIENT_SECRET=%s\n' '<client id>' '<client secret>' > ~/.config/twd/oauth.env
chmod 600 ~/.config/twd/oauth.env
```

**3. Start twd.** This starts the control plane (`twd-mgmt`) and a host
agent on this machine, both joining your tailnet as tagged nodes. No browser
login is needed:

```sh
twd up --oauth-client-file ~/.config/twd/oauth.env --service --yes
```

**4. Create and deploy a Durable Object app.** The `agent` template is a
Cloudflare Agents SDK `Agent`. Each instance is a Durable Object with its own
SQLite storage, **and** its own tailnet node.

```sh
twd init support --template agent              # also: hello, mcp, ai-agent, node-workflow
cd support
twd deploy                                     # bundles, uploads, waits until the node is ready
```

`twd deploy` preflights the app's tags. If your policy is missing a
`tagOwners` line for them, it prints the exact snippet to add; add it and
run `twd deploy` again. When it's done, it prints the workload's address,
e.g. `http://support-<host>.<tailnet>.ts.net/`.

**5. Call it** from any device on your tailnet. Each name is a separate
Durable Object instance. The JSON reply shows its verified `caller` (your
tailnet identity) and its persisted `state.visits`:

```sh
curl http://support-<host>.<tailnet>.ts.net/agents/support-agent/alice
curl http://support-<host>.<tailnet>.ts.net/agents/support-agent/alice   # same object: its visit count goes up
twd ls                                         # deployments and objects (each with its own node)
twd audit <object-id>                          # who created, called, spawned or killed it
twd kill <object-id> --cascade                 # cut it (and its sub-agents) off the tailnet
```

## More

- **Install details, other modes and troubleshooting:**
  [docs/INSTALL.md](docs/INSTALL.md).
  - Other ways to mint keys: workload identity federation (`twd up --wif`)
    or an API key.
  - A separate developer laptop: `twd link`.
  - Adding hosts: `twd host add`.
  - The Linux microVM tier (Firecracker).
- **Serve chosen paths to the public internet** (webhooks, OAuth callbacks)
  over Funnel: [docs/public-ingress.md](docs/public-ingress.md).
- **Run an app locally with no tailnet at all:** `twd dev`.
- **What runs and what doesn't:** [docs/COMPAT.md](docs/COMPAT.md)
  (Agents SDK, MCP, Vercel AI SDK) and [docs/node-tier.md](docs/node-tier.md)
  (Node.js apps).
- **mgmt's API:** [api/openapi.yaml](api/openapi.yaml), rendered in
  [docs/contracts/mgmt-api.md](docs/contracts/mgmt-api.md).
- **Running several mgmt replicas on Postgres:** the chaos and soak harness
  and its findings are in [e2e/multimgmt/](e2e/multimgmt/)
  (`LEDGER.md`).
- **Build status and measured results:** [STATUS.md](STATUS.md).
- **Design:** the RFC and ADRs live in
  `rosskukulinski/tailnet-agents-design` (private).
