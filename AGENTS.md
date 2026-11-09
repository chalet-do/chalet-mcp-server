# chalet-mcp-server

Guidance for agents working in this repo. The README is for the public: what
the server is, how to connect to it, and what it does with a person's data. It
names nothing of how Chalet runs it. How the server works, how it is
configured and how to change it belongs here.

**It knows no Chalet feature.** Its tools are chalet-cli's: the `mcpserver`
package builds them from the app's catalog (`/agent_catalog.json`), and the
`chalet` package speaks to the app's JSON API. This repo holds only what is
hosted: the HTTP transport, the challenges and the resource metadata, the
exchange and its cache of grants, the budgets and the drain, the binary and
its release. Never add a tool, an action or a domain here, and never
special-case one. A change to the tools is a change in chalet-cli. It reaches
this server when `go.mod` moves its chalet-cli pin, with the tests here as
the gate.

- **Never log, print or keep a token**, the person's or the delegated one.
  The cache keys a grant by the SHA-256 of the person's token. A request
  writes one log line, with no header in it. A panic is logged with its
  stack, never with the request. A broken answer can echo the delegated
  token, so the grant's first requests, the identity and the catalog, fail
  as the toolkit's `upstream.Failure` (`probeFailed`): it renders no cause,
  and the app's words only scrubbed, in the log as in the 403 a person
  reads. Their test runs the toolkit's hostile answers (`upstreamtest`)
  against both.
- **Every challenge is HTTP.** The hosted clients sign in on a 401 and step
  up on a 403 with `insufficient_scope`; a tool result with `isError` starts
  neither. The 401 (`resource_metadata`, `scope="read"`) answers no token,
  or the exchange's `invalid_grant`. The 403 `insufficient_scope`
  (`scope="write"`) answers a read grant's call of a tool that needs write,
  one the library's read-only build held back (`Server.HeldBack`), before
  the call runs. A write that the app itself refuses, which only a token
  cached from before a narrowing refresh can meet, gets the tool's
  `forbidden` error.
- **Only a token with the access tokens' prefix goes to the exchange**
  (`chalet_at_`); any other bearer gets the 401 at once. The prefix alone:
  the body's length is the app's to change, and a copy of it here would turn
  every grant away. The app's own limits keep made-up tokens from starving
  real people.
- **The exchange forks into terminal or retry** (the toolkit's
  `auth/exchange`). Only `invalid_grant` is terminal: the person's token is
  dead, and the 401 makes the client sign in again. Every other failure —
  this server's credential refused, the app away, a 429 — is a 503, with
  `Retry-After` when the exchange gives one, and never a reason to consent
  again.
- **The identity right after a good exchange** answers for the grant too. A
  401 or 404 there means the grant died in between (revoked, lapsed, or the
  person left the account): nothing of it is kept, and the 401 with
  `invalid_token` makes the client sign in again. A 403 there is the app's
  ruling, such as AI connections off for the account, and stays a 403 with
  the app's sentence: the grant survives the switch, and a 401 would make
  the client throw a good grant away. A 429 or a 5xx is the 503.
- **A dead token is dropped.** A 401 from the app during a call drops the
  grant from the cache (`hostedAPI`, which sees every answer of the app to a
  call), so the next request exchanges again, and a dead grant then gets the
  401. The call's own error says to reconnect the app, never `chalet auth
  login` (`mcpserver.Config.SignIn`).
- **A label in the path is the app's to rule.** `/mcp/<number>` names an
  account and `/mcp/agent-<id>` an agent, each a resource with its own
  metadata. The server keeps out what could not be a label (`withLabel`) and
  ignores the rest: consent decides what a label names, and the grant what
  it reaches, so an app signed in through an agent's link acts as the agent.
- **No write goes twice.** This server never repeats a request: not an
  exchange, not a call. chalet-cli's handler sends each call once, and its
  hint tells the agent to check before it calls again.
- **One bad call does not stop the server.** chalet-cli's tool layer turns a
  panic in a call into an internal error, on every grant's server.
- **The cache** keeps one delegated token per grant, with the server built
  for its account and scope, until 10 seconds before the token expires; a
  burst for one grant exchanges once. The catalog is asked for per grant and
  parsed only when its body changed; every grant shares the parsed catalog,
  which nothing in the tool layer or the gateway writes into
  (`loadCatalog`).
- **The budgets**: a request runs for 45 seconds at most, the write timeout
  is 60 seconds, and SIGTERM drains for 30. A request out of time may have
  written already, so its answer (`outOfTime`) says to check before calling
  again, never only to try again.
- **The libraries in `go.mod`** are pinned: chalet-cli, the MCP toolkit and
  the MCP SDK, the last two at chalet-cli's versions. Move them
  deliberately, with the tests as the gate.

## Configuration

The environment alone:

| Variable | What | Default |
|---|---|---|
| `CHALET_URL` | Where Chalet runs | `https://chalet.do` |
| `CHALET_MCP_RESOURCE` | This server's URL, as people paste it | `https://mcp.chalet.do/mcp` |
| `CHALET_MCP_LISTEN` | Where it listens | `:8080` |
| `CHALET_MCP_CLIENT_SECRET` | Its client secret at Chalet | none: required |

`http://` is accepted for localhost alone. Without the secret the server does
not start: it says so in one line and exits, so a deploy without it fails its
health check.

## Development

    go vet ./...
    go test -race ./...

The tests drive the HTTP handler with real MCP requests against a fake app in
`internal/hosted`: its token endpoint, identity, catalog and API.
`internal/hosted/testdata/catalog.json` is a copy of chalet-cli's recorded
catalog; refresh the two together.

To work on chalet-cli's tool layer and this server at once, put a `go.work`
beside `go.mod` that replaces chalet-cli with a local checkout. Git ignores it.

    go 1.26.7

    use .

    replace github.com/chalet-do/chalet-cli => ../chalet-cli

## Releasing

A version tag runs `.github/workflows/release.yml`: the tests, then
GoReleaser, which puts the archives and their checksums on the GitHub
release and publishes no image. The names of the archives and of the
checksum file are part of the interface: Chalet's deploy fetches the
linux/amd64 archive by its name and checks it against the checksum it pins.
