package iterm2

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/coder/websocket"
)

// unixSocketPath returns a path short enough for the 104-byte sun_path limit
// on macOS, which t.TempDir() can exceed.
func unixSocketPath(t *testing.T) string {
	dir, err := os.MkdirTemp("", "it2")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return filepath.Join(dir, "s")
}

// serveUnix runs an HTTP server on a Unix socket, as iTerm2 does.
func serveUnix(t *testing.T, h http.Handler) string {
	path := unixSocketPath(t)
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: h}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return path
}

func staticAuth(creds Credentials, err error) Authenticator {
	return func(context.Context, string) (Credentials, error) { return creds, err }
}

func TestDialHandshakeAndSnapshot(t *testing.T) {
	var mu sync.Mutex
	var got http.Header
	fake := newTopologyServer()

	path := serveUnix(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		got = r.Header.Clone()
		got.Set("Host", r.Host)
		got.Set("Path", r.URL.Path)
		mu.Unlock()
		w.Header().Set("X-iTerm2-Protocol-Version", "1.23")
		ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{"api.iterm2.com"}})
		if err != nil {
			t.Errorf("accept: %v", err)
			return
		}
		fake.start(wsTransport{ws})
	}))

	c, err := Dial(ctxT(t), Options{
		SocketPath:   path,
		AppName:      "tester",
		Authenticate: staticAuth(Credentials{Cookie: "cookie1", Key: "key1"}, nil),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	mu.Lock()
	for k, want := range map[string]string{
		"Host":                     "localhost",
		"Path":                     "/",
		"Origin":                   "ws://localhost/",
		"Sec-Websocket-Protocol":   "api.iterm2.com",
		"X-Iterm2-Library-Version": LibraryVersion,
		"X-Iterm2-Cookie":          "cookie1",
		"X-Iterm2-Key":             "key1",
		"X-Iterm2-Advisory-Name":   "tester",
		"X-Iterm2-Disable-Auth-Ui": "true",
	} {
		if v := got.Get(k); v != want {
			t.Errorf("header %s = %q, want %q", k, v, want)
		}
	}
	mu.Unlock()
	if c.ProtocolVersion() != "1.23" {
		t.Errorf("protocol version = %q", c.ProtocolVersion())
	}

	snap, err := c.Snapshot(ctxT(t))
	if err != nil {
		t.Fatal(err)
	}
	if snap.SessionCount() != 4 || snap.FocusedSessionID() != "s1" {
		t.Fatalf("snapshot = %+v", snap)
	}
}

func TestDialUnauthorized(t *testing.T) {
	path := serveUnix(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	authErr := errors.New("automation denied")
	_, err := Dial(ctxT(t), Options{SocketPath: path, Authenticate: staticAuth(Credentials{}, authErr)})
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("err = %v, want ErrUnauthorized", err)
	}
	if !errors.Is(err, authErr) {
		t.Fatalf("err = %v, want it to include the authenticator's error", err)
	}
}

func TestDialNotRunning(t *testing.T) {
	called := false
	auth := func(context.Context, string) (Credentials, error) { called = true; return Credentials{}, nil }

	_, err := Dial(ctxT(t), Options{SocketPath: unixSocketPath(t), Authenticate: auth})
	if !errors.Is(err, ErrNotRunning) {
		t.Fatalf("missing socket: err = %v", err)
	}

	// A stale socket file with no listener, as left behind when iTerm2 quits.
	path := unixSocketPath(t)
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	ln.(*net.UnixListener).SetUnlinkOnClose(false)
	ln.Close()
	if _, err := Dial(ctxT(t), Options{SocketPath: path, Authenticate: auth}); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("stale socket: err = %v", err)
	}
	if called {
		t.Fatal("authenticator ran although iTerm2 is not running")
	}
}

func TestDialAuthenticatorReportsNotRunning(t *testing.T) {
	path := serveUnix(t, http.NotFoundHandler())
	_, err := Dial(ctxT(t), Options{SocketPath: path, Authenticate: staticAuth(Credentials{}, ErrNotRunning)})
	if !errors.Is(err, ErrNotRunning) {
		t.Fatalf("err = %v", err)
	}
}

func TestDialRejectedLibraryVersion(t *testing.T) {
	path := serveUnix(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotAcceptable)
	}))
	_, err := Dial(ctxT(t), Options{SocketPath: path, Authenticate: NoAuth})
	if err == nil || errors.Is(err, ErrUnauthorized) {
		t.Fatalf("err = %v", err)
	}
}
