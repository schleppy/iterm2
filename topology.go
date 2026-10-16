package iterm2

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	pb "github.com/schleppy/iterm2/internal/iterm2pb"
)

// Snapshot fetches the complete topology. It pipelines ListSessions and Focus
// requests, then one variable request per session for its working directory.
func (c *Client) Snapshot(ctx context.Context) (*Snapshot, error) {
	st, err := c.fetchState(ctx)
	if err != nil {
		return nil, err
	}
	return &st.snap, nil
}

// state is a snapshot plus the arrival order of the responses it was built
// from. Watcher uses these to discard events that the snapshot already
// reflects.
type state struct {
	snap      Snapshot
	layoutSeq uint64
	focusSeq  uint64
}

func (c *Client) fetchState(ctx context.Context) (state, error) {
	rs := c.callMany(ctx, []*pb.ClientOriginatedMessage{
		{Submessage: &pb.ClientOriginatedMessage_ListSessionsRequest{ListSessionsRequest: &pb.ListSessionsRequest{}}},
		{Submessage: &pb.ClientOriginatedMessage_FocusRequest{FocusRequest: &pb.FocusRequest{}}},
	})
	for _, r := range rs {
		if r.err != nil {
			return state{}, r.err
		}
	}
	list := rs[0].msg.GetListSessionsResponse()
	focus := rs[1].msg.GetFocusResponse()
	if list == nil || focus == nil {
		return state{}, errors.New("iterm2: unexpected response to list sessions or focus request")
	}

	st := state{layoutSeq: rs[0].seq, focusSeq: rs[1].seq}
	st.snap.Windows, st.snap.BuriedSessions = parseLayout(list)
	st.snap.ProtocolVersion = c.protocolVersion
	// The focus response is applied over the layout even if it is older. Any
	// focus change after it arrives as an event with a later sequence number.
	for _, n := range focus.GetNotifications() {
		if fc, ok := focusChangeFromProto(n); ok {
			applyFocus(&st.snap, fc) // unknown IDs: keep the layout's own value
		}
	}

	paths, err := c.fetchPaths(ctx, sessionIDs(&st.snap, nil))
	if err != nil {
		return state{}, err
	}
	setPaths(&st.snap, paths)
	st.snap.UpdatedAt = time.Now()
	return st, nil
}

// parseLayout converts a ListSessionsResponse. It drops windows without tabs,
// as the Python library does (window.py:create_from_proto).
func parseLayout(r *pb.ListSessionsResponse) ([]Window, []Session) {
	var windows []Window
	for _, pw := range r.GetWindows() {
		if len(pw.GetTabs()) == 0 {
			continue
		}
		w := Window{
			ID:            pw.GetWindowId(),
			Number:        int(pw.GetNumber()),
			SelectedTabID: pw.GetSelectedTabId(),
		}
		for _, pt := range pw.GetTabs() {
			t := Tab{
				ID:               pt.GetTabId(),
				ActiveSessionID:  pt.GetActiveSessionId(),
				TmuxConnectionID: pt.GetTmuxConnectionId(),
			}
			if pt.Root != nil {
				t.Root = parseSplit(pt.Root, &t.Sessions)
			}
			for _, s := range pt.GetMinimizedSessions() {
				t.Sessions = append(t.Sessions, sessionFromSummary(s, func(s *Session) { s.Minimized = true }))
			}
			for _, f := range pt.GetFloatingPanes() {
				t.Sessions = append(t.Sessions, sessionFromSummary(f.GetSession(), func(s *Session) { s.Floating = true }))
			}
			w.Tabs = append(w.Tabs, t)
		}
		windows = append(windows, w)
	}
	var buried []Session
	for _, s := range r.GetBuriedSessions() {
		buried = append(buried, sessionFromSummary(s, nil))
	}
	return windows, buried
}

// parseSplit converts a split tree, appending its sessions to sessions in
// depth-first order (session.py:Splitter.from_node).
func parseSplit(n *pb.SplitTreeNode, sessions *[]Session) *Split {
	sp := &Split{Vertical: n.GetVertical()}
	for _, link := range n.GetLinks() {
		switch {
		case link.GetSession() != nil:
			s := sessionFromSummary(link.GetSession(), nil)
			*sessions = append(*sessions, s)
			sp.Children = append(sp.Children, SplitChild{SessionID: s.ID})
		case link.GetNode() != nil:
			sp.Children = append(sp.Children, SplitChild{Split: parseSplit(link.GetNode(), sessions)})
		}
	}
	return sp
}

func sessionFromSummary(s *pb.SessionSummary, mod func(*Session)) Session {
	out := Session{ID: s.GetUniqueIdentifier(), Name: s.GetTitle()}
	if mod != nil {
		mod(&out)
	}
	return out
}

// applyFocus updates s for one focus change, following app.py:_async_focus_change.
// It returns false if the change names a window, tab, or session that s does
// not contain. Callers should then refresh.
func applyFocus(s *Snapshot, fc FocusChange) bool {
	switch fc.Kind {
	case FocusApp:
		s.Focus.AppActive = fc.AppActive
	case FocusWindow:
		// A resign does not change the current terminal window.
		if fc.WindowStatus == WindowResignedKey {
			return true
		}
		if fc.WindowID != "" && s.Window(fc.WindowID) == nil {
			return false
		}
		s.Focus.CurrentWindowID = fc.WindowID
	case FocusTab:
		_, w := s.Tab(fc.TabID)
		if w == nil {
			return false
		}
		w.SelectedTabID = fc.TabID
	case FocusSession:
		_, _, t := s.Session(fc.SessionID)
		if t == nil {
			return false
		}
		t.ActiveSessionID = fc.SessionID
	}
	return true
}

// forEachSession calls f for every session in s, including buried ones.
func forEachSession(s *Snapshot, f func(*Session)) {
	for wi := range s.Windows {
		for ti := range s.Windows[wi].Tabs {
			for si := range s.Windows[wi].Tabs[ti].Sessions {
				f(&s.Windows[wi].Tabs[ti].Sessions[si])
			}
		}
	}
	for i := range s.BuriedSessions {
		f(&s.BuriedSessions[i])
	}
}

// sessionIDs returns the IDs of all sessions in s that keep accepts (all if
// keep is nil).
func sessionIDs(s *Snapshot, keep func(id string) bool) []string {
	var ids []string
	forEachSession(s, func(x *Session) {
		if keep == nil || keep(x.ID) {
			ids = append(ids, x.ID)
		}
	})
	return ids
}

func setPaths(s *Snapshot, paths map[string]string) {
	forEachSession(s, func(x *Session) {
		if p, ok := paths[x.ID]; ok {
			x.Path = p
		}
	})
}

// fetchPaths reads the "path" variable of each session. Sessions that iTerm2
// no longer knows about, or whose path is unset, map to "". Only connection
// and context errors are returned.
func (c *Client) fetchPaths(ctx context.Context, ids []string) (map[string]string, error) {
	paths := make(map[string]string, len(ids))
	if len(ids) == 0 {
		return paths, nil
	}
	reqs := make([]*pb.ClientOriginatedMessage, len(ids))
	for i, id := range ids {
		reqs[i] = &pb.ClientOriginatedMessage{
			Submessage: &pb.ClientOriginatedMessage_VariableRequest{
				VariableRequest: &pb.VariableRequest{
					Scope: &pb.VariableRequest_SessionId{SessionId: id},
					Get:   []string{"path"},
				},
			},
		}
	}
	for i, r := range c.callMany(ctx, reqs) {
		paths[ids[i]] = ""
		var rpcErr *RPCError
		switch {
		case errors.As(r.err, &rpcErr):
			continue
		case r.err != nil:
			return nil, r.err
		}
		vr := r.msg.GetVariableResponse()
		if vr.GetStatus() != pb.VariableResponse_OK || len(vr.GetValues()) == 0 {
			continue // typically SESSION_NOT_FOUND: closed since ListSessions
		}
		var p *string // JSON null for unset variables
		if err := json.Unmarshal([]byte(vr.GetValues()[0]), &p); err == nil && p != nil {
			paths[ids[i]] = *p
		}
	}
	return paths, nil
}
