package iterm2

import (
	"reflect"
	"testing"

	"google.golang.org/protobuf/proto"

	pb "github.com/schleppy/iterm2/internal/iterm2pb"
)

func TestParseLayout(t *testing.T) {
	floating := &pb.FloatingPane{Session: &pb.SessionSummary{UniqueIdentifier: proto.String("f1"), Title: proto.String("float")}}
	tab := pTab("t1", "s2", pSplit(true,
		pSession("s1", "left"),
		pNode(pSplit(false, pSession("s2", "top"), pSession("s3", "bottom"))),
	))
	tab.MinimizedSessions = []*pb.SessionSummary{{UniqueIdentifier: proto.String("m1"), Title: proto.String("min")}}
	tab.FloatingPanes = []*pb.FloatingPane{floating}
	tab.TmuxConnectionId = proto.String("tmux-1")

	resp := pLayout(
		pWindow("w1", 3, "t1", tab),
		pWindow("empty", 4, ""), // no tabs: dropped
	)
	resp.BuriedSessions = []*pb.SessionSummary{{UniqueIdentifier: proto.String("b1"), Title: proto.String("buried")}}

	windows, buried := parseLayout(resp)
	want := []Window{{
		ID: "w1", Number: 3, SelectedTabID: "t1",
		Tabs: []Tab{{
			ID: "t1", ActiveSessionID: "s2", TmuxConnectionID: "tmux-1",
			Root: &Split{Vertical: true, Children: []SplitChild{
				{SessionID: "s1"},
				{Split: &Split{Children: []SplitChild{{SessionID: "s2"}, {SessionID: "s3"}}}},
			}},
			Sessions: []Session{
				{ID: "s1", Name: "left"},
				{ID: "s2", Name: "top"},
				{ID: "s3", Name: "bottom"},
				{ID: "m1", Name: "min", Minimized: true},
				{ID: "f1", Name: "float", Floating: true},
			},
		}},
	}}
	if !reflect.DeepEqual(windows, want) {
		t.Errorf("windows =\n%+v\nwant\n%+v", windows, want)
	}
	if !reflect.DeepEqual(buried, []Session{{ID: "b1", Name: "buried"}}) {
		t.Errorf("buried = %+v", buried)
	}
}

func TestParseLayoutOldServer(t *testing.T) {
	// Protocol < 1.18 omits selected_tab_id and active_session_id.
	windows, _ := parseLayout(pLayout(pWindow("w1", 0, "", pTab("t1", "", pSplit(false, pSession("s1", "x"))))))
	if windows[0].SelectedTabID != "" || windows[0].Tabs[0].ActiveSessionID != "" {
		t.Fatalf("got %+v", windows)
	}
}

func TestSnapshotCombinesLayoutFocusAndPaths(t *testing.T) {
	srv := newTopologyServer()
	srv.paths["s2"] = nil   // variable unset
	delete(srv.paths, "s3") // SESSION_NOT_FOUND
	srv.focus = []*pb.FocusChangedNotification{
		focusApp(false),
		focusWindow("w2", pb.FocusChangedNotification_Window_TERMINAL_WINDOW_IS_CURRENT),
		focusSelectedTab("t2"), // overrides the layout's selected tab
		focusSelectedTab("unknown"),
		focusSession("s4"),
	}
	c := srv.connect()
	defer c.Close()

	snap, err := c.Snapshot(ctxT(t))
	if err != nil {
		t.Fatal(err)
	}
	if snap.Focus != (Focus{AppActive: false, CurrentWindowID: "w2"}) {
		t.Errorf("focus = %+v", snap.Focus)
	}
	if w := snap.Window("w1"); w.SelectedTabID != "t2" {
		t.Errorf("w1 selected tab = %q", w.SelectedTabID)
	}
	if got := snap.FocusedSessionID(); got != "s4" {
		t.Errorf("focused = %q", got)
	}
	for id, want := range map[string]string{"s1": "/home/s1", "s2": "", "s3": "", "s4": "/home/s4"} {
		s, _, _ := snap.Session(id)
		if s.Path != want {
			t.Errorf("%s path = %q, want %q", id, s.Path, want)
		}
	}
	if snap.ProtocolVersion != "1.23" || snap.UpdatedAt.IsZero() || snap.Stale {
		t.Errorf("metadata = %q %v %v", snap.ProtocolVersion, snap.UpdatedAt, snap.Stale)
	}
	// Two pipelined requests, then one variable request per session.
	if srv.count("list") != 1 || srv.count("focus") != 1 || srv.count("variable") != 4 {
		t.Errorf("counts = %v", srv.counts)
	}
}

func TestApplyFocus(t *testing.T) {
	windows, _ := parseLayout(twoWindowLayout())
	s := Snapshot{Windows: windows}

	steps := []struct {
		fc   FocusChange
		ok   bool
		want func() bool
	}{
		{FocusChange{Kind: FocusApp, AppActive: true}, true, func() bool { return s.Focus.AppActive }},
		{FocusChange{Kind: FocusWindow, WindowID: "w2", WindowStatus: WindowBecameKey}, true,
			func() bool { return s.Focus.CurrentWindowID == "w2" }},
		{FocusChange{Kind: FocusWindow, WindowID: "w2", WindowStatus: WindowResignedKey}, true,
			func() bool { return s.Focus.CurrentWindowID == "w2" }}, // resign keeps the current window
		{FocusChange{Kind: FocusWindow, WindowID: "nope", WindowStatus: WindowIsCurrent}, false, nil},
		{FocusChange{Kind: FocusTab, TabID: "t2"}, true, func() bool { return s.Window("w1").SelectedTabID == "t2" }},
		{FocusChange{Kind: FocusTab, TabID: "nope"}, false, nil},
		{FocusChange{Kind: FocusSession, SessionID: "s2"}, true, func() bool {
			tab, _ := s.Tab("t1")
			return tab.ActiveSessionID == "s2"
		}},
		{FocusChange{Kind: FocusSession, SessionID: "nope"}, false, nil},
	}
	for i, st := range steps {
		if ok := applyFocus(&s, st.fc); ok != st.ok {
			t.Fatalf("step %d: ok = %v", i, ok)
		}
		if st.want != nil && !st.want() {
			t.Fatalf("step %d: state = %+v", i, s)
		}
	}
}

func TestCloneIsDeep(t *testing.T) {
	windows, _ := parseLayout(twoWindowLayout())
	a := Snapshot{Windows: windows}
	b := a.Clone()
	b.Windows[0].SelectedTabID = "changed"
	b.Windows[0].Tabs[0].Sessions[0].Path = "changed"
	if a.Windows[0].SelectedTabID == "changed" || a.Windows[0].Tabs[0].Sessions[0].Path == "changed" {
		t.Fatal("clone shares state")
	}
}
