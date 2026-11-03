module github.com/chalet-do/chalet-mcp-server

go 1.26.7

require (
	github.com/basecamp/mcp v0.0.0-20261030141750-45f0629c39c7
	// TODO: pin the pushed chalet-cli commit with `GOWORK=off go get
	// github.com/chalet-do/chalet-cli@<commit> && GOWORK=off go mod tidy`.
	// Until then this placeholder resolves nowhere: only a go.work builds.
	github.com/chalet-do/chalet-cli v0.0.0-00010101000000-000000000000
	github.com/modelcontextprotocol/go-sdk v1.7.0
	golang.org/x/sync v0.20.0
)

require (
	github.com/go-jose/go-jose/v4 v4.1.5 // indirect
	github.com/google/jsonschema-go v0.4.3 // indirect
	github.com/segmentio/asm v1.1.3 // indirect
	github.com/segmentio/encoding v0.5.4 // indirect
	github.com/yosida95/uritemplate/v3 v3.0.2 // indirect
	golang.org/x/oauth2 v0.35.0 // indirect
	golang.org/x/sys v0.48.0 // indirect
	golang.org/x/time v0.15.0 // indirect
)
