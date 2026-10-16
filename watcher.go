package iterm2

import (
	"context"
	"log/slog"
	"math/rand/v2"
	"reflect"
	"sync"
	"time"
)

// WatcherOptions configures a Watcher. The zero value is usable.
type WatcherOptions struct {
	// Client configures each connection.
	Client Options

	// Dial replaces Dial(ctx, Client). It is mainly useful in tests.
	Dial func(ctx context.Context) (*Client, error)

	// Debounce is how long to wait after a notification for more to arrive
	// before reconciling. Defaults to 100ms.
	Debounce time.Duration

	// MaxDebounce caps the delay during a continuous stream of notifications.
	// Defaults to 1s.
	MaxDebounce time.Duration

	// ReconcileInterval is how often a full snapshot is taken even without
	// notifications. This picks up changes that iTerm2 sends no notification
	// for, such as session names and working directories. Defaults to 30s. A
	// negative value disables periodic reconciliation.
	ReconcileInterval time.Duration

	// MinBackoff and MaxBackoff bound the exponential reconnect delay.
	// Defaults are 500ms and 30s.
	MinBackoff, MaxBackoff time.Duration

	// Timeout bounds each dial, subscribe, and snapshot operation. Defaults to
	// 30s, which leaves time for the one-time macOS Automation prompt.
	Timeout time.Duration

	// Logger receives connection-level messages. Session names and paths are
	// never logged. Defaults to discarding.
	Logger *slog.Logger
}

// Watcher maintains an up-to-date Snapshot of iTerm2. It connects, subscribes
// to TopologyEvents, takes a snapshot, and then reconciles after debounced
// notifications and on a timer. When the connection is lost it marks the
// snapshot stale and reconnects with exponential backoff.
//
// All iTerm2 I/O happens on the goroutine that calls Run, one operation at a
// time, so an older result never overwrites a newer one.
type Watcher struct {
	opts    WatcherOptions
	log     *slog.Logger
	updates chan Snapshot

	mu        sync.Mutex
	snap      Snapshot // latest state, including Stale
	published Snapshot // last value sent on updates
}

// NewWatcher returns a Watcher. Call Run to start it.
func NewWatcher(opts WatcherOptions) *Watcher {
	def := func(d *time.Duration, v time.Duration) {
		if *d == 0 {
			*d = v
		}
	}
	def(&opts.Debounce, 100*time.Millisecond)
	def(&opts.MaxDebounce, time.Second)
	def(&opts.ReconcileInterval, 30*time.Second)
	def(&opts.MinBackoff, 500*time.Millisecond)
	def(&opts.MaxBackoff, 30*time.Second)
	def(&opts.Timeout, 30*time.Second)
	if opts.Dial == nil {
		clientOpts := opts.Client
		opts.Dial = func(ctx context.Context) (*Client, error) { return Dial(ctx, clientOpts) }
	}
	log := opts.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Watcher{
		opts:    opts,
		log:     log,
		updates: make(chan Snapshot, 1),
		snap:    Snapshot{Stale: true},
	}
}

// Snapshot returns the latest known state. Before the first successful
// connection it is empty and stale.
func (w *Watcher) Snapshot() Snapshot {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.snap.Clone()
}

// Updates delivers a snapshot each time the topology, focus, or staleness
// changes. Only the latest value is kept, so a slow reader skips intermediate
// states but never misses the newest one. The channel is closed when Run
// returns.
func (w *Watcher) Updates() <-chan Snapshot { return w.updates }

// Run connects and keeps the snapshot current until ctx is canceled, which is
// the only way it returns. Call it at most once.
func (w *Watcher) Run(ctx context.Context) error {
	defer close(w.updates)
	backoff := w.opts.MinBackoff
	for {
		synced, err := w.connectAndWatch(ctx)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		w.markStale()
		if synced {
			backoff = w.opts.MinBackoff
			w.log.Warn("iterm2: disconnected", "err", err)
		} else {
			w.log.Debug("iterm2: connect failed", "err", err, "retry_in", backoff)
		}
		// Up to 20% jitter avoids reconnecting in lockstep with other clients
		// after an iTerm2 restart.
		delay := backoff + rand.N(backoff/5+1)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
		if !synced {
			backoff = min(backoff*2, w.opts.MaxBackoff)
		}
	}
}

// connectAndWatch runs one connection. synced reports whether the initial
// snapshot was published, which resets the reconnect backoff.
func (w *Watcher) connectAndWatch(ctx context.Context) (synced bool, err error) {
	dctx, cancel := context.WithTimeout(ctx, w.opts.Timeout)
	c, err := w.opts.Dial(dctx)
	cancel()
	if err != nil {
		return false, err
	}
	defer c.Close()

	// Subscribe before the first snapshot: a change that happens in between
	// produces an event with a later sequence number than the snapshot, so it
	// is not lost.
	sctx, cancel := context.WithTimeout(ctx, w.opts.Timeout)
	err = c.Subscribe(sctx, TopologyEvents...)
	cancel()
	if err != nil {
		return false, err
	}
	r := reconciler{w: w, c: c}
	if err := r.refresh(ctx); err != nil {
		return false, err
	}
	w.log.Debug("iterm2: connected", "protocol", c.ProtocolVersion())
	return true, r.loop(ctx)
}

// reconciler holds per-connection state. Only the Run goroutine uses it.
type reconciler struct {
	w   *Watcher
	c   *Client
	cur state // working copy; published by cloning

	needFull      bool
	pendingLayout *Event
	pendingFocus  []Event
}

func (r *reconciler) loop(ctx context.Context) error {
	debounce := time.NewTimer(time.Hour)
	debounce.Stop()
	var armed bool
	var firstAt time.Time

	var tick <-chan time.Time
	if r.w.opts.ReconcileInterval > 0 {
		t := time.NewTicker(r.w.opts.ReconcileInterval)
		defer t.Stop()
		tick = t.C
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case ev, ok := <-r.c.Events():
			if !ok {
				<-r.c.Done()
				return r.c.Err()
			}
			r.handle(ev)
			if !armed {
				armed, firstAt = true, time.Now()
			}
			debounce.Reset(min(r.w.opts.Debounce, time.Until(firstAt.Add(r.w.opts.MaxDebounce))))
		case <-debounce.C:
			armed = false
			if err := r.flush(ctx); err != nil {
				return err
			}
		case <-tick:
			r.needFull = true
			if err := r.flush(ctx); err != nil {
				return err
			}
		}
	}
}

// handle records an event for the next flush. Events older than the current
// state are dropped: the snapshot that produced the state already reflects
// them.
func (r *reconciler) handle(ev Event) {
	switch ev.Type {
	case EventResync:
		r.needFull = true
	case EventSessionCreated, EventSessionTerminated:
		if ev.seq > r.cur.layoutSeq {
			r.needFull = true
		}
	case EventLayoutChanged:
		if ev.seq > r.cur.layoutSeq {
			r.pendingLayout = &ev
		}
	case EventFocusChanged:
		if ev.seq > r.cur.focusSeq {
			r.pendingFocus = append(r.pendingFocus, ev)
		}
	}
}

// flush applies everything recorded since the last flush and publishes the
// result if it changed.
func (r *reconciler) flush(ctx context.Context) error {
	// Take in events that are already queued, so that a refresh is not
	// followed immediately by a redundant one.
	for drained := false; !drained; {
		select {
		case ev, ok := <-r.c.Events():
			if !ok {
				drained = true
				break
			}
			r.handle(ev)
		default:
			drained = true
		}
	}

	if !r.needFull && r.pendingLayout != nil {
		if err := r.applyLayout(ctx, r.pendingLayout); err != nil {
			return err
		}
	}
	// Focus events are applied after the layout even if some are older. That
	// is safe because each one states an absolute fact ("tab T is selected"),
	// and every later focus change has its own later event in this list.
	if !r.needFull {
		for _, ev := range r.pendingFocus {
			if !applyFocus(&r.cur.snap, ev.Focus) {
				r.needFull = true // names a tab or session we have not seen
				break
			}
			r.cur.focusSeq = ev.seq
		}
	}
	r.pendingLayout, r.pendingFocus = nil, nil

	if r.needFull {
		return r.refresh(ctx)
	}
	r.w.store(r.cur.snap)
	return nil
}

// refresh replaces the state with a full snapshot.
func (r *reconciler) refresh(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, r.w.opts.Timeout)
	defer cancel()
	st, err := r.c.fetchState(ctx)
	if err != nil {
		return err
	}
	r.cur = st
	r.needFull, r.pendingLayout, r.pendingFocus = false, nil, nil
	r.w.store(r.cur.snap)
	return nil
}

// applyLayout updates the state from the topology in a layout notification.
// This avoids a ListSessions round trip. Only sessions not seen before need a
// working-directory fetch.
func (r *reconciler) applyLayout(ctx context.Context, ev *Event) error {
	if ev.layout == nil {
		r.needFull = true
		return nil
	}
	old := r.cur.snap
	next := old
	next.Windows, next.BuriedSessions = parseLayout(ev.layout)

	known := map[string]string{} // session ID -> path
	forEachSession(&old, func(s *Session) { known[s.ID] = s.Path })
	setPaths(&next, known)

	// Selected tabs and active sessions: protocol 1.18+ layouts carry them,
	// but they may be older than the focus information already applied (a
	// layout that arrived between a refresh's ListSessions and Focus
	// responses). Use whichever is newer and still valid; servers before 1.18
	// leave the layout's values empty.
	layoutNewer := ev.seq > r.cur.focusSeq
	for wi := range next.Windows {
		nw := &next.Windows[wi]
		if ow := old.Window(nw.ID); ow != nil && (nw.SelectedTabID == "" || !layoutNewer) {
			if t, w := next.Tab(ow.SelectedTabID); t != nil && w.ID == nw.ID {
				nw.SelectedTabID = ow.SelectedTabID
			}
		}
		for ti := range nw.Tabs {
			nt := &nw.Tabs[ti]
			if ot, _ := old.Tab(nt.ID); ot != nil && (nt.ActiveSessionID == "" || !layoutNewer) {
				if _, _, t := next.Session(ot.ActiveSessionID); t != nil && t.ID == nt.ID {
					nt.ActiveSessionID = ot.ActiveSessionID
				}
			}
		}
	}
	if next.Focus.CurrentWindowID != "" && next.Window(next.Focus.CurrentWindowID) == nil {
		r.needFull = true // the current window closed; let a full refresh settle focus
		return nil
	}

	newIDs := sessionIDs(&next, func(id string) bool { _, ok := known[id]; return !ok })
	if len(newIDs) > 0 {
		ctx, cancel := context.WithTimeout(ctx, r.w.opts.Timeout)
		defer cancel()
		paths, err := r.c.fetchPaths(ctx, newIDs)
		if err != nil {
			return err
		}
		setPaths(&next, paths)
	}

	next.UpdatedAt = time.Now()
	r.cur.snap = next
	r.cur.layoutSeq = ev.seq
	return nil
}

// store records s as the latest state and publishes it if it differs from the
// last published snapshot in anything other than UpdatedAt.
func (w *Watcher) store(s Snapshot) {
	s = s.Clone()
	w.mu.Lock()
	defer w.mu.Unlock()
	w.snap = s
	a, b := s, w.published
	a.UpdatedAt, b.UpdatedAt = time.Time{}, time.Time{}
	if reflect.DeepEqual(a, b) {
		return
	}
	w.published = s
	select {
	case <-w.updates: // replace an unread value
	default:
	}
	w.updates <- s.Clone()
}

func (w *Watcher) markStale() {
	w.mu.Lock()
	s := w.snap
	w.mu.Unlock()
	if !s.Stale {
		s.Stale = true
		w.store(s)
	}
}
