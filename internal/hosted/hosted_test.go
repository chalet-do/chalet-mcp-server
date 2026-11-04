package hosted

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/basecamp/mcp/auth/exchange"
	"github.com/basecamp/mcp/mcptest"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/chalet-do/chalet-cli/mcpserver"
)

// fakeApp is Chalet as this server meets it: the token endpoint, and for a
// delegated token the identity, the catalog and the API of account 1. It
// writes down every exchange and every other request.
type fakeApp struct {
	catalog string // the catalog it serves; empty for testdata/catalog.json

	mu        sync.Mutex
	exchanges int
	slow      chan struct{}   // when set, an exchange waits until it is closed
	dead      map[string]bool // grants that ended after their exchange
	sent      []*http.Request
}

const confirmURL = "https://chalet.test/1/agent_confirmation/new?token=abc"

func (a *fakeApp) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/oauth/token" {
		a.exchange(w, r)
		return
	}
	a.mu.Lock()
	a.sent = append(a.sent, r)
	a.mu.Unlock()

	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !strings.HasPrefix(token, "chalet_dt_") {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	switch r.Method + " " + r.URL.Path {
	case "GET /my/identity":
		if token == "chalet_dt_switched_off" {
			w.WriteHeader(http.StatusForbidden)
			io.WriteString(w, `{"error":"AI connections are off for this account"}`)
			return
		}
		io.WriteString(w, `{"accounts":[{"id":1,"name":"Personal","url":"https://chalet.test/1/"}]}`)
	case "GET /agent_catalog":
		if a.catalog != "" {
			io.WriteString(w, a.catalog)
			return
		}
		catalog, _ := os.ReadFile("testdata/catalog.json")
		w.Write(catalog)
	case "GET /1/my":
		if token == "chalet_dt_lapsing" {
			// The person disconnected the app: its grant ends, and the
			// delegated token with it.
			a.mu.Lock()
			a.dead["chalet_at_lapsing"] = true
			a.mu.Unlock()
			w.WriteHeader(http.StatusUnauthorized)
			io.WriteString(w, `{"error":"Unauthorized"}`)
			return
		}
		io.WriteString(w, `{"assignments":[{"id":"todo-1","type":"Todo","title":"Call Anna"}]}`)
	case "POST /1/todos/todo-1/completion":
		if token == "chalet_dt_narrowed" {
			// The grant lost write after the exchange: the app's answer to a
			// read token's write.
			w.Header().Set("WWW-Authenticate", `Bearer realm="Chalet", error="insufficient_scope", scope="write"`)
			w.WriteHeader(http.StatusForbidden)
			io.WriteString(w, `{"error":"Forbidden"}`)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	case "POST /1/recordings/todo-1/trash":
		w.WriteHeader(http.StatusPreconditionRequired)
		io.WriteString(w, `{"error":"Confirmation required","confirm":"Trash “Call Anna” in Website?","confirm_url":"`+confirmURL+`"}`)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

// The exchange as the app answers it: a form with this server's credential,
// and a subject the test names.
func (a *fakeApp) exchange(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	a.exchanges++
	slow, dead := a.slow, a.dead[r.PostFormValue("subject_token")]
	a.mu.Unlock()
	if slow != nil {
		<-slow
	}

	w.Header().Set("Content-Type", "application/json")
	if r.PostFormValue("grant_type") != "urn:ietf:params:oauth:grant-type:token-exchange" ||
		r.PostFormValue("subject_token_type") != "urn:ietf:params:oauth:token-type:access_token" {
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"error":"invalid_request"}`)
		return
	}
	if r.PostFormValue("client_id") != "chalet-mcp-server" || r.PostFormValue("client_secret") != "secret" {
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, `{"error":"invalid_client"}`)
		return
	}

	scope, life := "", 300
	switch subject := r.PostFormValue("subject_token"); {
	case dead:
	case subject == "chalet_at_read", subject == "chalet_at_switched_off", subject == "chalet_at_lapsing":
		scope = "read"
	case subject == "chalet_at_write", subject == "chalet_at_narrowed":
		scope = "read write"
	case subject == "chalet_at_brief":
		scope, life = "read", 5
	case subject == "chalet_at_busy":
		w.Header().Set("Retry-After", "60")
		w.WriteHeader(http.StatusTooManyRequests)
		io.WriteString(w, `{"error":"temporarily_unavailable"}`)
		return
	}
	if scope == "" {
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"error":"invalid_grant","error_description":"Subject token is not valid"}`)
		return
	}
	json.NewEncoder(w).Encode(map[string]any{
		"access_token":      strings.Replace(r.PostFormValue("subject_token"), "chalet_at_", "chalet_dt_", 1),
		"issued_token_type": "urn:ietf:params:oauth:token-type:access_token",
		"token_type":        "Bearer",
		"expires_in":        life,
		"scope":             scope,
	})
}

func (a *fakeApp) requests() []*http.Request {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]*http.Request(nil), a.sent...)
}

func (a *fakeApp) exchanged() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.exchanges
}

const metadataURL = "https://mcp.chalet.test/.well-known/oauth-protected-resource/mcp"

// serve runs the server in front of a fake Chalet. Without a secret it holds
// no credential.
func serve(t *testing.T, secret string, app *fakeApp) string {
	t.Helper()
	app.dead = map[string]bool{}
	chalet := httptest.NewServer(app)
	t.Cleanup(chalet.Close)

	handler, err := New(Config{Upstream: chalet.URL, Resource: "https://mcp.chalet.test/mcp", ClientID: "chalet-mcp-server", ClientSecret: secret}, "test", slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return server.URL
}

const initialize = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`

func toolCall(tool, arguments string) string {
	return `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"` + tool + `","arguments":` + arguments + `}}`
}

// request is one JSON-RPC message as a hosted client posts it.
func request(url, token, message string) *http.Request {
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(message))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return req
}

func post(t *testing.T, url, token, message string) (*http.Response, string) {
	t.Helper()
	resp, err := http.DefaultClient.Do(request(url, token, message))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp, string(body)
}

// connectAs connects a real MCP client over HTTP, on the 2026-07-28
// protocol, with the person's token on every request.
func connectAs(t *testing.T, endpoint, token string) *mcp.ClientSession {
	t.Helper()
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	session, err := client.Connect(t.Context(), &mcp.StreamableClientTransport{Endpoint: endpoint, HTTPClient: &http.Client{Transport: bearer(token)}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { session.Close() })
	return session
}

type bearer string

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+string(b))
	return http.DefaultTransport.RoundTrip(r)
}

// The tool's error as the tool layer answers it.
type failure struct {
	Error, Code, Hint string
}

func failed(t *testing.T, text string) failure {
	t.Helper()
	var got failure
	if err := json.Unmarshal([]byte(text), &got); err != nil {
		t.Fatalf("an error result is not JSON: %q", text)
	}
	return got
}

func keys(tools map[string]*mcp.Tool) string {
	var names []string
	for name := range tools {
		names = append(names, name)
	}
	sort.Strings(names)
	return strings.Join(names, ",")
}

// Clients sign in on a 401 alone: no token, a dead one, or a server that
// holds no credential yet. The app's own no is a 403 that leaves the grant
// alone, and anything else that goes wrong in the exchange asks for a retry,
// never a sign-in.
func TestSignsInAtTheHTTPLevel(t *testing.T) {
	for _, c := range []struct {
		name, secret, token string
		status              int
		challenge           string
		retryAfter, body    string
	}{
		{name: "no token", secret: "secret", status: 401, challenge: `Bearer resource_metadata="` + metadataURL + `", scope="read"`},
		{name: "a dead token", secret: "secret", token: "chalet_at_revoked", status: 401, challenge: `Bearer error="invalid_token", resource_metadata="` + metadataURL + `", scope="read"`},
		{name: "no secret", token: "chalet_at_read", status: 401, challenge: `Bearer resource_metadata="` + metadataURL + `", scope="read"`},
		{name: "AI connections off", secret: "secret", token: "chalet_at_switched_off", status: 403, body: "Chalet answered 403 AI connections are off for this account"},
		{name: "a wrong secret", secret: "wrong", token: "chalet_at_read", status: 503},
		{name: "a busy exchange", secret: "secret", token: "chalet_at_busy", status: 503, retryAfter: "60"},
	} {
		app := &fakeApp{}
		url := serve(t, c.secret, app)
		resp, body := post(t, url+"/mcp", c.token, initialize)
		if resp.StatusCode != c.status || resp.Header.Get("WWW-Authenticate") != c.challenge || resp.Header.Get("Retry-After") != c.retryAfter || !strings.Contains(body, c.body) {
			t.Errorf("%s: %d %q, Retry-After %q, %q", c.name, resp.StatusCode, resp.Header.Get("WWW-Authenticate"), resp.Header.Get("Retry-After"), body)
		}
		if c.secret == "" && app.exchanged() != 0 {
			t.Errorf("%s: a server without a credential exchanged %d times", c.name, app.exchanged())
		}
	}
}

func TestAnswersItsOwnRoutes(t *testing.T) {
	handler, err := New(Config{Upstream: "https://chalet.do", Resource: "https://mcp.chalet.do/mcp"}, "test", slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	answer := func(method, path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
		return rec
	}

	metadata := answer("GET", "/.well-known/oauth-protected-resource/mcp")
	if want := `{"resource":"https://mcp.chalet.do/mcp","authorization_servers":["https://chalet.do"],"scopes_supported":["read","write"],"bearer_methods_supported":["header"],"resource_name":"Chalet"}`; metadata.Code != 200 || strings.TrimSpace(metadata.Body.String()) != want {
		t.Errorf("metadata = %d %s", metadata.Code, metadata.Body)
	}
	if up := answer("GET", "/up"); up.Code != 200 {
		t.Errorf("/up = %d", up.Code)
	}
	for _, method := range []string{"GET", "DELETE"} {
		if rec := answer(method, "/mcp"); rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s /mcp = %d", method, rec.Code)
		}
	}
}

// The grant decides: its scope the tools, its one account the requests, and
// the delegated token is all the app sees. A query string changes nothing.
func TestServesWhatTheGrantAllows(t *testing.T) {
	app := &fakeApp{}
	url := serve(t, "secret", app)
	stepUp := `Bearer error="insufficient_scope", resource_metadata="` + metadataURL + `", scope="write"`

	read := connectAs(t, url+"/mcp", "chalet_at_read")
	if got := keys(mcptest.ListTools(t, read)); got != "chalet_my,chalet_picture" {
		t.Fatalf("a read grant lists %v", got)
	}
	if resp, _ := post(t, url+"/mcp", "chalet_at_read", toolCall("chalet_todos_write", `{"action":"complete","params":{"todo_id":"todo-1"}}`)); resp.StatusCode != 403 || resp.Header.Get("WWW-Authenticate") != stepUp {
		t.Errorf("a read grant's write = %d %q", resp.StatusCode, resp.Header.Get("WWW-Authenticate"))
	}

	write := connectAs(t, url+"/mcp?account=2", "chalet_at_write")
	if got := keys(mcptest.ListTools(t, write)); got != "chalet_destructive,chalet_my,chalet_picture,chalet_todos_write" {
		t.Fatalf("a write grant lists %v", got)
	}
	if text, isError := mcptest.CallText(t, write, "chalet_todos_write", map[string]any{"action": "complete", "params": map[string]any{"todo_id": "todo-1"}}); isError || !strings.Contains(text, "204") {
		t.Fatalf("complete = %q (error %v)", text, isError)
	}
	sent := app.requests()
	if last := sent[len(sent)-1]; last.URL.Path != "/1/todos/todo-1/completion" || last.Header.Get("Authorization") != "Bearer chalet_dt_write" {
		t.Errorf("the call went to %s with %q", last.URL.Path, last.Header.Get("Authorization"))
	}
	for _, r := range sent {
		if strings.Contains(r.Header.Get("Authorization"), "chalet_at_") {
			t.Errorf("the person's own token reached %s", r.URL.Path)
		}
	}

	// One hosted client reads the first 512 characters alone: the account and
	// what Chalet is are whole in them.
	data, _ := os.ReadFile("testdata/catalog.json")
	cat, err := mcpserver.ParseCatalog(data)
	if err != nil {
		t.Fatal(err)
	}
	opening, _, _ := strings.Cut(cat.Rules, "\n\n")
	instructions := []rune(write.InitializeResult().Instructions)
	if first := string(instructions[:min(512, len(instructions))]); !strings.Contains(first, "This server reaches one Chalet account: Personal (https://chalet.test/1/).") || !strings.Contains(first, opening) {
		t.Errorf("the first 512 characters = %q", first)
	}

	// No browser on a server: the link is the owner's to follow.
	if text, isError := mcptest.CallText(t, write, "chalet_destructive", map[string]any{"action": "trash", "params": map[string]any{"recording_id": "todo-1"}}); isError || !strings.Contains(text, "Not done yet: Trash “Call Anna” in Website? Give the owner the confirm page: "+confirmURL) {
		t.Errorf("trash = %q (error %v)", text, isError)
	}

	// The exchange said write, the app says no: the client asks for write.
	if resp, _ := post(t, url+"/mcp", "chalet_at_narrowed", toolCall("chalet_todos_write", `{"action":"complete","params":{"todo_id":"todo-1"}}`)); resp.StatusCode != 403 || resp.Header.Get("WWW-Authenticate") != stepUp {
		t.Errorf("the app's insufficient_scope = %d %q", resp.StatusCode, resp.Header.Get("WWW-Authenticate"))
	}
}

// One exchange per grant: a burst waits for the first, and the token is kept
// until shortly before it expires — so one about to expire is never kept.
func TestExchangesOncePerGrant(t *testing.T) {
	app := &fakeApp{}
	url := serve(t, "secret", app)

	app.slow = make(chan struct{})
	var burst sync.WaitGroup
	for range 5 {
		burst.Go(func() {
			if resp, err := http.DefaultClient.Do(request(url+"/mcp", "chalet_at_read", initialize)); err != nil || resp.StatusCode != 200 {
				t.Errorf("initialize = %v, %v", resp, err)
			} else {
				resp.Body.Close()
			}
		})
	}
	time.Sleep(50 * time.Millisecond) // the burst reaches the exchange
	close(app.slow)
	burst.Wait()
	post(t, url+"/mcp", "chalet_at_read", initialize)
	if app.exchanged() != 1 {
		t.Fatalf("six requests for one grant exchanged %d times", app.exchanged())
	}

	post(t, url+"/mcp", "chalet_at_brief", initialize)
	post(t, url+"/mcp", "chalet_at_brief", initialize)
	if app.exchanged() != 3 {
		t.Errorf("a token with 5 seconds left was kept: %d exchanges", app.exchanged())
	}
}

// A token that dies during a call is dropped, and the call's error says to
// reconnect the app. The next request exchanges again, and a dead grant then
// gets the 401 that makes the client sign in.
func TestADeadTokenIsDropped(t *testing.T) {
	app := &fakeApp{}
	url := serve(t, "secret", app)

	session := connectAs(t, url+"/mcp", "chalet_at_lapsing")
	text, isError := mcptest.CallText(t, session, "chalet_my", map[string]any{"action": "work"})
	if got := failed(t, text); !isError || got.Code != "unauthorized" || !strings.Contains(got.Hint, "reconnects this app") || strings.Contains(got.Hint, "chalet auth login") {
		t.Fatalf("the call = %q", text)
	}
	if resp, _ := post(t, url+"/mcp", "chalet_at_lapsing", initialize); resp.StatusCode != 401 || !strings.Contains(resp.Header.Get("WWW-Authenticate"), `error="invalid_token"`) {
		t.Errorf("the next request = %d %q", resp.StatusCode, resp.Header.Get("WWW-Authenticate"))
	}
	if app.exchanged() != 2 {
		t.Errorf("exchanged %d times, want the first and one after the drop", app.exchanged())
	}
}

// The person's files are not on this server: a full path is refused, and
// nothing reaches the app, neither the call nor an upload.
func TestTakesNoLocalFiles(t *testing.T) {
	app := &fakeApp{catalog: `{"version":"1.0","rules":"Chalet.","domains":[{"key":"files","title":"Docs & Files","blurb":"Files."}],"operations":[
		{"domain":"files","name":"list","summary":"The files","effect":"read","method":"GET","path":"/files","params":[]},
		{"domain":"files","name":"upload","summary":"Put files up","effect":"write","method":"POST","path":"/files","params":[],
		 "body":{"type":"object","properties":{"signed_ids":{"type":"array","items":{"type":"string","format":"upload"}}}}}]}`}
	url := serve(t, "secret", app)

	session := connectAs(t, url+"/mcp", "chalet_at_write")
	text, isError := mcptest.CallText(t, session, "chalet_files_write", map[string]any{"action": "upload", "params": map[string]any{"signed_ids": []any{"/etc/hostname"}}})
	if got := failed(t, text); !isError || got.Code != "bad_request" || !strings.Contains(got.Error, "not on the computer with /etc/hostname") {
		t.Fatalf("upload = %q", text)
	}
	for _, r := range app.requests() {
		if r.Method == http.MethodPost {
			t.Errorf("%s reached the app", r.URL.Path)
		}
	}
}

// A call that panics on a grant's server answers an internal error, and the
// server answers the next one: one bad call does not stop it for everyone.
func TestAPanicIsOneCallsInternalError(t *testing.T) {
	app := &fakeApp{dead: map[string]bool{}}
	chalet := httptest.NewServer(app)
	t.Cleanup(chalet.Close)
	exchanger, err := exchange.New(exchange.Config{TokenURL: chalet.URL + "/oauth/token", Auth: exchange.ClientSecretPost("chalet-mcp-server", "secret")})
	if err != nil {
		t.Fatal(err)
	}
	var log bytes.Buffer
	h := &hosted{upstream: chalet.URL, exchanger: exchanger, version: "test", logger: slog.New(slog.NewTextHandler(&log, nil)), grants: map[[32]byte]*grant{}}

	g, err := h.grant(t.Context(), "chalet_at_read")
	if err != nil {
		t.Fatal(err)
	}
	mcp.AddTool(g.server.MCP, &mcp.Tool{Name: "boom"}, func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, any, error) {
		panic("boom")
	})
	session := mcptest.Connect(t, g.server.MCP)

	if _, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: "boom", Arguments: map[string]any{}}); err == nil || !strings.Contains(err.Error(), "internal error") {
		t.Fatalf("the panicking call = %v", err)
	}
	if text, isError := mcptest.CallText(t, session, "chalet_my", map[string]any{"action": "work"}); isError || !strings.Contains(text, "Call Anna") {
		t.Errorf("the next call = %q (error %v)", text, isError)
	}
	if !strings.Contains(log.String(), `msg="a call panicked" method=tools/call panic=boom`) {
		t.Errorf("log = %s", log.String())
	}
}
