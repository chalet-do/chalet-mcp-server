// Package hosted is chalet-mcp-server: the tools of chalet-cli's mcpserver
// over HTTP, for the clients that connect by URL, each request signed in with
// the person's own OAuth grant. The person's token is audienced to this
// server, and Chalet's API refuses it; the exchange (RFC 8693) trades it for
// a short-lived token that the API takes.
package hosted

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/basecamp/mcp/auth/exchange"
	"github.com/basecamp/mcp/auth/upstream"
	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/modelcontextprotocol/go-sdk/oauthex"
	"golang.org/x/sync/singleflight"

	"github.com/chalet-do/chalet-cli/chalet"
	"github.com/chalet-do/chalet-cli/mcpserver"
)

// Config is what the server needs to know.
type Config struct {
	// Upstream is where Chalet runs: its token endpoint, its API, its catalog.
	Upstream string
	// Resource is this server's URL as people paste it, the audience of the
	// tokens they bring.
	Resource string
	// ClientSecret is this server's own credential for the exchange, as the
	// client chalet-mcp-server. New refuses a Config without it (ErrNoSecret).
	ClientSecret string
}

// The client this server is at the app's token endpoint: the app makes no
// other for the exchange.
const clientID = "chalet-mcp-server"

// Chalet's own icon, the app's favicon.ico, for a client or a search engine
// that looks for one on this host to show beside the connector.
//
//go:embed favicon.ico
var favicon []byte

// ErrNoSecret is New's refusal of a Config without a client secret: the
// server could sign no one in, and that is its fault, never the person's.
var ErrNoSecret = errors.New("no client secret, so the server could sign no one in")

const (
	// A delegated token goes this long before it expires, so none is sent in
	// its last seconds (the exchange's own skew).
	retireEarly = 10 * time.Second
	// How a person signs in again here: in the app, never with chalet auth
	// login, which is the command line's.
	signIn = "The connection to Chalet has ended or was revoked: the person you act for reconnects this app to Chalet and signs in again."
	// The answer to a request that ran out of time. A write may have gone
	// through by then, so it says what chalet-cli's own hint for a write says.
	outOfTime = "Chalet did not answer in time, and a change may have been made: check whether it was done before you call again."
)

// The most one request may take, the exchange included. A variable so that a
// test can stand in a shorter one.
var requestBudget = 45 * time.Second

// New is the server's HTTP handler: a POST to the resource's path is MCP,
// stateless, answered in JSON; GET /up is the health check; and the
// resource's metadata (RFC 9728) says where a client signs in. Any query
// string is ignored: the grant decides the account.
func New(cfg Config, version string, logger *slog.Logger) (http.Handler, error) {
	if err := chalet.CheckBaseURL(cfg.Upstream); err != nil {
		return nil, fmt.Errorf("upstream: %w", err)
	}
	if err := chalet.CheckBaseURL(cfg.Resource); err != nil {
		return nil, fmt.Errorf("resource: %w", err)
	}
	resource, _ := url.Parse(cfg.Resource)
	if resource.Path == "" {
		return nil, fmt.Errorf("resource %s: needs a path, like https://mcp.chalet.do/mcp", cfg.Resource)
	}
	if cfg.ClientSecret == "" {
		return nil, ErrNoSecret
	}
	metadataPath := "/.well-known/oauth-protected-resource" + resource.Path

	h := &hosted{
		upstream:    strings.TrimRight(cfg.Upstream, "/"),
		metadataURL: resource.Scheme + "://" + resource.Host + metadataPath,
		version:     version,
		logger:      logger,
		grants:      map[[sha256.Size]byte]*grant{},
	}
	var err error
	if h.exchanger, err = exchange.New(exchange.Config{
		TokenURL: h.upstream + "/oauth/token",
		Auth:     exchange.ClientSecretPost(clientID, cfg.ClientSecret),
	}); err != nil {
		return nil, err
	}
	h.mcp = mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server {
		return r.Context().Value(grantKey{}).(*grant).server.MCP
	}, &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true, PropagateRequestCancellation: true, Logger: logger})

	mux := http.NewServeMux()
	// Only POST: the mux answers GET and DELETE with 405.
	serve := http.TimeoutHandler(http.HandlerFunc(h.serveMCP), requestBudget, outOfTime)
	mux.Handle("POST "+resource.Path, serve)
	mux.Handle(metadataPath, h.metadata(cfg.Resource))
	// A person in several accounts may name one in the path, /mcp/2: a label
	// for the clients that keep one connector per URL with the query dropped.
	// An agent's own link names the agent, /mcp/agent-<id>, and consent signs
	// the app in as it. Like the query, the server ignores both and the grant
	// decides; each label is a resource of its own, with its own metadata.
	mux.Handle("POST "+resource.Path+"/{label}", withLabel(serve))
	mux.Handle(metadataPath+"/{label}", withLabel(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.metadata(cfg.Resource+"/"+r.PathValue("label")).ServeHTTP(w, r)
	})))
	mux.HandleFunc("GET /up", func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, "ok\n") })
	mux.HandleFunc("GET /favicon.ico", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/x-icon")
		w.Header().Set("Cache-Control", "public, max-age=86400")
		w.Write(favicon)
	})
	return h.logged(mux), nil
}

type hosted struct {
	upstream    string
	metadataURL string
	exchanger   *exchange.Exchanger
	mcp         http.Handler
	version     string
	logger      *slog.Logger

	mu sync.Mutex
	// By the SHA-256 of the person's token: the token itself is never kept.
	grants map[[sha256.Size]byte]*grant
	flight singleflight.Group

	// The catalog every grant shares, and the body it was parsed from.
	catalogMu   sync.Mutex
	catalogBody []byte
	catalog     *mcpserver.Catalog
}

// grant is one person's connection as this server holds it: the server built
// for its one account and its scope, the delegated token inside, until the
// token is about to expire.
type grant struct {
	server *mcpserver.Server
	retire time.Time
}

type grantKey struct{}

// serveMCP signs the request in, then lets the MCP handler answer it. Every
// challenge is HTTP: a client signs in on a 401 and steps up on a 403 with
// insufficient_scope, and a tool result with isError starts neither.
func (h *hosted) serveMCP(w http.ResponseWriter, r *http.Request) {
	token := bearerToken(r)
	switch {
	case token == "":
		h.challenge(w, r, http.StatusUnauthorized, "", "read")
		return
	case !strings.HasPrefix(token, "chalet_at_"):
		// Not one of Chalet's access tokens: refused here, without an
		// exchange. The prefix alone, not the body's length, which is the
		// app's to change; the app's own limits guard the exchange.
		h.challenge(w, r, http.StatusUnauthorized, "invalid_token", "read")
		return
	}
	g, err := h.grant(r.Context(), token)
	var probe *upstream.Failure
	refused := 0 // the status of the identity or the catalog, when the app refused it
	if errors.As(err, &probe) {
		refused = probe.Status
	}
	switch {
	case exchange.IsTerminal(err), refused == http.StatusUnauthorized, refused == http.StatusNotFound:
		// The grant is dead: the exchange said so (invalid_grant), or the
		// identity or the catalog right after a good exchange (revoked,
		// lapsed, or the person left the account). Nothing of it is kept, and
		// the client signs in again.
		h.challenge(w, r, http.StatusUnauthorized, "invalid_token", "read")
		return
	case refused == http.StatusForbidden:
		// The app's own ruling, such as AI connections being off: the grant
		// lives on and works again when they are back on, so no 401. The
		// person reads the app's words, scrubbed as the log has them.
		http.Error(w, "Chalet answered "+upstream.ScrubDescription(probe.Description), http.StatusForbidden)
		return
	case err != nil:
		h.unavailable(w, err)
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, mcp.DefaultMaxRequestBodyBytes))
	if err != nil {
		http.Error(w, err.Error(), http.StatusRequestEntityTooLarge)
		return
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	method, tool := called(body)
	// What was asked, beside the request's own line: a request that runs out
	// of time says no more than its path there.
	h.logger.Info("call", "rpc", method, "tool", tool)
	if slices.Contains(g.server.HeldBack, tool) {
		h.challenge(w, r, http.StatusForbidden, "insufficient_scope", "write")
		return
	}
	h.mcp.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), grantKey{}, g)))
}

// The protected resource metadata (RFC 9728) of the resource at that URL.
func (h *hosted) metadata(resource string) http.Handler {
	return auth.ProtectedResourceMetadataHandler(&oauthex.ProtectedResourceMetadata{
		Resource:               resource,
		AuthorizationServers:   []string{h.upstream},
		ScopesSupported:        []string{"read", "write"},
		BearerMethodsSupported: []string{"header"},
		ResourceName:           "Chalet",
	})
}

// withLabel serves a path's label only when it could be one; what a label may
// name is the app's to rule, at consent.
func withLabel(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if label := r.PathValue("label"); len(label) > 64 || strings.Trim(label, "0123456789abcdefghijklmnopqrstuvwxyz-") != "" {
			http.NotFound(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// The challenge names the metadata of the resource the request came to: the
// label's, when the path has one.
func (h *hosted) challenge(w http.ResponseWriter, r *http.Request, status int, code, scope string) {
	metadataURL := h.metadataURL
	if label := r.PathValue("label"); label != "" {
		metadataURL += "/" + label
	}
	params := fmt.Sprintf("resource_metadata=%q, scope=%q", metadataURL, scope)
	if code != "" {
		params = fmt.Sprintf("error=%q, ", code) + params
	}
	w.Header().Set("WWW-Authenticate", "Bearer "+params)
	http.Error(w, http.StatusText(status), status)
}

// unavailable answers a failure that is not the person's: the exchange
// refused this server or did not answer, or Chalet was away. It asks for a
// retry, never a new consent, so it is never a 401.
func (h *hosted) unavailable(w http.ResponseWriter, err error) {
	h.logger.Warn("could not sign a request in", "error", err)
	var infra *exchange.InfraError
	if errors.As(err, &infra) && infra.RetryAfter > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(int(infra.RetryAfter/time.Second)))
	}
	http.Error(w, "Chalet is not answering: try again in a moment.", http.StatusServiceUnavailable)
}

// grant finds the person's grant, or makes it. A burst for one person makes
// it once, and the others wait for that one.
func (h *hosted) grant(ctx context.Context, subject string) (*grant, error) {
	key := sha256.Sum256([]byte(subject))
	if g := h.fresh(key); g != nil {
		return g, nil
	}
	g, err, _ := h.flight.Do(string(key[:]), func() (any, error) {
		if g := h.fresh(key); g != nil { // made while this one waited
			return g, nil
		}
		// Not the caller's context: the others in the flight wait on it too.
		g, err := h.build(context.WithoutCancel(ctx), key, subject)
		if err != nil {
			return nil, err
		}
		h.mu.Lock()
		defer h.mu.Unlock()
		now := time.Now() // the expired go as the new come
		maps.DeleteFunc(h.grants, func(_ [sha256.Size]byte, old *grant) bool { return now.After(old.retire) })
		h.grants[key] = g
		return g, nil
	})
	if err != nil {
		return nil, err
	}
	return g.(*grant), nil
}

func (h *hosted) fresh(key [sha256.Size]byte) *grant {
	h.mu.Lock()
	defer h.mu.Unlock()
	if g := h.grants[key]; g != nil && time.Now().Before(g.retire) {
		return g
	}
	return nil
}

// build signs a person in: the exchange, then — with the delegated token —
// the one account the grant reaches, where `chalet mcp` takes it from the
// profile, and the catalog; then the server the grant's scope allows.
func (h *hosted) build(ctx context.Context, key [sha256.Size]byte, subject string) (*grant, error) {
	token, err := h.exchanger.Exchange(ctx, subject)
	if err != nil {
		return nil, err
	}
	client := &chalet.Client{BaseURL: h.upstream, Token: token.AccessToken, UserAgent: "chalet-mcp-server/" + h.version}

	who, err := client.Identity(ctx)
	if err != nil {
		return nil, probeFailed("identity", err)
	}
	if len(who.Accounts) != 1 {
		return nil, fmt.Errorf("the identity of a grant names %d accounts, not one", len(who.Accounts))
	}
	account := who.Accounts[0]

	// Asked for per grant, so that a new feature reaches every grant within
	// five minutes; parsed only when it changed.
	cat, err := h.loadCatalog(ctx, client)
	if err != nil {
		return nil, probeFailed("catalog", err)
	}

	client.Account = strconv.Itoa(account.ID)
	api := hostedAPI{Client: client, drop: func() { h.drop(key) }}
	server, err := mcpserver.New(cat, api, nil, mcpserver.Config{
		// "read" or "read write"; none at all is the narrowest grant.
		Writes:  slices.Contains(strings.Fields(token.Scope), "write"),
		Account: account,
		SignIn:  signIn,
	}, h.version, h.logger)
	if err != nil {
		return nil, err
	}
	server.MCP.AddReceivingMiddleware(settled)
	return &grant{server: server, retire: token.ExpiresAt.Add(-retireEarly)}, nil
}

// settled says that a grant's tools never change: its server is built once,
// from the catalog, and a new one replaces it. The SDK offers a notice of a
// change by default, and holds a client that listens for one (SEP-2575's
// subscriptions/listen) until the request's time runs out, which a client
// reads as a server that does not answer. With nothing to wait for, the
// listener gets its acknowledgement and its answer at once.
func settled(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		if listen, ok := req.GetParams().(*mcp.SubscriptionsListenParams); ok && listen != nil && listen.Notifications != nil {
			listen.Notifications.ToolsListChanged = false
		}
		result, err := next(ctx, method, req)
		switch r := result.(type) {
		case *mcp.InitializeResult:
			if r != nil {
				unchanging(r.Capabilities)
			}
		case *mcp.DiscoverResult:
			if r != nil {
				unchanging(r.Capabilities)
			}
		}
		return result, err
	}
}

func unchanging(caps *mcp.ServerCapabilities) {
	if caps != nil && caps.Tools != nil {
		caps.Tools.ListChanged = false
	}
}

// loadCatalog asks for the catalog with the grant's token and parses it only
// when its body differs from the last one. Every grant shares the parsed
// catalog: nothing in the tool layer or the gateway writes into it.
func (h *hosted) loadCatalog(ctx context.Context, client *chalet.Client) (*mcpserver.Catalog, error) {
	resp, err := client.Do(ctx, http.MethodGet, "/agent_catalog", nil, nil, nil)
	if err != nil {
		return nil, err
	}
	if !resp.OK() {
		// A refusal, as the identity's is, so that it keeps its status: a grant
		// that died since is a 401, AI connections switched off a 403.
		return nil, chalet.Refusal{Response: resp}
	}
	h.catalogMu.Lock()
	defer h.catalogMu.Unlock()
	if h.catalog == nil || !bytes.Equal(resp.Body, h.catalogBody) {
		cat, err := mcpserver.ParseCatalog(resp.Body)
		if err != nil {
			return nil, err
		}
		h.catalog, h.catalogBody = cat, resp.Body
	}
	return h.catalog, nil
}

// drop forgets a grant whose delegated token the app refused.
func (h *hosted) drop(key [sha256.Size]byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.grants, key)
}

// probeFailed is a failure of the grant's first requests, the identity and
// the catalog, as the toolkit's upstream.Failure. They carry the delegated
// token, and a broken answer can echo it back, into a transport's error or
// into the app's own words; a Failure renders no cause, and its description
// only scrubbed of anything shaped like a token. A refusal keeps its status,
// so that a dead grant (401, 404) is told from the app's ruling (403).
func probeFailed(request string, err error) *upstream.Failure {
	var refused chalet.Refusal
	if errors.As(err, &refused) {
		f := upstream.New(request, "Chalet refused it", refused.Status, err)
		f.Description = refused.Response.Error()
		return f
	}
	f := upstream.New(request, "Chalet did not answer it", 0, err)
	f.Description = err.Error()
	return f
}

// hostedAPI is the client as this server holds it, and it sees every answer
// of the app to the grant's calls. A 401 means the delegated token died, most
// likely with its grant: the grant is dropped, so the next request exchanges
// again, and a dead grant then gets the 401 challenge. The person's files are
// not here, so none goes up.
type hostedAPI struct {
	*chalet.Client
	drop func()
}

func (a hostedAPI) Do(ctx context.Context, method, path string, query url.Values, body map[string]any, header http.Header) (*chalet.Response, error) {
	resp, err := a.Client.Do(ctx, method, path, query, body, header)
	a.heard(resp)
	return resp, err
}

func (a hostedAPI) Fetch(ctx context.Context, link string) (*chalet.Response, error) {
	resp, err := a.Client.Fetch(ctx, link)
	a.heard(resp)
	return resp, err
}

func (a hostedAPI) heard(resp *chalet.Response) {
	if resp != nil && resp.Status == http.StatusUnauthorized {
		a.drop()
	}
}

func (hostedAPI) Upload(_ context.Context, path string) (string, error) {
	return "", fmt.Errorf("this Chalet server is not on the computer with %s, so it cannot put it up: add the file in Chalet itself, or connect chalet on that computer", path)
}

// called is the method of a JSON-RPC request, and the tool it calls or "".
func called(body []byte) (method, tool string) {
	var call struct {
		Method string `json:"method"`
		Params struct {
			Name string `json:"name"`
		} `json:"params"`
	}
	if json.Unmarshal(body, &call) != nil || call.Method != "tools/call" {
		return call.Method, ""
	}
	return call.Method, call.Params.Name
}

func bearerToken(r *http.Request) string {
	fields := strings.Fields(r.Header.Get("Authorization"))
	if len(fields) != 2 || !strings.EqualFold(fields[0], "Bearer") {
		return ""
	}
	return fields[1]
}

// logged writes a line to the log for each request, and never a header, so
// never a token. Behind the proxy the client's address is in X-Forwarded-For.
func (h *hosted) logged(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start, status := time.Now(), &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(status, r)
		h.logger.Info("request", "method", r.Method, "path", r.URL.Path, "status", status.status,
			"duration", time.Since(start).Round(time.Millisecond), "client", cmp.Or(r.Header.Get("X-Forwarded-For"), r.RemoteAddr))
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(status int) {
	s.status = status
	s.ResponseWriter.WriteHeader(status)
}
