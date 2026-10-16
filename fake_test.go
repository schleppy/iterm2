package iterm2

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	pb "github.com/schleppy/iterm2/internal/iterm2pb"
)

// pipe is an in-memory, message-oriented transport. Writes are unbuffered, so
// a message written before Close is always read before the close is seen.
type pipe struct {
	in, out chan []byte
	closed  chan struct{}
	once    *sync.Once
}

var errPipeClosed = errors.New("pipe closed")

// newPipe returns the two ends of a connection.
func newPipe() (client, server *pipe) {
	a, b := make(chan []byte), make(chan []byte)
	closed, once := make(chan struct{}), &sync.Once{}
	return &pipe{in: a, out: b, closed: closed, once: once},
		&pipe{in: b, out: a, closed: closed, once: once}
}

func (p *pipe) Read(ctx context.Context) ([]byte, error) {
	select {
	case m := <-p.in:
		return m, nil
	case <-p.closed:
		return nil, errPipeClosed
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (p *pipe) Write(ctx context.Context, m []byte) error {
	select {
	case p.out <- m:
		return nil
	case <-p.closed:
		return errPipeClosed
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (p *pipe) Close() error {
	p.once.Do(func() { close(p.closed) })
	return nil
}

// startClient wraps the client end of a pipe the way Dial wraps a WebSocket.
func startClient(t transport, eventBuffer int) *Client {
	c := newClient(t, eventBuffer)
	c.protocolVersion = "1.23"
	go c.readLoop()
	return c
}

// fakeServer imitates the parts of iTerm2's API server that this package
// uses. Its state can be changed while it runs.
type fakeServer struct {
	tr transport

	mu        sync.Mutex
	layout    *pb.ListSessionsResponse
	focus     []*pb.FocusChangedNotification
	paths     map[string]*string // nil value: variable unset
	subs      map[pb.NotificationType]bool
	subStatus pb.NotificationResponse_Status
	counts    map[string]int
	// beforeResponse, if set, runs before each response is sent. It may call
	// notify. It runs on the serve goroutine.
	beforeResponse func(s *fakeServer, req *pb.ClientOriginatedMessage)
}

func newFakeServer(layout *pb.ListSessionsResponse) *fakeServer {
	return &fakeServer{
		layout: layout,
		paths:  map[string]*string{},
		subs:   map[pb.NotificationType]bool{},
		counts: map[string]int{},
	}
}

// connect starts serving on a new pipe and returns a client for it.
func (s *fakeServer) connect() *Client {
	ct, st := newPipe()
	s.start(st)
	return startClient(ct, 0)
}

// start serves the server end of a connection.
func (s *fakeServer) start(tr transport) {
	s.mu.Lock()
	s.tr = tr
	s.mu.Unlock()
	go s.serve(tr)
}

func (s *fakeServer) serve(tr transport) {
	for {
		p, err := tr.Read(context.Background())
		if err != nil {
			return
		}
		req := &pb.ClientOriginatedMessage{}
		if err := proto.Unmarshal(p, req); err != nil {
			panic(err)
		}
		s.mu.Lock()
		hook := s.beforeResponse
		s.mu.Unlock()
		if hook != nil {
			hook(s, req)
		}
		s.send(tr, s.respond(req))
	}
}

func (s *fakeServer) respond(req *pb.ClientOriginatedMessage) *pb.ServerOriginatedMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	resp := &pb.ServerOriginatedMessage{Id: req.Id}
	switch r := req.GetSubmessage().(type) {
	case *pb.ClientOriginatedMessage_ListSessionsRequest:
		s.counts["list"]++
		resp.Submessage = &pb.ServerOriginatedMessage_ListSessionsResponse{
			ListSessionsResponse: proto.Clone(s.layout).(*pb.ListSessionsResponse),
		}
	case *pb.ClientOriginatedMessage_FocusRequest:
		s.counts["focus"]++
		resp.Submessage = &pb.ServerOriginatedMessage_FocusResponse{
			FocusResponse: &pb.FocusResponse{Notifications: s.focus},
		}
	case *pb.ClientOriginatedMessage_VariableRequest:
		s.counts["variable"]++
		vr := &pb.VariableResponse{}
		id := r.VariableRequest.GetSessionId()
		if p, ok := s.paths[id]; ok && sessionInLayout(s.layout, id) {
			vr.Status = pb.VariableResponse_OK.Enum()
			if p == nil {
				vr.Values = []string{"null"}
			} else {
				vr.Values = []string{`"` + *p + `"`}
			}
		} else {
			vr.Status = pb.VariableResponse_SESSION_NOT_FOUND.Enum()
		}
		resp.Submessage = &pb.ServerOriginatedMessage_VariableResponse{VariableResponse: vr}
	case *pb.ClientOriginatedMessage_NotificationRequest:
		s.counts["notification"]++
		nr := r.NotificationRequest
		status := s.subStatus
		if status == pb.NotificationResponse_OK {
			s.subs[nr.GetNotificationType()] = nr.GetSubscribe()
		}
		resp.Submessage = &pb.ServerOriginatedMessage_NotificationResponse{
			NotificationResponse: &pb.NotificationResponse{Status: status.Enum()},
		}
	default:
		resp.Submessage = &pb.ServerOriginatedMessage_Error{Error: "unsupported"}
	}
	return resp
}

func (s *fakeServer) send(tr transport, m *pb.ServerOriginatedMessage) {
	p, err := proto.Marshal(m)
	if err != nil {
		panic(err)
	}
	tr.Write(context.Background(), p) // fails only once closed
}

// notify sends a notification on the current connection. From outside the
// serve goroutine it must not race with a response, which is fine for tests:
// the pipe serializes writes.
func (s *fakeServer) notify(n *pb.Notification) {
	s.mu.Lock()
	tr := s.tr
	s.mu.Unlock()
	s.send(tr, &pb.ServerOriginatedMessage{Submessage: &pb.ServerOriginatedMessage_Notification{Notification: n}})
}

func (s *fakeServer) disconnect() {
	s.mu.Lock()
	tr := s.tr
	s.mu.Unlock()
	tr.Close()
}

func (s *fakeServer) set(f func(s *fakeServer)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f(s)
}

func (s *fakeServer) count(kind string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.counts[kind]
}

func (s *fakeServer) subscribed(t pb.NotificationType) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.subs[t]
}

func sessionInLayout(l *pb.ListSessionsResponse, id string) bool {
	found := false
	var walk func(n *pb.SplitTreeNode)
	walk = func(n *pb.SplitTreeNode) {
		for _, link := range n.GetLinks() {
			if link.GetSession().GetUniqueIdentifier() == id {
				found = true
			}
			walk(link.GetNode())
		}
	}
	for _, w := range l.GetWindows() {
		for _, t := range w.GetTabs() {
			walk(t.GetRoot())
		}
	}
	return found
}

// Layout builders.

func pSession(id, title string) *pb.SplitTreeNode_SplitTreeLink {
	return &pb.SplitTreeNode_SplitTreeLink{Child: &pb.SplitTreeNode_SplitTreeLink_Session{
		Session: &pb.SessionSummary{UniqueIdentifier: proto.String(id), Title: proto.String(title)},
	}}
}

func pSplit(vertical bool, links ...*pb.SplitTreeNode_SplitTreeLink) *pb.SplitTreeNode {
	return &pb.SplitTreeNode{Vertical: proto.Bool(vertical), Links: links}
}

func pNode(n *pb.SplitTreeNode) *pb.SplitTreeNode_SplitTreeLink {
	return &pb.SplitTreeNode_SplitTreeLink{Child: &pb.SplitTreeNode_SplitTreeLink_Node{Node: n}}
}

func pTab(id, active string, root *pb.SplitTreeNode) *pb.ListSessionsResponse_Tab {
	t := &pb.ListSessionsResponse_Tab{TabId: proto.String(id), Root: root}
	if active != "" {
		t.ActiveSessionId = proto.String(active)
	}
	return t
}

func pWindow(id string, number int32, selected string, tabs ...*pb.ListSessionsResponse_Tab) *pb.ListSessionsResponse_Window {
	w := &pb.ListSessionsResponse_Window{WindowId: proto.String(id), Number: proto.Int32(number), Tabs: tabs}
	if selected != "" {
		w.SelectedTabId = proto.String(selected)
	}
	return w
}

func pLayout(windows ...*pb.ListSessionsResponse_Window) *pb.ListSessionsResponse {
	return &pb.ListSessionsResponse{Windows: windows}
}

func focusSelectedTab(tab string) *pb.FocusChangedNotification {
	return &pb.FocusChangedNotification{Event: &pb.FocusChangedNotification_SelectedTab{SelectedTab: tab}}
}

func focusSession(id string) *pb.FocusChangedNotification {
	return &pb.FocusChangedNotification{Event: &pb.FocusChangedNotification_Session{Session: id}}
}

func focusWindow(id string, status pb.FocusChangedNotification_Window_WindowStatus) *pb.FocusChangedNotification {
	return &pb.FocusChangedNotification{Event: &pb.FocusChangedNotification_Window_{
		Window: &pb.FocusChangedNotification_Window{WindowId: proto.String(id), WindowStatus: status.Enum()},
	}}
}

func focusApp(active bool) *pb.FocusChangedNotification {
	return &pb.FocusChangedNotification{Event: &pb.FocusChangedNotification_ApplicationActive{ApplicationActive: active}}
}

func strp(s string) *string { return &s }

// twoWindowLayout: w1 has tab t1 (s1 | s2, vertical) and tab t2 (s3); w2 has t3 (s4).
func twoWindowLayout() *pb.ListSessionsResponse {
	return pLayout(
		pWindow("w1", 1, "t1",
			pTab("t1", "s1", pSplit(true, pSession("s1", "one"), pSession("s2", "two"))),
			pTab("t2", "s3", pSplit(false, pSession("s3", "three")))),
		pWindow("w2", 2, "t3",
			pTab("t3", "s4", pSplit(false, pSession("s4", "four")))),
	)
}

func defaultFocus() []*pb.FocusChangedNotification {
	return []*pb.FocusChangedNotification{
		focusApp(true),
		focusWindow("w1", pb.FocusChangedNotification_Window_TERMINAL_WINDOW_BECAME_KEY),
		focusSelectedTab("t1"), focusSelectedTab("t3"),
		focusSession("s1"), focusSession("s3"), focusSession("s4"),
	}
}

func newTopologyServer() *fakeServer {
	s := newFakeServer(twoWindowLayout())
	s.focus = defaultFocus()
	for _, id := range []string{"s1", "s2", "s3", "s4"} {
		s.paths[id] = strp("/home/" + id)
	}
	return s
}

func ctxT(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}
