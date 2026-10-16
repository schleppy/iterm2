package iterm2

import (
	"slices"
	"time"
)

// Snapshot is a point-in-time view of iTerm2's windows, tabs, and sessions.
//
// Snapshots returned by this package are not shared with any other consumer,
// but they do share Split trees with each other. Treat them as read-only, or
// call Clone before modifying one.
type Snapshot struct {
	// Windows in iTerm2's order. Windows without tabs are omitted, matching
	// the Python library.
	Windows []Window

	// BuriedSessions are sessions that are running but not attached to any
	// tab (Session > Bury).
	BuriedSessions []Session

	Focus Focus

	// ProtocolVersion is the server's X-iTerm2-Protocol-Version, e.g. "1.23".
	ProtocolVersion string

	// UpdatedAt is when the snapshot was last reconciled with iTerm2.
	UpdatedAt time.Time

	// Stale is set by Watcher while it is disconnected from iTerm2. The rest of
	// the snapshot is the last known state.
	Stale bool
}

// Focus describes which window has focus.
type Focus struct {
	// AppActive reports whether iTerm2 is the active application.
	AppActive bool

	// CurrentWindowID is the terminal window that receives keyboard input when
	// iTerm2 is active. It is empty if there is no terminal window.
	CurrentWindowID string
}

// Window is a terminal window.
type Window struct {
	ID string

	// Number is the window number used in keyboard shortcuts and in the
	// "w<N>" prefix of ITERM_SESSION_ID.
	Number int

	// SelectedTabID is the tab showing in this window.
	SelectedTabID string

	Tabs []Tab
}

// Tab is a tab within a window.
type Tab struct {
	ID string

	// ActiveSessionID is the session in this tab that has focus.
	ActiveSessionID string

	// Root describes the split-pane arrangement. It is nil only if iTerm2
	// reported no layout for the tab.
	Root *Split

	// Sessions lists every session in the tab, in this order: split panes in
	// Root order (depth first), then minimized sessions, then floating panes.
	Sessions []Session

	// TmuxConnectionID is set when the tab belongs to a tmux integration
	// (tmux -CC) client.
	TmuxConnectionID string
}

// Split is a node in a tab's split-pane tree.
type Split struct {
	// Vertical reports whether the dividers are vertical, which puts the
	// children side by side.
	Vertical bool
	Children []SplitChild
}

// SplitChild is either a session (a pane) or a nested split. Exactly one of
// the two fields is set.
type SplitChild struct {
	SessionID string
	Split     *Split
}

// Session is a terminal session, which corresponds to a pane.
type Session struct {
	// ID is iTerm2's unique session identifier, a GUID. It equals the part of
	// ITERM_SESSION_ID after the colon. See ParseSessionID.
	ID string

	// Name is the session name as shown in the tab bar.
	Name string

	// Path is the session's working directory as known to iTerm2. It is
	// empty when unknown. Without shell integration iTerm2 can only guess the
	// directory, so it may lag behind.
	Path string

	// Minimized is set for sessions hidden because another pane in the tab is
	// maximized.
	Minimized bool

	// Floating is set for floating panes (protocol 1.21+).
	Floating bool
}

// Session returns the session with the given ID, including buried sessions,
// along with its window and tab. Window and tab are nil for buried sessions.
func (s *Snapshot) Session(id string) (*Session, *Window, *Tab) {
	for wi := range s.Windows {
		w := &s.Windows[wi]
		for ti := range w.Tabs {
			t := &w.Tabs[ti]
			for si := range t.Sessions {
				if t.Sessions[si].ID == id {
					return &t.Sessions[si], w, t
				}
			}
		}
	}
	for i := range s.BuriedSessions {
		if s.BuriedSessions[i].ID == id {
			return &s.BuriedSessions[i], nil, nil
		}
	}
	return nil, nil, nil
}

// Window returns the window with the given ID, or nil.
func (s *Snapshot) Window(id string) *Window {
	for i := range s.Windows {
		if s.Windows[i].ID == id {
			return &s.Windows[i]
		}
	}
	return nil
}

// Tab returns the tab with the given ID and its window, or nil.
func (s *Snapshot) Tab(id string) (*Tab, *Window) {
	for wi := range s.Windows {
		w := &s.Windows[wi]
		for ti := range w.Tabs {
			if w.Tabs[ti].ID == id {
				return &w.Tabs[ti], w
			}
		}
	}
	return nil, nil
}

// FocusedSessionID returns the active session of the selected tab in the
// current window. It is empty if any link in that chain is unknown. A
// non-empty result does not mean that iTerm2 is the active application; check
// Focus.AppActive for that.
func (s *Snapshot) FocusedSessionID() string {
	w := s.Window(s.Focus.CurrentWindowID)
	if w == nil {
		return ""
	}
	t, _ := s.Tab(w.SelectedTabID)
	if t == nil {
		return ""
	}
	return t.ActiveSessionID
}

// SessionCount returns the number of sessions in tabs. Buried sessions are not
// counted.
func (s *Snapshot) SessionCount() int {
	n := 0
	for _, w := range s.Windows {
		for _, t := range w.Tabs {
			n += len(t.Sessions)
		}
	}
	return n
}

// Clone returns a deep copy of the snapshot. Split trees are shared, because
// nothing in this package modifies them after parsing.
func (s Snapshot) Clone() Snapshot {
	c := s
	c.Windows = slices.Clone(s.Windows)
	for i := range c.Windows {
		w := &c.Windows[i]
		w.Tabs = slices.Clone(w.Tabs)
		for j := range w.Tabs {
			w.Tabs[j].Sessions = slices.Clone(w.Tabs[j].Sessions)
		}
	}
	c.BuriedSessions = slices.Clone(s.BuriedSessions)
	return c
}
