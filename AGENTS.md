# chalet-mcp-server

Guidance for agents working in this repo. The README is for the operator:
what the server is, its configuration, running it locally, releasing it. How
it works and how to change it belongs here.

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
  stack, never with the request.
- **Every challenge is HTTP.** The hosted clients sign in on a 401 and step
  up on a 403 with `insufficient_scope`; a tool result with `isError` starts
  neither. The 401 (`resource_metadata`, `scope="read"`) answers no token,
  no client secret, or the exchange's `invalid_grant`. The 403
  `insufficient_scope` (`scope="write"`) answers a write tool on a read
  grant, and the app's own 403 `insufficient_scope` during a call: the MCP
  answer is held until the call is done, so that it can still become the 403.
- **The exchange forks into terminal or retry** (the toolkit's
  `auth/exchange`). Only `invalid_grant` is terminal: the person's token is
  dead, and the 401 makes the client sign in again. Every other failure —
  this server's credential refused, the app away, a 429 — is a 503, with
  `Retry-After` when the exchange gives one, and never a reason to consent
  again. The app's own no to a grant's identity, such as AI connections off
  for the account, is a 403 with the app's sentence: the grant survives the
  switch, and a 401 would make the client throw a good grant away.
- **A dead token is dropped.** A 401 from the app during a call drops the
  grant from the cache, so the next request exchanges again, and a dead
  grant then gets the 401. The call's own error says to reconnect the app,
  never `chalet auth login` (`mcpserver.Config.SignIn`).
- **No write goes twice.** This server never repeats a request: not an
  exchange, not a call. chalet-cli's handler sends each call once, and its
  hint tells the agent to check before it calls again.
- **One bad call does not stop the server.** Every grant's server turns a
  panic into an internal error (`recovered`).
- **The cache** keeps one delegated token per grant, with the server built
  for its account and scope, until 10 seconds before the token expires; a
  burst for one grant exchanges once. The catalog is asked per grant, and a
  cache shared by every grant would go in `build`.
- **The budgets**: a request runs for 45 seconds at most, the write timeout
  is 60 seconds, and SIGTERM drains for 30.
- **The libraries in `go.mod`** are pinned: chalet-cli, the MCP toolkit and
  the MCP SDK, the last two at chalet-cli's versions. Move them
  deliberately, with the tests as the gate.

## Development

    go vet ./...
    go test -race ./...

The tests drive the HTTP handler with real MCP requests against a fake app in
`internal/hosted`: its token endpoint, identity, catalog and API.
`internal/hosted/testdata/catalog.json` is a copy of chalet-cli's recorded
catalog; refresh the two together.

To work on chalet-cli's tool layer and this server at once, use a `go.work`
that replaces chalet-cli with a local checkout (README). Git ignores it.
