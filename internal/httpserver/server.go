package httpserver

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"myhostmcp/internal/httpauth"
	"myhostmcp/internal/httpconfig"
	"myhostmcp/internal/protocol"
	"myhostmcp/internal/remote"
	"myhostmcp/internal/version"
)

type principalKey struct{}

// Server hosts the MCP streamable HTTP endpoint with token auth.
type Server struct {
	cfg  *httpconfig.Config
	auth *httpauth.Config
	log  *log.Logger

	mu    sync.Mutex
	users map[string]*userSessions

	mcpSrv  *mcp.Server
	handler http.Handler
}

type userSessions struct {
	sessions map[string]*execSession
	order    []string // creation order; last = default
}

type execSession struct {
	name   string
	owner  string
	mu     sync.Mutex
	nextID int64

	stdin  *io.PipeWriter
	stdout *bufio.Reader
	cancel context.CancelFunc
	done   <-chan error

	ready protocol.Response
	alive bool
}

func New(cfg *httpconfig.Config, auth *httpauth.Config) (*Server, error) {
	logger, err := openLogger(cfg.LogFile)
	if err != nil {
		return nil, err
	}
	s := &Server{
		cfg:   cfg,
		auth:  auth,
		log:   logger,
		users: map[string]*userSessions{},
	}
	s.mcpSrv = s.buildMCPServer()
	streamable := mcp.NewStreamableHTTPHandler(func(_ *http.Request) *mcp.Server {
		return s.mcpSrv
	}, &mcp.StreamableHTTPOptions{})
	s.handler = s.authMiddleware(streamable)
	return s, nil
}

func (s *Server) Handler() http.Handler { return s.handler }

func (s *Server) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, us := range s.users {
		for _, sess := range us.sessions {
			sess.close()
		}
	}
	s.users = map[string]*userSessions{}
}

func (s *Server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		username, ok := s.authenticateRequest(r)
		if !ok {
			s.authChallenge(w)
			return
		}
		ctx := context.WithValue(r.Context(), principalKey{}, username)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (s *Server) authenticateRequest(r *http.Request) (username string, ok bool) {
	if u, tok, hasBasic := r.BasicAuth(); hasBasic {
		if err := s.auth.AuthenticateBasic(u, tok); err == nil {
			return u, true
		}
		s.log.Printf("http auth failed (basic): user=%q remote=%s", u, r.RemoteAddr)
		return "", false
	}
	if tok, hasBearer := bearerToken(r.Header.Get("Authorization")); hasBearer {
		u, err := s.auth.AuthenticateBearer(tok)
		if err == nil {
			return u, true
		}
		s.log.Printf("http auth failed (bearer): remote=%s", r.RemoteAddr)
		return "", false
	}
	return "", false
}

func (s *Server) authChallenge(w http.ResponseWriter) {
	w.Header().Add("WWW-Authenticate", `Basic realm="myhostmcp"`)
	w.Header().Add("WWW-Authenticate", `Bearer realm="myhostmcp"`)
	http.Error(w, "unauthorized", http.StatusUnauthorized)
}

func (s *Server) buildMCPServer() *mcp.Server {
	srv := mcp.NewServer(&mcp.Implementation{Name: "myhostmcp-http", Version: version.Version}, nil)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "remote_connect",
		Description: "Open (or reuse) a persistent execution session on this host. The session stays open until remote_disconnect or process shutdown.",
	}, s.handleConnect)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "remote_exec",
		Description: "Run a shell command in the persistent session. cd/export/aliases persist across calls. Subject to the remote allowlist.",
	}, s.handleExec)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "remote_allowed_commands",
		Description: "Report the enforced allowlist for this authenticated user/session.",
	}, s.handleAllowed)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "remote_status",
		Description: "List sessions for the authenticated user and the default session.",
	}, s.handleStatus)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "remote_disconnect",
		Description: "Disconnect one session, default session, or all sessions (session=\"*\").",
	}, s.handleDisconnect)

	return srv
}

// ----- tool input/output types -------------------------------------------------

type ConnectInput struct {
	Host    string `json:"host,omitempty"`
	Session string `json:"session,omitempty"`
}

type ConnectOutput struct {
	Session          string     `json:"session"`
	Host             string     `json:"host"`
	User             string     `json:"user,omitempty"`
	RemotePID        int        `json:"remotePid"`
	RemoteVersion    string     `json:"remoteVersion"`
	HasTimeout       bool       `json:"hasTimeout"`
	AllowCommands    [][]string `json:"allowCommands,omitempty"`
	AlreadyConnected bool       `json:"alreadyConnected,omitempty"`
	Transport        string     `json:"transport,omitempty"`
}

type ExecInput struct {
	Command string `json:"command"`
	Session string `json:"session,omitempty"`
	CWD     string `json:"cwd,omitempty"`
	Timeout string `json:"timeout,omitempty"`
	PTY     bool   `json:"pty,omitempty"`
}

type ExecOutput struct {
	ExitCode   int    `json:"exitCode"`
	Stdout     string `json:"stdout"`
	Stderr     string `json:"stderr"`
	CWD        string `json:"cwd"`
	DurationMs int64  `json:"durationMs"`
	TimedOut   bool   `json:"timedOut"`
	Session    string `json:"session"`
}

type AllowedCommandsInput struct {
	Session string `json:"session,omitempty"`
}

type AllowedCommandsOutput struct {
	Session       string     `json:"session"`
	AllowCommands [][]string `json:"allowCommands"`
}

type StatusOutput struct {
	Default  string        `json:"default,omitempty"`
	Sessions []SessionInfo `json:"sessions"`
	Version  string        `json:"version"`
}

type SessionInfo struct {
	Name          string `json:"name"`
	Host          string `json:"host"`
	User          string `json:"user,omitempty"`
	RemotePID     int    `json:"remotePid"`
	RemoteVersion string `json:"remoteVersion"`
	HasTimeout    bool   `json:"hasTimeout"`
	Transport     string `json:"transport,omitempty"`
}

type DisconnectInput struct {
	Session string `json:"session,omitempty"`
}

type DisconnectOutput struct {
	Disconnected []string `json:"disconnected"`
}

// ----- handlers ----------------------------------------------------------------

func (s *Server) handleConnect(ctx context.Context, req *mcp.CallToolRequest, in ConnectInput) (*mcp.CallToolResult, ConnectOutput, error) {
	username, err := principalFrom(ctx, req)
	if err != nil {
		return s.errResult(err.Error(), ConnectOutput{}), ConnectOutput{}, nil
	}
	name := strings.TrimSpace(in.Session)
	if name == "" {
		name = "default"
	}
	if in.Host == "" {
		in.Host = "localhost"
	}

	s.mu.Lock()
	us := s.ensureUserLocked(username)
	if ex, ok := us.sessions[name]; ok {
		s.bumpDefault(us, name)
		s.mu.Unlock()
		allow, qerr := ex.allowedCommands()
		if qerr != nil {
			return s.errResult("query allowlist: "+qerr.Error(), ConnectOutput{}), ConnectOutput{}, nil
		}
		out := ConnectOutput{
			Session:          name,
			Host:             in.Host,
			User:             username,
			RemotePID:        ex.ready.PID,
			RemoteVersion:    ex.ready.Version,
			HasTimeout:       ex.ready.HasTimeout,
			AllowCommands:    allow,
			AlreadyConnected: true,
			Transport:        "local-http",
		}
		text := fmt.Sprintf("Session %q already connected for %s", name, username)
		return s.okResult(text, out), out, nil
	}
	s.mu.Unlock()

	sess, err := newExecSession(username, name, s.cfg.RemoteConfigPath, s.log)
	if err != nil {
		return s.errResult("connect failed: "+err.Error(), ConnectOutput{}), ConnectOutput{}, nil
	}
	allow, err := sess.allowedCommands()
	if err != nil {
		sess.close()
		return s.errResult("query allowlist: "+err.Error(), ConnectOutput{}), ConnectOutput{}, nil
	}

	s.mu.Lock()
	us = s.ensureUserLocked(username)
	us.sessions[name] = sess
	s.bumpDefault(us, name)
	s.mu.Unlock()

	out := ConnectOutput{
		Session:       name,
		Host:          in.Host,
		User:          username,
		RemotePID:     sess.ready.PID,
		RemoteVersion: sess.ready.Version,
		HasTimeout:    sess.ready.HasTimeout,
		AllowCommands: allow,
		Transport:     "local-http",
	}
	text := fmt.Sprintf("Connected session %q for %s (remote v%s pid=%d)", name, username, out.RemoteVersion, out.RemotePID)
	return s.okResult(text, out), out, nil
}

func (s *Server) handleExec(ctx context.Context, req *mcp.CallToolRequest, in ExecInput) (*mcp.CallToolResult, ExecOutput, error) {
	username, err := principalFrom(ctx, req)
	if err != nil {
		return s.errResult(err.Error(), ExecOutput{}), ExecOutput{}, nil
	}
	sess, name, err := s.session(username, in.Session)
	if err != nil {
		return s.errResult(err.Error(), ExecOutput{}), ExecOutput{}, nil
	}
	if strings.TrimSpace(in.Command) == "" {
		return s.errResult("command is required", ExecOutput{}), ExecOutput{}, nil
	}
	if in.PTY {
		return s.errResult("pty mode not implemented", ExecOutput{}), ExecOutput{}, nil
	}

	tmo := time.Duration(s.cfg.ExecTimeout)
	if in.Timeout != "" {
		d, perr := time.ParseDuration(in.Timeout)
		if perr != nil {
			return s.errResult("invalid timeout: "+perr.Error(), ExecOutput{}), ExecOutput{}, nil
		}
		tmo = d
	}

	resp, err := sess.exec(protocol.Request{
		Type:      "exec",
		Command:   in.Command,
		CWD:       in.CWD,
		TimeoutMs: int(tmo / time.Millisecond),
		PTY:       in.PTY,
	})
	if err != nil {
		return s.errResult("exec failed: "+err.Error(), ExecOutput{}), ExecOutput{}, nil
	}
	if resp.Type == "error" {
		return s.errResult(resp.Error, ExecOutput{}), ExecOutput{}, nil
	}
	out := ExecOutput{
		ExitCode:   resp.ExitCode,
		Stdout:     resp.Stdout,
		Stderr:     resp.Stderr,
		CWD:        resp.CWD,
		DurationMs: resp.DurationMs,
		TimedOut:   resp.TimedOut,
		Session:    name,
	}
	text := fmt.Sprintf("exit=%d timedOut=%v cwd=%q", out.ExitCode, out.TimedOut, out.CWD)
	return s.okResult(text, out), out, nil
}

func (s *Server) handleAllowed(ctx context.Context, req *mcp.CallToolRequest, in AllowedCommandsInput) (*mcp.CallToolResult, AllowedCommandsOutput, error) {
	username, err := principalFrom(ctx, req)
	if err != nil {
		return s.errResult(err.Error(), AllowedCommandsOutput{}), AllowedCommandsOutput{}, nil
	}
	sess, name, err := s.session(username, in.Session)
	if err != nil {
		return s.errResult(err.Error(), AllowedCommandsOutput{}), AllowedCommandsOutput{}, nil
	}
	allow, err := sess.allowedCommands()
	if err != nil {
		return s.errResult("query allowlist failed: "+err.Error(), AllowedCommandsOutput{}), AllowedCommandsOutput{}, nil
	}
	out := AllowedCommandsOutput{Session: name, AllowCommands: allow}
	text := fmt.Sprintf("%d allowed command(s) for session %q", len(allow), name)
	return s.okResult(text, out), out, nil
}

func (s *Server) handleStatus(ctx context.Context, req *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, StatusOutput, error) {
	username, err := principalFrom(ctx, req)
	if err != nil {
		return s.errResult(err.Error(), StatusOutput{}), StatusOutput{}, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	us := s.users[username]
	if us == nil {
		out := StatusOutput{Version: version.Version}
		return s.okResult("0 session(s)", out), out, nil
	}
	out := StatusOutput{Version: version.Version}
	if len(us.order) > 0 {
		out.Default = us.order[len(us.order)-1]
	}
	names := append([]string{}, us.order...)
	sort.Strings(names)
	for _, n := range names {
		sess, ok := us.sessions[n]
		if !ok {
			continue
		}
		out.Sessions = append(out.Sessions, SessionInfo{
			Name:          n,
			Host:          "localhost",
			User:          username,
			RemotePID:     sess.ready.PID,
			RemoteVersion: sess.ready.Version,
			HasTimeout:    sess.ready.HasTimeout,
			Transport:     "local-http",
		})
	}
	text := fmt.Sprintf("myhostmcp v%s. %d session(s). default=%q", out.Version, len(out.Sessions), out.Default)
	return s.okResult(text, out), out, nil
}

func (s *Server) handleDisconnect(ctx context.Context, req *mcp.CallToolRequest, in DisconnectInput) (*mcp.CallToolResult, DisconnectOutput, error) {
	username, err := principalFrom(ctx, req)
	if err != nil {
		return s.errResult(err.Error(), DisconnectOutput{}), DisconnectOutput{}, nil
	}
	s.mu.Lock()
	us := s.users[username]
	if us == nil {
		s.mu.Unlock()
		out := DisconnectOutput{}
		return s.okResult("no sessions", out), out, nil
	}
	want := strings.TrimSpace(in.Session)
	if want == "" && len(us.order) > 0 {
		want = us.order[len(us.order)-1]
	}
	var toClose []string
	if want == "*" {
		toClose = append(toClose, us.order...)
	} else if want != "" {
		if _, ok := us.sessions[want]; ok {
			toClose = append(toClose, want)
		}
	}
	closed := make([]*execSession, 0, len(toClose))
	for _, n := range toClose {
		if sess, ok := us.sessions[n]; ok {
			closed = append(closed, sess)
			delete(us.sessions, n)
		}
	}
	if len(toClose) > 0 {
		us.order = filterOrder(us.order, toClose)
	}
	if len(us.sessions) == 0 {
		delete(s.users, username)
	}
	s.mu.Unlock()

	for _, sess := range closed {
		sess.close()
	}
	out := DisconnectOutput{Disconnected: toClose}
	text := fmt.Sprintf("disconnected %d session(s)", len(toClose))
	return s.okResult(text, out), out, nil
}

func (s *Server) ensureUserLocked(username string) *userSessions {
	us := s.users[username]
	if us == nil {
		us = &userSessions{sessions: map[string]*execSession{}}
		s.users[username] = us
	}
	return us
}

func (s *Server) bumpDefault(us *userSessions, name string) {
	for i, n := range us.order {
		if n == name {
			us.order = append(us.order[:i], us.order[i+1:]...)
			break
		}
	}
	us.order = append(us.order, name)
}

func (s *Server) session(username, name string) (*execSession, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	us := s.users[username]
	if us == nil || len(us.sessions) == 0 {
		return nil, "", fmt.Errorf("no open sessions for %s", username)
	}
	if strings.TrimSpace(name) == "" {
		name = us.order[len(us.order)-1]
	}
	sess, ok := us.sessions[name]
	if !ok {
		return nil, "", fmt.Errorf("session %q not found", name)
	}
	return sess, name, nil
}

func principalFrom(ctx context.Context, req *mcp.CallToolRequest) (string, error) {
	if v, ok := ctx.Value(principalKey{}).(string); ok && v != "" {
		return v, nil
	}
	if req != nil && req.GetExtra() != nil {
		if u, _, ok := parseBasicAuthHeader(req.GetExtra().Header.Get("Authorization")); ok && u != "" {
			return u, nil
		}
	}
	return "", fmt.Errorf("missing authenticated principal")
}

func parseBasicAuthHeader(v string) (username, token string, ok bool) {
	const prefix = "Basic "
	if !strings.HasPrefix(v, prefix) {
		return "", "", false
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(v[len(prefix):]))
	if err != nil {
		return "", "", false
	}
	parts := strings.SplitN(string(raw), ":", 2)
	if len(parts) != 2 {
		return "", "", false
	}
	return parts[0], parts[1], true
}

func bearerToken(v string) (token string, ok bool) {
	const prefix = "Bearer "
	if !strings.HasPrefix(v, prefix) {
		return "", false
	}
	tok := strings.TrimSpace(v[len(prefix):])
	if tok == "" {
		return "", false
	}
	return tok, true
}

func (s *Server) okResult(text string, out any) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}, StructuredContent: out}
}

func (s *Server) errResult(msg string, _ any) *mcp.CallToolResult {
	return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: msg}}}
}

func openLogger(path string) (*log.Logger, error) {
	if strings.TrimSpace(path) == "" {
		return log.New(os.Stderr, "myhostmcp-http: ", log.LstdFlags|log.Lmicroseconds), nil
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	return log.New(f, "myhostmcp-http: ", log.LstdFlags|log.Lmicroseconds), nil
}

func filterOrder(order []string, remove []string) []string {
	rm := map[string]struct{}{}
	for _, n := range remove {
		rm[n] = struct{}{}
	}
	out := order[:0]
	for _, n := range order {
		if _, ok := rm[n]; !ok {
			out = append(out, n)
		}
	}
	return out
}

func newExecSession(owner, name, remoteConfigPath string, logger *log.Logger) (*execSession, error) {
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	e, err := remote.New(remote.Config{ConfigPath: remoteConfigPath}, inR, outW, logWriter{log: logger, prefix: fmt.Sprintf("[%s/%s remote] ", owner, name)})
	if err != nil {
		_ = inW.Close()
		_ = outR.Close()
		_ = outW.Close()
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	doneCh := make(chan error, 1)
	go func() {
		defer close(doneCh)
		defer outW.Close()
		doneCh <- e.Run(ctx)
	}()

	sess := &execSession{
		name:   name,
		owner:  owner,
		stdin:  inW,
		stdout: bufio.NewReader(outR),
		cancel: cancel,
		done:   doneCh,
		alive:  true,
	}
	ready, err := sess.readResp()
	if err != nil {
		sess.close()
		return nil, fmt.Errorf("wait for ready: %w", err)
	}
	if ready.Type != "log" || ready.Msg != "ready" {
		sess.close()
		return nil, fmt.Errorf("unexpected startup response: type=%q msg=%q", ready.Type, ready.Msg)
	}
	sess.ready = ready
	return sess, nil
}

func (s *execSession) exec(req protocol.Request) (protocol.Response, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.alive {
		return protocol.Response{}, fmt.Errorf("session closed")
	}
	s.nextID++
	req.ID = s.nextID
	if err := s.writeReq(req); err != nil {
		s.alive = false
		return protocol.Response{}, err
	}
	for {
		resp, err := s.readResp()
		if err != nil {
			s.alive = false
			return protocol.Response{}, err
		}
		if resp.ID == req.ID {
			return resp, nil
		}
	}
}

func (s *execSession) allowedCommands() ([][]string, error) {
	resp, err := s.exec(protocol.Request{Type: "allowed_commands"})
	if err != nil {
		return nil, err
	}
	if resp.Type == "error" {
		return nil, fmt.Errorf("%s", resp.Error)
	}
	return resp.AllowCommands, nil
}

func (s *execSession) close() {
	s.mu.Lock()
	if !s.alive {
		s.mu.Unlock()
		return
	}
	s.alive = false
	s.nextID++
	_ = s.writeReq(protocol.Request{ID: s.nextID, Type: "shutdown"})
	_ = s.stdin.Close()
	s.cancel()
	s.mu.Unlock()

	select {
	case <-s.done:
	case <-time.After(2 * time.Second):
	}
}

func (s *execSession) writeReq(req protocol.Request) error {
	b, err := json.Marshal(req)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	_, err = s.stdin.Write(b)
	if err != nil {
		return fmt.Errorf("write request: %w", err)
	}
	return nil
}

func (s *execSession) readResp() (protocol.Response, error) {
	for {
		line, err := s.stdout.ReadString('\n')
		if err != nil {
			return protocol.Response{}, fmt.Errorf("read response: %w", err)
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var resp protocol.Response
		if err := json.Unmarshal([]byte(line), &resp); err != nil {
			continue
		}
		return resp, nil
	}
}

type logWriter struct {
	log    *log.Logger
	prefix string
}

func (w logWriter) Write(p []byte) (int, error) {
	for _, line := range strings.Split(strings.TrimRight(string(p), "\n"), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		w.log.Printf("%s%s", w.prefix, line)
	}
	return len(p), nil
}
