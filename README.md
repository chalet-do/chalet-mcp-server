# chalet-mcp-server

Chalet's tools for the hosted clients: the assistants that connect to an MCP
server by URL and sign in with OAuth. It is the server behind
`https://mcp.chalet.do/mcp`. It serves the same tools as `chalet mcp` from
[chalet-cli](https://github.com/chalet-do/chalet-cli), which builds them from
Chalet's catalog, over HTTP instead of stdio.

It holds no token of its own. Each request brings the person's token for this
server, and the server trades it at Chalet's token endpoint, as its own
client, for a Chalet token that lasts five minutes at most. It keeps that
token, with the tools built for the person's grant, until shortly before it
expires.

## Configuration

The environment alone:

| Variable | What | Default |
|---|---|---|
| `CHALET_URL` | Where Chalet runs | `https://chalet.do` |
| `CHALET_MCP_RESOURCE` | This server's URL, as people paste it | `https://mcp.chalet.do/mcp` |
| `CHALET_MCP_LISTEN` | Where it listens | `:8080` |
| `CHALET_MCP_CLIENT_ID` | Its client id at Chalet | `chalet-mcp-server` |
| `CHALET_MCP_CLIENT_SECRET` | Its client secret | none: every request gets the 401 |

`http://` is accepted for localhost alone.

## What it answers

- `POST /mcp`: MCP, stateless, answered in JSON. GET and DELETE answer 405.
  A query string changes nothing: the person's grant decides the account.
- `GET /up`: 200, for the health check.
- `GET /.well-known/oauth-protected-resource/mcp`: where a client signs in.

A request without a token, or with one that Chalet refuses, gets a 401 that
tells the client where to sign in. A *Read* grant lists the reading tools
alone, and a call that needs more gets a 403 that asks for *Read and write*.
When Chalet does not answer, or refuses this server's own credential, the
answer is a 503: try again later, never sign in again. When the account has
AI connections off, the answer is a 403 that says so, and the grant works
again once they are back on.

A request runs for 45 seconds at most, and a response is written within 60.
On SIGTERM the server lets the requests in flight finish, for up to 30
seconds. It writes one line to stderr for each request, with the client's
address from `X-Forwarded-For`, and never a token.

## Run it locally

Against chalet-app in development, at its own URL (here
`http://localhost:3007`):

1. Give the app this server's local URL as its MCP resource,
   `CHALET_OAUTH_MCP_RESOURCE=http://localhost:8080/mcp`, in its
   environment, and make the client's secret there:

       bin/rails oauth:mint_mcp_server_secret

   It prints the secret once; each run makes a new one.

2. Start the server with that secret:

       CHALET_URL=http://localhost:3007 \
       CHALET_MCP_RESOURCE=http://localhost:8080/mcp \
       CHALET_MCP_CLIENT_SECRET=... \
         go run ./cmd/chalet-mcp-server

3. Connect an MCP client that signs in with OAuth to
   `http://localhost:8080/mcp`.

To change chalet-cli's tool layer and this server together, put a `go.work`
beside `go.mod` that replaces chalet-cli with your checkout. Git ignores it.

    go 1.26.7

    use .

    replace github.com/chalet-do/chalet-cli => ../chalet-cli

## Releasing

Push a version tag. `.github/workflows/release.yml` runs the tests (the same
`test.yml` runs on every pull request), then GoReleaser
(`.goreleaser.yaml`). The archives go on the GitHub release with their
checksums: `chalet-mcp-server_<version>_linux_amd64.tar.gz` for the VM,
`chalet-mcp-server_<version>_darwin_arm64.tar.gz` for a local run, and
`chalet-mcp-server_<version>_checksums.txt`.

    git tag v0.1.0 && git push origin v0.1.0

No image is published. chalet-app's deploy pins the version and the
checksum of the linux/amd64 archive, builds the image from that archive, and
loads it on the VM.
