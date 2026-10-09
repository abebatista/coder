package workspacesdk

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"time"

	"github.com/google/uuid"
	"golang.org/x/xerrors"

	"cdr.dev/slog/v3"
	"github.com/coder/coder/v2/tailnet"
)

// AgentDialer opens a TCP connection to port on the agent a transport is
// bound to.
type AgentDialer func(ctx context.Context, port uint16) (net.Conn, error)

// AgentTransport sends HTTP requests to one agent. It rejects requests for
// any other host, and dials only ports its port rule allows.
type AgentTransport struct {
	agentID   uuid.UUID
	addr      netip.Addr
	logger    slog.Logger
	transport *http.Transport
}

// AgentTransportOptions configures an AgentTransport.
type AgentTransportOptions struct {
	// AllowPort reports whether a port may be dialed. It must give the same
	// answer for a port every time: pooled connections are reused without
	// asking again.
	AllowPort           func(port uint16) bool
	MaxIdleConnsPerPort int           // Per port and scheme. Zero keeps no idle connections.
	IdleConnTimeout     time.Duration // Required if MaxIdleConnsPerPort is positive.
	TLSClientConfig     *tls.Config
	HTTP2               bool // Negotiate HTTP/2 over TLS.
}

// NewAgentTransport returns a transport for agentID. dial must connect to
// that agent.
func NewAgentTransport(agentID uuid.UUID, dial AgentDialer, logger slog.Logger, opts AgentTransportOptions) (*AgentTransport, error) {
	if opts.AllowPort == nil {
		return nil, xerrors.New("port rule is required")
	}
	if opts.MaxIdleConnsPerPort < 0 {
		return nil, xerrors.Errorf("max idle connections %d must not be negative", opts.MaxIdleConnsPerPort)
	}
	if opts.MaxIdleConnsPerPort > 0 && opts.IdleConnTimeout <= 0 {
		return nil, xerrors.Errorf("idle connection timeout %s must be positive to keep idle connections", opts.IdleConnTimeout)
	}
	return buildAgentTransport(agentID, dial, logger, opts), nil
}

func buildAgentTransport(agentID uuid.UUID, dial AgentDialer, logger slog.Logger, opts AgentTransportOptions) *AgentTransport {
	return &AgentTransport{
		agentID: agentID,
		addr:    tailnet.TailscaleServicePrefix.AddrFromUUID(agentID),
		logger:  logger,
		transport: &http.Transport{
			DialContext:           dialAgentPort(dial, opts.AllowPort),
			DisableKeepAlives:     opts.MaxIdleConnsPerPort == 0,
			MaxIdleConnsPerHost:   opts.MaxIdleConnsPerPort,
			IdleConnTimeout:       opts.IdleConnTimeout,
			TLSClientConfig:       opts.TLSClientConfig,
			ForceAttemptHTTP2:     opts.HTTP2,
			TLSHandshakeTimeout:   10 * time.Second, // As http.DefaultTransport.
			ExpectContinueTimeout: time.Second,      // As http.DefaultTransport.
		},
	}
}

// Addr returns the agent's address. Request URLs must use it as their host.
func (t *AgentTransport) Addr() netip.Addr {
	return t.addr
}

type dialRequestContextKey struct{}

func (t *AgentTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// The dialer only receives the port, so without this check a request for
	// another host would silently reach this agent.
	if addr, err := netip.ParseAddr(req.URL.Hostname()); err != nil || addr != t.addr {
		if req.Body != nil {
			_ = req.Body.Close() // Required by the http.RoundTripper contract.
		}
		t.logger.Warn(req.Context(), "blocked workspace agent request to unintended host",
			slog.F("agent_id", t.agentID),
			slog.F("request_host", req.URL.Host),
			slog.F("agent_addr", t.addr),
		)
		return nil, xerrors.Errorf("request host %q does not match agent %s address %s", req.URL.Host, t.agentID, t.addr)
	}
	ctx := context.WithValue(req.Context(), dialRequestContextKey{}, req.Context())
	return t.transport.RoundTrip(req.WithContext(ctx))
}

func (t *AgentTransport) CloseIdleConnections() {
	t.transport.CloseIdleConnections()
}

// AgentAPIPool sets the idle connections an AgentAPITransport keeps. The zero
// value keeps none.
type AgentAPIPool struct {
	MaxIdleConns    int           // Per agent.
	IdleConnTimeout time.Duration // Required if MaxIdleConns is positive.
}

// AgentAPITransport sends requests to one agent's HTTP API server. It reuses
// idle connections only for requests that http.Transport resends on a new
// connection when a reused one fails (see resendable). Other requests use a
// new connection each, so the agent closing an idle connection cannot fail
// them.
type AgentAPITransport struct {
	pooled   *AgentTransport
	unpooled *AgentTransport // Same as pooled if pooled keeps no idle connections.
}

// NewAgentAPITransport returns a transport for the HTTP API server of
// agentID. dial must connect to that agent.
func NewAgentAPITransport(agentID uuid.UUID, dial AgentDialer, logger slog.Logger, pool AgentAPIPool) (*AgentAPITransport, error) {
	pooled, err := NewAgentTransport(agentID, dial, logger, AgentTransportOptions{
		AllowPort:           isAgentAPIPort,
		MaxIdleConnsPerPort: pool.MaxIdleConns,
		IdleConnTimeout:     pool.IdleConnTimeout,
	})
	if err != nil {
		return nil, err
	}
	unpooled := pooled
	if pool.MaxIdleConns > 0 {
		unpooled = buildAgentTransport(agentID, dial, logger, AgentTransportOptions{AllowPort: isAgentAPIPort})
	}
	return &AgentAPITransport{pooled: pooled, unpooled: unpooled}, nil
}

func newUnpooledAPITransport(agentID uuid.UUID, dial AgentDialer, logger slog.Logger) *AgentAPITransport {
	t := buildAgentTransport(agentID, dial, logger, AgentTransportOptions{AllowPort: isAgentAPIPort})
	return &AgentAPITransport{pooled: t, unpooled: t}
}

func isAgentAPIPort(port uint16) bool {
	return port == AgentHTTPAPIServerPort
}

// Addr returns the agent's address. Request URLs must use it as their host.
func (t *AgentAPITransport) Addr() netip.Addr {
	return t.pooled.Addr()
}

func (t *AgentAPITransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if resendable(req) {
		return t.pooled.RoundTrip(req)
	}
	return t.unpooled.RoundTrip(req)
}

func (t *AgentAPITransport) CloseIdleConnections() {
	t.pooled.CloseIdleConnections()
}

// resendable reports whether http.Transport resends req on a new connection
// when a reused connection fails before the response arrives: the method is
// idempotent or an Idempotency-Key or X-Idempotency-Key header is present,
// even with no value, and the body is empty or has GetBody. This is the rule
// of the unexported (*http.Request).isReplayable.
func resendable(req *http.Request) bool {
	if req.Body != nil && req.Body != http.NoBody && req.GetBody == nil {
		return false
	}
	switch req.Method {
	case "", http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodTrace:
		return true
	}
	_, key := req.Header["Idempotency-Key"]
	_, xKey := req.Header["X-Idempotency-Key"]
	return key || xKey
}

// dialAgentPort dials the port of addr if allowed. http.Transport has already
// replaced an empty port with the scheme's default. The host of addr is
// checked in RoundTrip.
func dialAgentPort(dial AgentDialer, allowed func(port uint16) bool) func(ctx context.Context, network, addr string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		if network != "tcp" {
			return nil, xerrors.Errorf("network %q is not tcp", network)
		}
		_, rawPort, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, xerrors.Errorf("split host port %q: %w", addr, err)
		}
		port, err := strconv.ParseUint(rawPort, 10, 16)
		if err != nil {
			return nil, xerrors.Errorf("parse port %q: %w", rawPort, err)
		}
		if !allowed(uint16(port)) {
			return nil, xerrors.Errorf("port %d is not allowed", port)
		}
		// http.Transport keeps a dial running after its request ends, so a
		// later request can use the connection. End it with the request:
		// a dial to an unreachable agent otherwise waits until the agent
		// answers, which may be never.
		if reqCtx, ok := ctx.Value(dialRequestContextKey{}).(context.Context); ok {
			var cancel context.CancelFunc
			ctx, cancel = context.WithCancel(ctx)
			defer cancel()
			stop := context.AfterFunc(reqCtx, cancel)
			defer stop()
		}
		return dial(ctx, uint16(port))
	}
}
