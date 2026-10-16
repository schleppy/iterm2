package iterm2

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"google.golang.org/protobuf/proto"

	pb "github.com/schleppy/iterm2/internal/iterm2pb"
)

// LibraryVersion is sent to iTerm2 in the x-iterm2-library-version header.
// iTerm2 enforces a minimum version only for "python" clients.
const LibraryVersion = "go 1.0"

const (
	subprotocol        = "api.iterm2.com"
	defaultAppName     = "iterm2-go"
	defaultEventBuffer = 256
	writeTimeout       = 10 * time.Second
	readLimit          = 256 << 20
	protocolVersionHdr = "X-iTerm2-Protocol-Version"
)

var (
	// ErrNotRunning means the API socket does not exist or nothing is
	// listening on it: iTerm2 is not running, or its API server is disabled
	// (Settings > General > Magic > Enable Python API).
	ErrNotRunning = errors.New("iterm2: iTerm2 is not running or its API is disabled")

	// ErrUnauthorized means iTerm2 rejected the connection with HTTP 401.
	ErrUnauthorized = errors.New("iterm2: connection not authorized")

	// ErrClosed is returned for calls made after the client was closed.
	ErrClosed = errors.New("iterm2: client closed")
)

// ConnectionLostError reports that the connection ended other than through
// Close.
type ConnectionLostError struct{ Err error }

func (e *ConnectionLostError) Error() string { return "iterm2: connection lost: " + e.Err.Error() }
func (e *ConnectionLostError) Unwrap() error { return e.Err }

// RPCError means iTerm2 rejected a request as malformed.
type RPCError struct{ Message string }

func (e *RPCError) Error() string { return "iterm2: rpc error: " + e.Message }

// Options configures Dial. The zero value is usable.
type Options struct {
	// SocketPath overrides the API socket path. By default it is
	// ~/Library/Application Support/$IT2_SUITE/private/socket, where
	// IT2_SUITE defaults to "iTerm2".
	SocketPath string

	// AppName is shown in iTerm2's Script Console and in the AppleScript
	// cookie request. Defaults to "iterm2-go".
	AppName string

	// Authenticate obtains credentials before each connection attempt.
	// Defaults to AppleScriptAuth. If it returns an error other than
	// ErrNotRunning, Dial still tries to connect without a cookie, which
	// works when "Allow all apps to connect" is enabled in iTerm2.
	Authenticate Authenticator

	// EventBuffer is the capacity of the Events channel. Defaults to 256.
	EventBuffer int
}

// transport carries whole binary messages. It is satisfied by a WebSocket
// connection and, in tests, by an in-memory fake.
type transport interface {
	Read(ctx context.Context) ([]byte, error)
	Write(ctx context.Context, p []byte) error
	Close() error
}

type wsTransport struct{ c *websocket.Conn }

func (t wsTransport) Read(ctx context.Context) ([]byte, error) {
	_, p, err := t.c.Read(ctx)
	return p, err
}

func (t wsTransport) Write(ctx context.Context, p []byte) error {
	return t.c.Write(ctx, websocket.MessageBinary, p)
}

func (t wsTransport) Close() error { return t.c.Close(websocket.StatusNormalClosure, "") }

// response is a server message tagged with its arrival order.
type response struct {
	msg *pb.ServerOriginatedMessage
	seq uint64
}

// Client is a connection to iTerm2. All methods are safe for concurrent use.
// A Client does not reconnect. Once Done is closed, create a new one (Watcher
// does this automatically).
type Client struct {
	t               transport
	protocolVersion string

	nextID atomic.Int64
	wmu    sync.Mutex // serializes writes

	mu      sync.Mutex
	pending map[int64]chan response
	err     error // terminal error; set once

	done     chan struct{}
	events   chan Event
	readDone chan struct{}

	// Owned by the read loop.
	seq     uint64
	dropped bool
}

// Dial connects to a running iTerm2. It never launches iTerm2.
func Dial(ctx context.Context, opts Options) (*Client, error) {
	path := opts.SocketPath
	if path == "" {
		var err error
		if path, err = DefaultSocketPath(); err != nil {
			return nil, err
		}
	}
	appName := opts.AppName
	if appName == "" {
		appName = defaultAppName
	}
	auth := opts.Authenticate
	if auth == nil {
		auth = AppleScriptAuth
	}

	// Connect to the socket before authenticating. A successful connection
	// shows that iTerm2 is listening, so the AppleScript step cannot launch it.
	nc, err := (&net.Dialer{}).DialContext(ctx, "unix", path)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("%w: %v", ErrNotRunning, err)
	}

	creds, authErr := auth(ctx, appName)
	if errors.Is(authErr, ErrNotRunning) || ctx.Err() != nil {
		nc.Close()
		return nil, errors.Join(authErr, ctx.Err())
	}

	hdr := http.Header{}
	hdr.Set("origin", "ws://localhost/")
	hdr.Set("x-iterm2-library-version", LibraryVersion)
	hdr.Set("x-iterm2-disable-auth-ui", "true")
	hdr.Set("x-iterm2-advisory-name", appName)
	if creds.Cookie != "" {
		hdr.Set("x-iterm2-cookie", creds.Cookie)
	}
	if creds.Key != "" {
		hdr.Set("x-iterm2-key", creds.Key)
	}

	var used atomic.Bool
	httpClient := &http.Client{Transport: &http.Transport{
		DialContext: func(context.Context, string, string) (net.Conn, error) {
			if used.Swap(true) {
				return nil, errors.New("iterm2: socket already used")
			}
			return nc, nil
		},
		DisableKeepAlives: true,
	}}
	ws, resp, err := websocket.Dial(ctx, "ws://localhost/", &websocket.DialOptions{
		HTTPClient:      httpClient,
		HTTPHeader:      hdr,
		Subprotocols:    []string{subprotocol},
		CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		nc.Close()
		if resp != nil {
			switch resp.StatusCode {
			case http.StatusUnauthorized:
				return nil, errors.Join(ErrUnauthorized, authErr)
			case http.StatusNotAcceptable:
				return nil, fmt.Errorf("iterm2: library version %q rejected (406): %w", LibraryVersion, err)
			}
		}
		return nil, fmt.Errorf("iterm2: websocket handshake: %w", err)
	}
	ws.SetReadLimit(readLimit)

	c := newClient(wsTransport{ws}, opts.EventBuffer)
	c.protocolVersion = resp.Header.Get(protocolVersionHdr)
	go c.readLoop()
	return c, nil
}

// DefaultSocketPath returns the path iTerm2 listens on, honoring IT2_SUITE.
func DefaultSocketPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("iterm2: locating home directory: %w", err)
	}
	return filepath.Join(appSupportDir(home), "private", "socket"), nil
}

func appSupportDir(home string) string {
	suite := os.Getenv("IT2_SUITE")
	if suite == "" {
		suite = "iTerm2"
	}
	return filepath.Join(home, "Library", "Application Support", suite)
}

// newClient wraps an established transport. The caller starts readLoop.
func newClient(t transport, eventBuffer int) *Client {
	if eventBuffer <= 0 {
		eventBuffer = defaultEventBuffer
	}
	return &Client{
		t:        t,
		pending:  make(map[int64]chan response),
		done:     make(chan struct{}),
		events:   make(chan Event, eventBuffer),
		readDone: make(chan struct{}),
	}
}

// ProtocolVersion returns the server's X-iTerm2-Protocol-Version header, such
// as "1.23". It is empty for very old iTerm2 versions.
func (c *Client) ProtocolVersion() string { return c.protocolVersion }

// Events delivers notifications for subscribed event types. It is closed when
// the connection ends. The read loop never blocks on this channel. If the
// buffer fills up, events are dropped and an EventResync is delivered once
// space is available.
func (c *Client) Events() <-chan Event { return c.events }

// Done is closed when the connection has ended, through Close or a failure.
func (c *Client) Done() <-chan struct{} { return c.done }

// Err returns nil while connected. After Done is closed, it returns ErrClosed
// if Close was called, or a *ConnectionLostError otherwise.
func (c *Client) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

// Close shuts down the connection and fails outstanding calls with ErrClosed.
// It waits for the read loop to exit.
func (c *Client) Close() error {
	c.shutdown(ErrClosed)
	<-c.readDone
	return nil
}

// shutdown records the terminal error, unblocks callers, and closes the
// transport. Only the first call has any effect.
func (c *Client) shutdown(err error) {
	c.mu.Lock()
	if c.err != nil {
		c.mu.Unlock()
		return
	}
	c.err = err
	c.pending = nil
	close(c.done)
	c.mu.Unlock()
	c.t.Close()
}

func (c *Client) readLoop() {
	defer close(c.readDone)
	defer close(c.events)
	for {
		p, err := c.t.Read(context.Background())
		if err != nil {
			c.shutdown(&ConnectionLostError{Err: err})
			return
		}
		msg := &pb.ServerOriginatedMessage{}
		if err := proto.Unmarshal(p, msg); err != nil {
			c.shutdown(&ConnectionLostError{Err: fmt.Errorf("decoding message: %w", err)})
			return
		}
		c.seq++
		if n := msg.GetNotification(); n != nil {
			if ev, ok := eventFromNotification(n); ok {
				ev.seq = c.seq
				c.deliver(ev)
			}
			continue
		}
		c.mu.Lock()
		ch, ok := c.pending[msg.GetId()]
		delete(c.pending, msg.GetId())
		c.mu.Unlock()
		if ok {
			ch <- response{msg: msg, seq: c.seq} // buffered; never blocks
		}
		// Responses to abandoned (context-canceled) calls are dropped.
	}
}

// deliver hands ev to the consumer without blocking.
func (c *Client) deliver(ev Event) {
	if c.dropped {
		select {
		case c.events <- Event{Type: EventResync, seq: ev.seq}:
			c.dropped = false
		default:
			return
		}
	}
	select {
	case c.events <- ev:
	default:
		c.dropped = true
	}
}

// call sends one request and waits for its response.
func (c *Client) call(ctx context.Context, req *pb.ClientOriginatedMessage) (response, error) {
	r := c.callMany(ctx, []*pb.ClientOriginatedMessage{req})
	return r[0].response, r[0].err
}

type result struct {
	response
	err error
}

// callMany pipelines several requests: it sends all of them before waiting for
// any response. A per-request *RPCError does not affect the other requests. A
// connection or context error fails all requests that have not completed.
func (c *Client) callMany(ctx context.Context, reqs []*pb.ClientOriginatedMessage) []result {
	results := make([]result, len(reqs))
	chans := make([]chan response, len(reqs))
	ids := make([]int64, len(reqs))

	c.mu.Lock()
	if c.err != nil {
		err := c.err
		c.mu.Unlock()
		for i := range results {
			results[i].err = err
		}
		return results
	}
	for i, req := range reqs {
		ids[i] = c.nextID.Add(1)
		req.Id = proto.Int64(ids[i])
		chans[i] = make(chan response, 1)
		c.pending[ids[i]] = chans[i]
	}
	c.mu.Unlock()

	defer func() {
		c.mu.Lock()
		for _, id := range ids {
			delete(c.pending, id) // no-op on nil map after shutdown
		}
		c.mu.Unlock()
	}()

	for i, req := range reqs {
		if err := c.write(req); err != nil {
			failFrom(results, i, err)
			reqs = reqs[:i]
			break
		}
	}

	for i := range reqs {
		var r response
		select {
		case r = <-chans[i]:
		case <-ctx.Done():
			failFrom(results, i, ctx.Err())
			return results
		case <-c.done:
			// The response may have arrived just before shutdown.
			select {
			case r = <-chans[i]:
			default:
				failFrom(results, i, c.Err())
				return results
			}
		}
		results[i].response = r
		if e, ok := r.msg.GetSubmessage().(*pb.ServerOriginatedMessage_Error); ok {
			results[i].err = &RPCError{Message: e.Error}
		}
	}
	return results
}

func failFrom(results []result, i int, err error) {
	for ; i < len(results); i++ {
		if results[i].err == nil {
			results[i].err = err
		}
	}
}

func (c *Client) write(req *pb.ClientOriginatedMessage) error {
	p, err := proto.Marshal(req)
	if err != nil {
		return fmt.Errorf("iterm2: encoding request: %w", err)
	}
	// Writes use their own timeout rather than the caller's context: an
	// interrupted WebSocket write corrupts the stream and closes the
	// connection, which would affect every other caller.
	ctx, cancel := context.WithTimeout(context.Background(), writeTimeout)
	defer cancel()
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if err := c.Err(); err != nil {
		return err
	}
	if err := c.t.Write(ctx, p); err != nil {
		c.shutdown(&ConnectionLostError{Err: err})
		return c.Err()
	}
	return nil
}
