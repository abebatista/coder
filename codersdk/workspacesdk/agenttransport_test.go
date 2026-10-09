package workspacesdk_test

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"net/netip"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/coder/coder/v2/codersdk/workspacesdk"
	"github.com/coder/coder/v2/tailnet"
	"github.com/coder/coder/v2/testutil"
)

// serverDialer dials srv whatever port it is asked for, and records the
// last port.
func serverDialer(srv *httptest.Server, port *atomic.Int32) workspacesdk.AgentDialer {
	return func(ctx context.Context, p uint16) (net.Conn, error) {
		port.Store(int32(p))
		var d net.Dialer
		return d.DialContext(ctx, "tcp", srv.Listener.Addr().String())
	}
}

func okServer(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func roundTrip(ctx context.Context, rt http.RoundTripper, rawURL string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return err
	}
	res, err := rt.RoundTrip(req)
	if err != nil {
		return err
	}
	_, _ = io.Copy(io.Discard, res.Body)
	return res.Body.Close()
}

func addrURL(addr netip.Addr, port string) string {
	return "http://" + net.JoinHostPort(addr.String(), port) + "/"
}

func anyPort(uint16) bool { return true }

var testAPIPool = workspacesdk.AgentAPIPool{MaxIdleConns: 1, IdleConnTimeout: time.Minute}

func mustAgentTransport(t *testing.T, dial workspacesdk.AgentDialer, opts workspacesdk.AgentTransportOptions) *workspacesdk.AgentTransport {
	t.Helper()
	tr, err := workspacesdk.NewAgentTransport(uuid.New(), dial, testutil.Logger(t), opts)
	require.NoError(t, err)
	t.Cleanup(tr.CloseIdleConnections)
	return tr
}

// gotConn sends req and reports whether it reused a connection.
func gotConn(t *testing.T, rt http.RoundTripper, req *http.Request) (reused bool) {
	t.Helper()
	req = req.WithContext(httptrace.WithClientTrace(req.Context(), &httptrace.ClientTrace{
		GotConn: func(info httptrace.GotConnInfo) { reused = info.Reused },
	}))
	res, err := rt.RoundTrip(req)
	require.NoError(t, err)
	_, _ = io.Copy(io.Discard, res.Body)
	require.NoError(t, res.Body.Close())
	return reused
}

func TestAgentTransport(t *testing.T) {
	t.Parallel()

	t.Run("RejectsOtherHosts", func(t *testing.T) {
		t.Parallel()

		ctx := testutil.Context(t, testutil.WaitShort)
		var port atomic.Int32
		tr := mustAgentTransport(t, serverDialer(okServer(t), &port), workspacesdk.AgentTransportOptions{AllowPort: anyPort})

		otherAgent := tailnet.TailscaleServicePrefix.AddrFromUUID(uuid.New())
		require.ErrorContains(t, roundTrip(ctx, tr, addrURL(otherAgent, "8080")), "does not match agent")
		require.ErrorContains(t, roundTrip(ctx, tr, "http://localhost:8080/"), "does not match agent")
		require.Zero(t, port.Load(), "rejected request must not be dialed")
	})

	t.Run("PortRule", func(t *testing.T) {
		t.Parallel()

		ctx := testutil.Context(t, testutil.WaitShort)
		var port atomic.Int32
		tr := mustAgentTransport(t, serverDialer(okServer(t), &port), workspacesdk.AgentTransportOptions{
			AllowPort: func(p uint16) bool { return p == 8080 },
		})

		require.ErrorContains(t, roundTrip(ctx, tr, addrURL(tr.Addr(), "8081")), "port 8081 is not allowed")
		require.Zero(t, port.Load(), "rejected port must not be dialed")
		require.NoError(t, roundTrip(ctx, tr, addrURL(tr.Addr(), "8080")))
		require.EqualValues(t, 8080, port.Load())
	})

	t.Run("NoPortUsesSchemeDefault", func(t *testing.T) {
		t.Parallel()

		ctx := testutil.Context(t, testutil.WaitShort)
		var port atomic.Int32
		tr := mustAgentTransport(t, serverDialer(okServer(t), &port), workspacesdk.AgentTransportOptions{
			AllowPort: func(p uint16) bool { return p == 80 },
		})

		// The app reverse proxy produces "[addr]:" for an app URL without a port.
		require.NoError(t, roundTrip(ctx, tr, addrURL(tr.Addr(), "")))
		require.EqualValues(t, 80, port.Load())
	})

	t.Run("IdleConnections", func(t *testing.T) {
		t.Parallel()

		ctx := testutil.Context(t, testutil.WaitShort)
		dial := serverDialer(okServer(t), &atomic.Int32{})
		for _, tc := range []struct {
			maxIdle int
			reused  bool
		}{
			{maxIdle: 0, reused: false},
			{maxIdle: 1, reused: true},
		} {
			tr := mustAgentTransport(t, dial, workspacesdk.AgentTransportOptions{
				AllowPort:           anyPort,
				MaxIdleConnsPerPort: tc.maxIdle,
				IdleConnTimeout:     time.Minute,
			})
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, addrURL(tr.Addr(), "8080"), nil)
			require.NoError(t, err)
			require.False(t, gotConn(t, tr, req))
			require.Equal(t, tc.reused, gotConn(t, tr, req), "max idle %d", tc.maxIdle)
		}
	})

	t.Run("TLSAndHTTP2", func(t *testing.T) {
		t.Parallel()

		ctx := testutil.Context(t, testutil.WaitShort)
		srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		srv.EnableHTTP2 = true
		srv.StartTLS()
		t.Cleanup(srv.Close)
		dial := serverDialer(srv, &atomic.Int32{})
		insecure := &tls.Config{InsecureSkipVerify: true} //nolint:gosec // Test server certificate.

		protoMajor := func(opts workspacesdk.AgentTransportOptions) (int, error) {
			opts.AllowPort = anyPort
			tr := mustAgentTransport(t, dial, opts)
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+net.JoinHostPort(tr.Addr().String(), "8443")+"/", nil)
			require.NoError(t, err)
			res, err := tr.RoundTrip(req)
			if err != nil {
				return 0, err
			}
			defer res.Body.Close()
			return res.ProtoMajor, nil
		}

		_, err := protoMajor(workspacesdk.AgentTransportOptions{})
		require.ErrorContains(t, err, "certificate", "the default TLS config verifies certificates")
		major, err := protoMajor(workspacesdk.AgentTransportOptions{TLSClientConfig: insecure})
		require.NoError(t, err)
		require.Equal(t, 1, major)
		major, err = protoMajor(workspacesdk.AgentTransportOptions{TLSClientConfig: insecure.Clone(), HTTP2: true})
		require.NoError(t, err)
		require.Equal(t, 2, major)
	})

	t.Run("CanceledRequestCancelsDial", func(t *testing.T) {
		t.Parallel()

		ctx := testutil.Context(t, testutil.WaitShort)
		dialStarted := make(chan struct{})
		dialEnded := make(chan struct{})
		// With idle connections, http.Transport detaches the dial from its
		// request.
		tr := mustAgentTransport(t, func(ctx context.Context, _ uint16) (net.Conn, error) {
			close(dialStarted)
			<-ctx.Done() // An unreachable agent.
			close(dialEnded)
			return nil, ctx.Err()
		}, workspacesdk.AgentTransportOptions{AllowPort: anyPort, MaxIdleConnsPerPort: 1, IdleConnTimeout: time.Minute})

		reqCtx, cancel := context.WithCancel(ctx)
		go func() {
			<-dialStarted
			cancel()
		}()
		require.Error(t, roundTrip(reqCtx, tr, addrURL(tr.Addr(), "8080")))
		testutil.TryReceive(ctx, t, dialEnded)
	})

	t.Run("InvalidOptions", func(t *testing.T) {
		t.Parallel()

		dial := serverDialer(okServer(t), &atomic.Int32{})
		for _, tc := range []struct {
			opts workspacesdk.AgentTransportOptions
			err  string
		}{
			{opts: workspacesdk.AgentTransportOptions{}, err: "port rule is required"},
			{opts: workspacesdk.AgentTransportOptions{AllowPort: anyPort, MaxIdleConnsPerPort: -1}, err: "must not be negative"},
			{opts: workspacesdk.AgentTransportOptions{AllowPort: anyPort, MaxIdleConnsPerPort: 1}, err: "must be positive"},
		} {
			_, err := workspacesdk.NewAgentTransport(uuid.New(), dial, testutil.Logger(t), tc.opts)
			require.ErrorContains(t, err, tc.err)
		}
	})
}

func TestAgentAPITransport(t *testing.T) {
	t.Parallel()

	apiPort := strconv.Itoa(workspacesdk.AgentHTTPAPIServerPort)

	t.Run("RejectsOtherPorts", func(t *testing.T) {
		t.Parallel()

		ctx := testutil.Context(t, testutil.WaitShort)
		var port atomic.Int32
		tr, err := workspacesdk.NewAgentAPITransport(uuid.New(), serverDialer(okServer(t), &port), testutil.Logger(t), testAPIPool)
		require.NoError(t, err)
		t.Cleanup(tr.CloseIdleConnections)

		require.ErrorContains(t, roundTrip(ctx, tr, addrURL(tr.Addr(), "80")), "port 80 is not allowed")
		require.Zero(t, port.Load(), "rejected port must not be dialed")
	})

	// Only requests that http.Transport resends when a reused connection
	// fails may use a pooled connection.
	t.Run("PoolsOnlyResendableRequests", func(t *testing.T) {
		t.Parallel()

		for _, tc := range []struct {
			name    string
			method  string
			body    func() io.Reader
			header  http.Header
			reused  bool
			keySent bool
		}{
			{name: "GET", method: http.MethodGet, reused: true},
			{name: "POST", method: http.MethodPost},
			{name: "POSTWithKey", method: http.MethodPost, header: http.Header{"Idempotency-Key": {"k"}}, reused: true, keySent: true},
			{name: "POSTWithXKey", method: http.MethodPost, header: http.Header{"X-Idempotency-Key": {"k"}}, reused: true},
			{name: "POSTWithEmptyKey", method: http.MethodPost, header: http.Header{"Idempotency-Key": nil}, reused: true},
			{
				name:    "POSTWithKeyAndRewindableBody",
				method:  http.MethodPost,
				body:    func() io.Reader { return strings.NewReader("x") },
				header:  http.Header{"Idempotency-Key": {"k"}},
				reused:  true,
				keySent: true,
			},
			{
				name:    "POSTWithKeyAndOneShotBody",
				method:  http.MethodPost,
				body:    func() io.Reader { return struct{ io.Reader }{strings.NewReader("x")} },
				header:  http.Header{"Idempotency-Key": {"k"}},
				keySent: true,
			},
		} {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()

				ctx := testutil.Context(t, testutil.WaitShort)
				var keySent atomic.Bool
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					_, _ = io.Copy(io.Discard, r.Body)
					keySent.Store(len(r.Header.Values("Idempotency-Key")) > 0)
					w.WriteHeader(http.StatusOK)
				}))
				t.Cleanup(srv.Close)
				tr, err := workspacesdk.NewAgentAPITransport(uuid.New(), serverDialer(srv, &atomic.Int32{}), testutil.Logger(t), testAPIPool)
				require.NoError(t, err)
				t.Cleanup(tr.CloseIdleConnections)
				get, err := http.NewRequestWithContext(ctx, http.MethodGet, addrURL(tr.Addr(), apiPort), nil)
				require.NoError(t, err)
				require.False(t, gotConn(t, tr, get), "the first request dials")

				var body io.Reader
				if tc.body != nil {
					body = tc.body()
				}
				req, err := http.NewRequestWithContext(ctx, tc.method, addrURL(tr.Addr(), apiPort), body)
				require.NoError(t, err)
				for k, v := range tc.header {
					req.Header[k] = v
				}
				require.Equal(t, tc.reused, gotConn(t, tr, req), "reused a pooled connection")
				require.Equal(t, tc.keySent, keySent.Load(), "sent Idempotency-Key")
			})
		}
	})

	t.Run("NoPool", func(t *testing.T) {
		t.Parallel()

		ctx := testutil.Context(t, testutil.WaitShort)
		tr, err := workspacesdk.NewAgentAPITransport(uuid.New(), serverDialer(okServer(t), &atomic.Int32{}), testutil.Logger(t), workspacesdk.AgentAPIPool{})
		require.NoError(t, err)

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, addrURL(tr.Addr(), apiPort), nil)
		require.NoError(t, err)
		require.False(t, gotConn(t, tr, req))
		require.False(t, gotConn(t, tr, req))
	})

	t.Run("InvalidPool", func(t *testing.T) {
		t.Parallel()

		dial := serverDialer(okServer(t), &atomic.Int32{})
		_, err := workspacesdk.NewAgentAPITransport(uuid.New(), dial, testutil.Logger(t), workspacesdk.AgentAPIPool{MaxIdleConns: -1})
		require.ErrorContains(t, err, "must not be negative")
		_, err = workspacesdk.NewAgentAPITransport(uuid.New(), dial, testutil.Logger(t), workspacesdk.AgentAPIPool{MaxIdleConns: 1})
		require.ErrorContains(t, err, "must be positive")
	})
}

// dropIdleListener closes a connection when a request arrives on it after
// a response was written, like an agent that restarted while the
// connection sat idle.
type dropIdleListener struct{ net.Listener }

func (l dropIdleListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return &dropIdleConn{Conn: c}, nil
}

type dropIdleConn struct {
	net.Conn
	responded atomic.Bool
}

func (c *dropIdleConn) Write(p []byte) (int, error) {
	c.responded.Store(true)
	return c.Conn.Write(p)
}

func (c *dropIdleConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if n > 0 && c.responded.Load() {
		_ = c.Close()
		return 0, io.EOF
	}
	return n, err
}

// TestAgentConn_DroppedIdleConnection checks that no request fails when the
// agent closes a pooled connection: resendable requests are resent on a new
// connection, and other requests never use a pooled one.
func TestAgentConn_DroppedIdleConnection(t *testing.T) {
	t.Parallel()

	toolCall := func(ctx context.Context) context.Context {
		return workspacesdk.WithToolCallID(ctx, uuid.New())
	}
	for _, tc := range []struct {
		name   string
		call   func(context.Context, workspacesdk.AgentConn) error
		pooled bool
	}{
		{
			name: "ToolCall",
			call: func(ctx context.Context, conn workspacesdk.AgentConn) error {
				return conn.WriteFile(toolCall(ctx), "/tmp/f", strings.NewReader("x"))
			},
			pooled: true,
		},
		{
			name: "ToolCallOneShotBody",
			call: func(ctx context.Context, conn workspacesdk.AgentConn) error {
				return conn.WriteFile(toolCall(ctx), "/tmp/f", struct{ io.Reader }{strings.NewReader("x")})
			},
		},
		{
			name: "CancelToolCall",
			call: func(ctx context.Context, conn workspacesdk.AgentConn) error {
				_, err := conn.CancelToolCall(ctx, uuid.New())
				return err
			},
			pooled: true,
		},
		{
			name: "LS",
			call: func(ctx context.Context, conn workspacesdk.AgentConn) error {
				_, err := conn.LS(ctx, "", workspacesdk.LSRequest{})
				return err
			},
			pooled: true,
		},
		{
			name: "NoToolCall",
			call: func(ctx context.Context, conn workspacesdk.AgentConn) error {
				return conn.WriteFile(ctx, "/tmp/f", strings.NewReader("x"))
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ctx := testutil.Context(t, testutil.WaitShort)
			var handled atomic.Int32
			srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				handled.Add(1)
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{}`))
			}))
			srv.Listener = dropIdleListener{srv.Listener}
			srv.Start()
			t.Cleanup(srv.Close)

			agentID := uuid.New()
			tr, err := workspacesdk.NewAgentAPITransport(agentID, serverDialer(srv, &atomic.Int32{}), testutil.Logger(t), testAPIPool)
			require.NoError(t, err)
			t.Cleanup(tr.CloseIdleConnections)
			conn := workspacesdk.NewAgentConn(nil, workspacesdk.AgentConnOptions{
				AgentID:      agentID,
				APITransport: tr,
			})

			// Leaves a pooled connection, which the server drops when the
			// next request arrives on it.
			_, err = conn.ListeningPorts(ctx)
			require.NoError(t, err)
			var reused atomic.Bool
			traceCtx := httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
				GotConn: func(info httptrace.GotConnInfo) {
					if info.Reused {
						reused.Store(true)
					}
				},
			})
			require.NoError(t, tc.call(traceCtx, conn))
			require.NoError(t, tc.call(traceCtx, conn))
			require.EqualValues(t, 3, handled.Load())
			require.Equal(t, tc.pooled, reused.Load(), "tried a pooled connection")
		})
	}
}
