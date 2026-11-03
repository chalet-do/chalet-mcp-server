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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/url"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/basecamp/mcp/auth/exchange"
	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
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
	// ClientID and ClientSecret are this server's own credential for the
	// exchange. Without the secret it holds none, and every request gets the
	// 401: dark until configured.
	ClientID, ClientSecret string
}

const (
	// The most one request may take, the exchange included.
	requestBudget = 45 * time.Second
	// A delegated token goes this long before it expires, so none is sent in
	// its last seconds (the exchange's own skew).
	retireEarly = 10 * time.Second
	// How a person signs in again here: in the app, never with chalet auth
	// login, which is the command line's.
	signIn = "The connection to Chalet has ended or was revoked: the owner reconnects this app to Chalet and signs in again."
)

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
	metadataPath := "/.well-known/oauth-protected-resource" + resource.Path

	h := &hosted{
		upstream:    strings.TrimRight(cfg.Upstream, "/"),
		metadataURL: resource.Scheme + "://" + resource.Host + metadataPath,
		version:     version,
		logger:      logger,
		grants:      map[[sha256.Size]byte]*grant{},
	}
	if cfg.ClientSecret != "" {
		var err error
		if h.exchanger, err = exchange.New(exchange.Config{
			TokenURL: h.upstream + "/oauth/token",
			Auth:     exchange.ClientSecretPost(cfg.ClientID, cfg.ClientSecret),
		}); err != nil {
			return nil, err
		}
	}
	h.mcp = mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server {
		return r.Context().Value(grantKey{}).(*grant).server.MCP
	}, &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true, PropagateRequestCancellation: true, Logger: logger})

	mux := http.NewServeMux()
	// Only POST: the mux answers GET and DELETE with 405.
	mux.Handle("POST "+resource.Path, http.TimeoutHandler(http.HandlerFunc(h.serveMCP), requestBudget, "Chalet did not answer in time: try again."))
	mux.Handle(metadataPath, auth.ProtectedResourceMetadataHandler(&oauthex.ProtectedResourceMetadata{
		Resource:               cfg.Resource,
		AuthorizationServers:   []string{h.upstream},
		ScopesSupported:        []string{"read", "write"},
		BearerMethodsSupported: []string{"header"},
		ResourceName:           "Chalet",
	}))
	mux.HandleFunc("GET /up", func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, "ok\n") })
	return h.logged(mux), nil
}

type hosted struct {
	upstream    string
	metadataURL string
	exchanger   *exchange.Exchanger // nil without a secret
	mcp         http.Handler
	version     string
	logger      *slog.Logger

	mu sync.Mutex
	// By the SHA-256 of the person's token: the token itself is never kept.
	grants map[[sha256.Size]byte]*grant
	flight singleflight.Group
}

// grant is one person's connection as this server holds it: the server built
// for its one account and its scope, the delegated token inside, until the
// token is about to expire.
type grant struct {
	server     *mcpserver.Server
	writes     bool
	writeTools map[string]bool // the tools a write grant has; a read grant's call to one asks for write
	retire     time.Time
}

type grantKey struct{}

// serveMCP signs the request in, then lets the MCP handler answer it. Every
// challenge is HTTP: a client signs in on a 401 and steps up on a 403 with
// insufficient_scope, and a tool result with isError starts neither.
func (h *hosted) serveMCP(w http.ResponseWriter, r *http.Request) {
	token := bearerToken(r)
	if token == "" || h.exchanger == nil {
		h.challenge(w, http.StatusUnauthorized, "", "read")
		return
	}
	g, err := h.grant(r.Context(), token)
	var refused refusedGrant
	switch {
	case exchange.IsTerminal(err):
		// invalid_grant: the person's token is dead, and the client signs in again.
		h.challenge(w, http.StatusUnauthorized, "invalid_token", "read")
		return
	case errors.As(err, &refused) && refused.Status < 500 && refused.Status != http.StatusTooManyRequests:
		// The app's own no, such as AI connections being off: the grant
		// lives on and works again when they are back on, so no 401.
		http.Error(w, refused.Error(), http.StatusForbidden)
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
	if !g.writes && g.writeTools[calledTool(body)] {
		h.challenge(w, http.StatusForbidden, "insufficient_scope", "write")
		return
	}

	// The answer is held until the call is done: a call the app refused as
	// insufficient_scope goes out as the 403 that asks for write instead.
	wantsWrite := new(atomic.Bool)
	ctx := context.WithValue(context.WithValue(r.Context(), grantKey{}, g), stepUpKey{}, wantsWrite)
	held := &answer{header: http.Header{}}
	h.mcp.ServeHTTP(held, r.WithContext(ctx))
	if wantsWrite.Load() {
		h.challenge(w, http.StatusForbidden, "insufficient_scope", "write")
		return
	}
	held.sendTo(w)
}

func (h *hosted) challenge(w http.ResponseWriter, status int, code, scope string) {
	params := fmt.Sprintf("resource_metadata=%q, scope=%q", h.metadataURL, scope)
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
		for k, old := range h.grants { // the expired go as the new come
			if time.Now().After(old.retire) {
				delete(h.grants, k)
			}
		}
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

	resp, err := client.Do(ctx, http.MethodGet, "/my/identity", nil, nil, nil)
	if err != nil {
		return nil, err
	}
	if !resp.OK() {
		return nil, refusedGrant{resp}
	}
	var who struct {
		Accounts []struct {
			ID   int    `json:"id"`
			Name string `json:"name"`
			URL  string `json:"url"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal(resp.Body, &who); err != nil {
		return nil, fmt.Errorf("unexpected identity from %s: %w", h.upstream, err)
	}
	if len(who.Accounts) != 1 {
		return nil, fmt.Errorf("the identity of a grant names %d accounts, not one", len(who.Accounts))
	}
	account := who.Accounts[0]

	// Per grant: three calls to the app for each active grant every five
	// minutes, and a new feature reaches every grant as fast. The app serves
	// one catalog to every account, so a cache shared by all grants would go
	// here, when the number of grants makes these calls count.
	cat, err := mcpserver.LoadCatalog(ctx, client)
	if err != nil {
		return nil, err
	}

	client.Account = strconv.Itoa(account.ID)
	// "read" or "read write"; none at all is the narrowest grant.
	writes := slices.Contains(strings.Fields(token.Scope), "write")
	server, err := mcpserver.New(cat, hostedAPI{client}, nil, mcpserver.Config{
		Writes:  writes,
		Account: fmt.Sprintf("%s (%s)", account.Name, account.URL),
		SignIn:  signIn,
		Refused: func(ctx context.Context, resp *chalet.Response) { h.refused(ctx, key, resp) },
	}, h.version, h.logger)
	if err != nil {
		return nil, err
	}
	server.MCP.AddReceivingMiddleware(recovered(h.logger))

	// mcpserver.New has built the same domains without an error.
	domains, _ := cat.GatewayDomains()
	writeTools := map[string]bool{}
	for _, d := range domains {
		if !d.AllReadOnly() {
			writeTools[d.ToolName()] = true
		}
	}
	return &grant{server: server, writes: writes, writeTools: writeTools, retire: token.ExpiresAt.Add(-retireEarly)}, nil
}

// refused hears the app refuse one of the grant's calls. A 401 means the
// delegated token died, most likely with its grant: it is dropped, so the
// next request exchanges again, and a dead grant then gets the 401 challenge.
// A 403 with insufficient_scope means the grant may only read: the request's
// answer becomes the 403 that asks for write.
func (h *hosted) refused(ctx context.Context, key [sha256.Size]byte, resp *chalet.Response) {
	switch {
	case resp.Status == http.StatusUnauthorized:
		h.mu.Lock()
		delete(h.grants, key)
		h.mu.Unlock()
	case resp.Status == http.StatusForbidden && strings.Contains(resp.Header.Get("WWW-Authenticate"), "insufficient_scope"):
		if wanted, ok := ctx.Value(stepUpKey{}).(*atomic.Bool); ok {
			wanted.Store(true)
		}
	}
}

type stepUpKey struct{}

// recovered answers a call that panicked with an internal error, and logs the
// panic and its stack, never the request and so never a token: one bad call
// must not stop the server for everyone.
func recovered(logger *slog.Logger) mcp.Middleware {
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (result mcp.Result, err error) {
			defer func() {
				if v := recover(); v != nil {
					logger.Error("a call panicked", "method", method, "panic", fmt.Sprint(v), "stack", string(debug.Stack()))
					result, err = nil, &jsonrpc.Error{Code: jsonrpc.CodeInternalError, Message: "internal error"}
				}
			}()
			return next(ctx, method, req)
		}
	}
}

// refusedGrant is the app's no to a grant's first request, the identity: its
// ruling, such as AI connections being off for the account, which no retry
// changes.
type refusedGrant struct{ *chalet.Response }

func (e refusedGrant) Error() string { return "Chalet answered " + e.Response.Error() }

// hostedAPI is the client as this server holds it: the person's files are not
// here, so none goes up.
type hostedAPI struct{ *chalet.Client }

func (hostedAPI) Upload(_ context.Context, path string) (string, error) {
	return "", fmt.Errorf("this Chalet server is not on the computer with %s, so it cannot put it up: add the file in Chalet itself, or connect chalet on that computer", path)
}

// calledTool is the tool a JSON-RPC request calls, or "".
func calledTool(body []byte) string {
	var call struct {
		Method string `json:"method"`
		Params struct {
			Name string `json:"name"`
		} `json:"params"`
	}
	if json.Unmarshal(body, &call) != nil || call.Method != "tools/call" {
		return ""
	}
	return call.Params.Name
}

func bearerToken(r *http.Request) string {
	fields := strings.Fields(r.Header.Get("Authorization"))
	if len(fields) != 2 || !strings.EqualFold(fields[0], "Bearer") {
		return ""
	}
	return fields[1]
}

// answer holds a response until it is complete.
type answer struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (a *answer) Header() http.Header { return a.header }

func (a *answer) WriteHeader(status int) {
	if a.status == 0 {
		a.status = status
	}
}

func (a *answer) Write(p []byte) (int, error) {
	a.WriteHeader(http.StatusOK)
	return a.body.Write(p)
}

func (a *answer) sendTo(w http.ResponseWriter) {
	maps.Copy(w.Header(), a.header)
	w.WriteHeader(cmp.Or(a.status, http.StatusOK))
	w.Write(a.body.Bytes())
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
