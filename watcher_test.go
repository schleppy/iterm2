package iterm2

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	pb "github.com/schleppy/iterm2/internal/iterm2pb"
)

// dialSequence returns a Dial function that connects to each server in turn.
// A nil entry simulates iTerm2 not running. After the list is exhausted it
// keeps failing.
func dialSequence(servers ...*fakeServer) (func(context.Context) (*Client, error), func() int) {
	var mu sync.Mutex
	n := 0
	dial := func(context.Context) (*Client, error) {
		mu.Lock()
		defer mu.Unlock()
		i := n
		n++
		if i >= len(servers) || servers[i] == nil {
			return nil, ErrNotRunning
		}
		return servers[i].connect(), nil
	}
	attempts := func() int {
		mu.Lock()
		defer mu.Unlock()
		return n
	}
	return dial, attempts
}

func startWatcher(t *testing.T, opts WatcherOptions) *Watcher {
	t.Helper()
	if opts.Debounce == 0 {
		opts.Debounce = 20 * time.Millisecond
	}
	if opts.ReconcileInterval == 0 {
		opts.ReconcileInterval = -1
	}
	opts.MinBackoff, opts.MaxBackoff = 5*time.Millisecond, 20*time.Millisecond
	w := NewWatcher(opts)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Errorf("Run returned %v", err)
		}
		if _, ok := <-w.Updates(); ok {
			// Drain the possible last value; the channel must then be closed.
			if _, ok := <-w.Updates(); ok {
				t.Error("Updates not closed after Run returned")
			}
		}
	})
	return w
}

// waitFor reads updates until one satisfies pred.
func waitFor(t *testing.T, w *Watcher, what string, pred func(Snapshot) bool) Snapshot {
	t.Helper()
	timeout := time.After(3 * time.Second)
	for {
		select {
		case s := <-w.Updates():
			if pred(s) {
				return s
			}
		case <-timeout:
			t.Fatalf("timed out waiting for %s; last snapshot %+v", what, w.Snapshot())
		}
	}
}

func synced(s Snapshot) bool { return !s.Stale && s.SessionCount() > 0 }

// noUpdate asserts that nothing is published for a while.
func noUpdate(t *testing.T, w *Watcher, d time.Duration) {
	t.Helper()
	select {
	case s := <-w.Updates():
		t.Fatalf("unexpected update %+v", s)
	case <-time.After(d):
	}
}

func newSession(id string) *pb.Notification {
	return &pb.Notification{NewSessionNotification: &pb.NewSessionNotification{SessionId: &id}}
}

func layoutChanged(l *pb.ListSessionsResponse) *pb.Notification {
	return &pb.Notification{LayoutChangedNotification: &pb.LayoutChangedNotification{ListSessionsResponse: l}}
}

func focusChanged(f *pb.FocusChangedNotification) *pb.Notification {
	return &pb.Notification{FocusChangedNotification: f}
}

// withExtraTab returns twoWindowLayout plus tab t5 (session s5) in window w2.
func withExtraTab() *pb.ListSessionsResponse {
	l := twoWindowLayout()
	l.Windows[1].Tabs = append(l.Windows[1].Tabs, pTab("t5", "s5", pSplit(false, pSession("s5", "five"))))
	return l
}

func TestWatcherInitialSync(t *testing.T) {
	srv := newTopologyServer()
	dial, _ := dialSequence(srv)
	w := startWatcher(t, WatcherOptions{Dial: dial})

	if s := w.Snapshot(); !s.Stale || s.SessionCount() != 0 {
		t.Fatalf("before connecting: %+v", s)
	}
	s := waitFor(t, w, "initial snapshot", synced)
	if s.SessionCount() != 4 || s.FocusedSessionID() != "s1" {
		t.Fatalf("snapshot = %+v", s)
	}
	if p, _, _ := s.Session("s3"); p.Path != "/home/s3" {
		t.Fatalf("s3 = %+v", p)
	}
	for _, nt := range []pb.NotificationType{
		pb.NotificationType_NOTIFY_ON_NEW_SESSION, pb.NotificationType_NOTIFY_ON_TERMINATE_SESSION,
		pb.NotificationType_NOTIFY_ON_LAYOUT_CHANGE, pb.NotificationType_NOTIFY_ON_FOCUS_CHANGE,
	} {
		if !srv.subscribed(nt) {
			t.Errorf("not subscribed to %v", nt)
		}
	}
}

func TestWatcherDebouncesRefresh(t *testing.T) {
	srv := newTopologyServer()
	dial, _ := dialSequence(srv)
	w := startWatcher(t, WatcherOptions{Dial: dial, Debounce: 50 * time.Millisecond})
	waitFor(t, w, "initial snapshot", synced)

	srv.set(func(s *fakeServer) {
		s.layout = withExtraTab()
		s.paths["s5"] = strp("/home/s5")
	})
	for range 10 {
		srv.notify(newSession("s5"))
	}
	s := waitFor(t, w, "new session", func(s Snapshot) bool { return s.SessionCount() == 5 })
	if p, _, _ := s.Session("s5"); p == nil || p.Path != "/home/s5" {
		t.Fatalf("s5 = %+v", p)
	}
	noUpdate(t, w, 150*time.Millisecond)
	if n := srv.count("list"); n != 2 {
		t.Fatalf("ListSessions called %d times, want 2 (initial + one debounced refresh)", n)
	}
}

func TestWatcherAppliesLayoutNotification(t *testing.T) {
	srv := newTopologyServer()
	dial, _ := dialSequence(srv)
	w := startWatcher(t, WatcherOptions{Dial: dial})
	waitFor(t, w, "initial snapshot", synced)

	srv.set(func(s *fakeServer) {
		s.layout = withExtraTab()
		s.paths["s5"] = strp("/home/s5")
	})
	srv.notify(layoutChanged(withExtraTab()))
	s := waitFor(t, w, "layout change", func(s Snapshot) bool { return s.SessionCount() == 5 })

	if p, _, _ := s.Session("s5"); p.Path != "/home/s5" {
		t.Errorf("new session path = %q", p.Path)
	}
	if p, _, _ := s.Session("s1"); p.Path != "/home/s1" {
		t.Errorf("existing session lost its path: %q", p.Path)
	}
	// The layout came with the notification, and only the new session needed
	// a variable fetch.
	if srv.count("list") != 1 || srv.count("variable") != 5 {
		t.Errorf("counts = list %d, variable %d", srv.count("list"), srv.count("variable"))
	}

	// An identical layout (as sent when a pane is resized) publishes nothing.
	srv.notify(layoutChanged(withExtraTab()))
	noUpdate(t, w, 100*time.Millisecond)
}

func TestWatcherAppliesFocusIncrementally(t *testing.T) {
	srv := newTopologyServer()
	dial, _ := dialSequence(srv)
	w := startWatcher(t, WatcherOptions{Dial: dial})
	waitFor(t, w, "initial snapshot", synced)

	srv.notify(focusChanged(focusSelectedTab("t2")))
	srv.notify(focusChanged(focusWindow("w2", pb.FocusChangedNotification_Window_TERMINAL_WINDOW_BECAME_KEY)))
	srv.notify(focusChanged(focusApp(false)))
	s := waitFor(t, w, "focus change", func(s Snapshot) bool { return s.Focus.CurrentWindowID == "w2" })
	if s.Window("w1").SelectedTabID != "t2" || s.Focus.AppActive || s.FocusedSessionID() != "s4" {
		t.Fatalf("snapshot = %+v", s)
	}
	if srv.count("list") != 1 || srv.count("focus") != 1 {
		t.Fatalf("focus change caused a refresh: list %d, focus %d", srv.count("list"), srv.count("focus"))
	}
}

func TestWatcherRefreshesOnUnknownFocusTarget(t *testing.T) {
	srv := newTopologyServer()
	dial, _ := dialSequence(srv)
	w := startWatcher(t, WatcherOptions{Dial: dial})
	waitFor(t, w, "initial snapshot", synced)

	srv.set(func(s *fakeServer) {
		s.layout = withExtraTab()
		s.paths["s5"] = strp("/home/s5")
		s.focus = append(s.focus, focusSelectedTab("t5"))
	})
	srv.notify(focusChanged(focusSelectedTab("t5"))) // a tab this snapshot has not seen
	waitFor(t, w, "refresh", func(s Snapshot) bool { return s.Window("w2").SelectedTabID == "t5" })
	if srv.count("list") != 2 {
		t.Fatalf("list count = %d, want 2", srv.count("list"))
	}
}

func TestWatcherIgnoresEventsOlderThanSnapshot(t *testing.T) {
	srv := newTopologyServer()
	first := true
	// Just before answering the first ListSessions, send events that describe
	// an older state. They arrive before the response, so the snapshot already
	// covers them.
	srv.beforeResponse = func(s *fakeServer, req *pb.ClientOriginatedMessage) {
		if req.GetListSessionsRequest() == nil || !first {
			return
		}
		first = false
		old := pLayout(pWindow("w1", 1, "t1", pTab("t1", "s1", pSplit(false, pSession("s1", "one")))))
		s.notify(layoutChanged(old))
		s.notify(newSession("s4"))
	}
	dial, _ := dialSequence(srv)
	w := startWatcher(t, WatcherOptions{Dial: dial})
	waitFor(t, w, "initial snapshot", synced)

	noUpdate(t, w, 100*time.Millisecond)
	if s := w.Snapshot(); s.SessionCount() != 4 {
		t.Fatalf("stale layout applied: %+v", s)
	}
	if srv.count("list") != 1 {
		t.Fatalf("stale event caused a refresh")
	}
}

func TestWatcherCatchesChangeDuringInitialSnapshot(t *testing.T) {
	srv := newTopologyServer()
	first := true
	// The topology changes after ListSessions is answered but before the
	// focus request is. The resulting notification must not be lost.
	srv.beforeResponse = func(s *fakeServer, req *pb.ClientOriginatedMessage) {
		if req.GetFocusRequest() == nil || !first {
			return
		}
		first = false
		s.set(func(s *fakeServer) {
			s.layout = withExtraTab()
			s.paths["s5"] = strp("/home/s5")
		})
		s.notify(newSession("s5"))
	}
	dial, _ := dialSequence(srv)
	w := startWatcher(t, WatcherOptions{Dial: dial})
	waitFor(t, w, "session created during init", func(s Snapshot) bool { return s.SessionCount() == 5 })
}

func TestWatcherLayoutDoesNotOverrideNewerFocus(t *testing.T) {
	srv := newTopologyServer()
	first := true
	// A layout notification arrives between the ListSessions and Focus
	// responses. It is newer than the list, so it is applied, but its selected
	// tab is older than the focus response and must not win.
	srv.beforeResponse = func(s *fakeServer, req *pb.ClientOriginatedMessage) {
		if req.GetFocusRequest() == nil || !first {
			return
		}
		first = false
		l := twoWindowLayout()
		l.Windows[0].SelectedTabId = strp("t2")
		l.Windows[0].Tabs[0].Root.Links[0].GetSession().Title = strp("renamed")
		s.notify(layoutChanged(l))
	}
	dial, _ := dialSequence(srv)
	w := startWatcher(t, WatcherOptions{Dial: dial})
	s := waitFor(t, w, "layout applied", func(s Snapshot) bool {
		p, _, _ := s.Session("s1")
		return p != nil && p.Name == "renamed"
	})
	if got := s.Window("w1").SelectedTabID; got != "t1" {
		t.Fatalf("selected tab = %q, want t1 from the newer focus response", got)
	}
}

func TestWatcherReconnects(t *testing.T) {
	srv1 := newTopologyServer()
	srv2 := newFakeServer(pLayout(pWindow("w9", 0, "t9", pTab("t9", "s9", pSplit(false, pSession("s9", "nine"))))))
	srv2.focus = []*pb.FocusChangedNotification{focusWindow("w9", pb.FocusChangedNotification_Window_TERMINAL_WINDOW_BECAME_KEY)}
	// srv1, then two failed attempts (iTerm2 restarting), then srv2.
	dial, attempts := dialSequence(srv1, nil, nil, srv2)
	w := startWatcher(t, WatcherOptions{Dial: dial})
	waitFor(t, w, "initial snapshot", synced)

	srv1.disconnect()
	stale := waitFor(t, w, "stale snapshot", func(s Snapshot) bool { return s.Stale })
	if stale.SessionCount() != 4 {
		t.Errorf("stale snapshot should keep the last known topology: %+v", stale)
	}
	if !w.Snapshot().Stale {
		t.Error("Snapshot() not stale")
	}

	s := waitFor(t, w, "reconnected snapshot", func(s Snapshot) bool { return !s.Stale })
	if s.Window("w9") == nil || s.SessionCount() != 1 || s.FocusedSessionID() != "s9" {
		t.Fatalf("snapshot after reconnect = %+v", s)
	}
	if !srv2.subscribed(pb.NotificationType_NOTIFY_ON_LAYOUT_CHANGE) || !srv2.subscribed(pb.NotificationType_NOTIFY_ON_FOCUS_CHANGE) {
		t.Error("subscriptions not reestablished")
	}
	if attempts() != 4 {
		t.Errorf("dial attempts = %d, want 4", attempts())
	}

	// Notifications work on the new connection.
	srv2.notify(focusChanged(focusApp(true)))
	waitFor(t, w, "focus on new connection", func(s Snapshot) bool { return s.Focus.AppActive })
}

func TestWatcherRetriesAfterSubscriptionFailure(t *testing.T) {
	srv1 := newTopologyServer()
	srv1.subStatus = pb.NotificationResponse_REQUEST_MALFORMED
	srv2 := newTopologyServer()
	dial, _ := dialSequence(srv1, srv2)
	w := startWatcher(t, WatcherOptions{Dial: dial})

	waitFor(t, w, "snapshot from second server", synced)
	if srv1.count("list") != 0 {
		t.Error("snapshot taken on a connection whose subscriptions failed")
	}
	select {
	case <-srv1.tr.(*pipe).closed:
	default:
		t.Error("failed connection not closed")
	}
}

func TestWatcherPeriodicReconcile(t *testing.T) {
	srv := newTopologyServer()
	dial, _ := dialSequence(srv)
	w := startWatcher(t, WatcherOptions{Dial: dial, ReconcileInterval: 50 * time.Millisecond})
	waitFor(t, w, "initial snapshot", synced)

	// Directory changes produce no notification.
	srv.set(func(s *fakeServer) { s.paths["s1"] = strp("/elsewhere") })
	waitFor(t, w, "reconciled path", func(s Snapshot) bool {
		p, _, _ := s.Session("s1")
		return p.Path == "/elsewhere"
	})
}

func TestWatcherResyncAfterDroppedEvents(t *testing.T) {
	srv := newTopologyServer()
	dial := func(context.Context) (*Client, error) {
		ct, st := newPipe()
		srv.start(st)
		return startClient(ct, 1), nil // room for a single event
	}
	w := startWatcher(t, WatcherOptions{Dial: dial})
	waitFor(t, w, "initial snapshot", synced)

	// Hold the next refresh in its variable fetch, so the watcher stops
	// reading events while they arrive.
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	srv.set(func(s *fakeServer) {
		s.beforeResponse = func(_ *fakeServer, req *pb.ClientOriginatedMessage) {
			if req.GetVariableRequest() != nil {
				once.Do(func() { close(entered); <-release })
			}
		}
	})
	srv.notify(newSession("s1")) // triggers the refresh
	<-entered

	srv.notify(focusChanged(focusApp(true))) // fills the buffer
	srv.set(func(s *fakeServer) {
		s.layout = withExtraTab()
		s.paths["s5"] = strp("/home/s5")
	})
	srv.notify(newSession("s5")) // dropped
	close(release)

	// A later notification is preceded by a resync, which forces a refresh
	// that picks up the change whose notification was dropped. Keep nudging,
	// because a nudge is itself dropped while the buffer is still full.
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		for {
			srv.notify(focusChanged(focusApp(true)))
			select {
			case <-stop:
				return
			case <-time.After(10 * time.Millisecond):
			}
		}
	}()
	waitFor(t, w, "resync refresh", func(s Snapshot) bool { return s.SessionCount() == 5 })
}
