package iterm2

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	pb "github.com/schleppy/iterm2/internal/iterm2pb"
)

// readRequest and writeMessage act as the server. They may run on goroutines
// other than the test's, so they report failures with Errorf, not Fatal.
func readRequest(t *testing.T, srv *pipe) *pb.ClientOriginatedMessage {
	t.Helper()
	req := &pb.ClientOriginatedMessage{}
	p, err := srv.Read(ctxT(t))
	if err != nil {
		t.Errorf("server read: %v", err)
		return req
	}
	if err := proto.Unmarshal(p, req); err != nil {
		t.Errorf("server decode: %v", err)
	}
	return req
}

func writeMessage(t *testing.T, srv *pipe, m *pb.ServerOriginatedMessage) {
	t.Helper()
	p, _ := proto.Marshal(m)
	if err := srv.Write(ctxT(t), p); err != nil {
		t.Errorf("server write: %v", err)
	}
}

// variableResponse tags a response with a value so the test can check which
// request it reached.
func variableResponse(id int64, value string) *pb.ServerOriginatedMessage {
	return &pb.ServerOriginatedMessage{Id: proto.Int64(id), Submessage: &pb.ServerOriginatedMessage_VariableResponse{
		VariableResponse: &pb.VariableResponse{Status: pb.VariableResponse_OK.Enum(), Values: []string{value}},
	}}
}

func variableRequest(session string) *pb.ClientOriginatedMessage {
	return &pb.ClientOriginatedMessage{Submessage: &pb.ClientOriginatedMessage_VariableRequest{
		VariableRequest: &pb.VariableRequest{Scope: &pb.VariableRequest_SessionId{SessionId: session}, Get: []string{"path"}},
	}}
}

func layoutNotification() *pb.ServerOriginatedMessage {
	return &pb.ServerOriginatedMessage{Submessage: &pb.ServerOriginatedMessage_Notification{Notification: &pb.Notification{
		LayoutChangedNotification: &pb.LayoutChangedNotification{ListSessionsResponse: twoWindowLayout()},
	}}}
}

func newSessionNotification(id string) *pb.ServerOriginatedMessage {
	return &pb.ServerOriginatedMessage{Submessage: &pb.ServerOriginatedMessage_Notification{Notification: &pb.Notification{
		NewSessionNotification: &pb.NewSessionNotification{SessionId: proto.String(id)},
	}}}
}

func TestResponsesCorrelatedOutOfOrder(t *testing.T) {
	ct, st := newPipe()
	c := startClient(ct, 0)
	defer c.Close()

	const n = 5
	var wg sync.WaitGroup
	got := make([]string, n)
	errs := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := c.call(ctxT(t), variableRequest("s"+strconv.Itoa(i)))
			errs[i] = err
			if err == nil {
				got[i] = r.msg.GetVariableResponse().GetValues()[0]
			}
		}()
	}

	// Read every request before answering any, then answer in reverse order,
	// echoing the session each request asked about.
	reqs := make([]*pb.ClientOriginatedMessage, n)
	for i := range n {
		reqs[i] = readRequest(t, st)
	}
	for i := n - 1; i >= 0; i-- {
		writeMessage(t, st, variableResponse(reqs[i].GetId(), reqs[i].GetVariableRequest().GetSessionId()))
	}
	wg.Wait()

	for i := range n {
		if errs[i] != nil {
			t.Fatalf("call %d: %v", i, errs[i])
		}
		if want := "s" + strconv.Itoa(i); got[i] != want {
			t.Errorf("call %d got response for %q, want %q", i, got[i], want)
		}
	}
	ids := map[int64]bool{}
	for _, r := range reqs {
		if ids[r.GetId()] || r.GetId() == 0 {
			t.Errorf("request id %d reused or zero", r.GetId())
		}
		ids[r.GetId()] = true
	}
}

func TestNotificationsInterleavedWithResponses(t *testing.T) {
	ct, st := newPipe()
	c := startClient(ct, 0)
	defer c.Close()

	done := make(chan error, 1)
	var got string
	go func() {
		r, err := c.call(ctxT(t), variableRequest("s1"))
		if err == nil {
			got = r.msg.GetVariableResponse().GetValues()[0]
		}
		done <- err
	}()

	req := readRequest(t, st)
	writeMessage(t, st, newSessionNotification("before"))
	writeMessage(t, st, layoutNotification())
	writeMessage(t, st, variableResponse(req.GetId(), "answer"))
	writeMessage(t, st, newSessionNotification("after"))

	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if got != "answer" {
		t.Fatalf("response = %q", got)
	}
	want := []Event{
		{Type: EventSessionCreated, SessionID: "before"},
		{Type: EventLayoutChanged},
		{Type: EventSessionCreated, SessionID: "after"},
	}
	var lastSeq uint64
	for i, w := range want {
		ev := <-c.Events()
		if ev.Type != w.Type || ev.SessionID != w.SessionID {
			t.Errorf("event %d = %v %q, want %v %q", i, ev.Type, ev.SessionID, w.Type, w.SessionID)
		}
		if ev.seq <= lastSeq {
			t.Errorf("event %d seq %d not increasing", i, ev.seq)
		}
		lastSeq = ev.seq
		if ev.Type == EventLayoutChanged && ev.layout == nil {
			t.Error("layout event lost its topology")
		}
	}
}

func TestConnectionLostFailsOutstandingCalls(t *testing.T) {
	ct, st := newPipe()
	c := startClient(ct, 0)

	done := make(chan error, 1)
	go func() {
		_, err := c.call(ctxT(t), variableRequest("s1"))
		done <- err
	}()
	readRequest(t, st) // never answered
	st.Close()

	err := <-done
	var lost *ConnectionLostError
	if !errors.As(err, &lost) {
		t.Fatalf("err = %v, want ConnectionLostError", err)
	}
	<-c.Done()
	if !errors.As(c.Err(), &lost) {
		t.Fatalf("Err() = %v", c.Err())
	}
	if _, ok := <-c.Events(); ok {
		t.Fatal("Events not closed")
	}
	if _, err := c.call(ctxT(t), variableRequest("s1")); !errors.As(err, &lost) {
		t.Fatalf("call after loss = %v", err)
	}
	c.Close() // safe after loss
}

func TestCloseFailsOutstandingCalls(t *testing.T) {
	ct, st := newPipe()
	c := startClient(ct, 0)

	done := make(chan error, 1)
	go func() {
		_, err := c.call(ctxT(t), variableRequest("s1"))
		done <- err
	}()
	readRequest(t, st)
	c.Close()
	if err := <-done; !errors.Is(err, ErrClosed) {
		t.Fatalf("err = %v, want ErrClosed", err)
	}
	if !errors.Is(c.Err(), ErrClosed) {
		t.Fatalf("Err() = %v", c.Err())
	}
}

func TestContextCancellation(t *testing.T) {
	ct, st := newPipe()
	c := startClient(ct, 0)
	defer c.Close()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := c.call(ctx, variableRequest("slow"))
		done <- err
	}()
	slow := readRequest(t, st)
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}

	// The abandoned response arrives late and is discarded. The connection
	// stays usable.
	writeMessage(t, st, variableResponse(slow.GetId(), "late"))
	go func() {
		req := readRequest(t, st)
		writeMessage(t, st, variableResponse(req.GetId(), "fresh"))
	}()
	r, err := c.call(ctxT(t), variableRequest("next"))
	if err != nil {
		t.Fatal(err)
	}
	if v := r.msg.GetVariableResponse().GetValues()[0]; v != "fresh" {
		t.Fatalf("got %q, want fresh", v)
	}
	select {
	case <-c.Done():
		t.Fatal("connection closed by cancellation")
	default:
	}
}

func TestRPCError(t *testing.T) {
	ct, st := newPipe()
	c := startClient(ct, 0)
	defer c.Close()
	go func() {
		req := readRequest(t, st)
		writeMessage(t, st, &pb.ServerOriginatedMessage{Id: req.Id, Submessage: &pb.ServerOriginatedMessage_Error{Error: "bad"}})
	}()
	_, err := c.call(ctxT(t), variableRequest("s1"))
	var rpcErr *RPCError
	if !errors.As(err, &rpcErr) || rpcErr.Message != "bad" {
		t.Fatalf("err = %v", err)
	}
}

func TestUndecodableMessageEndsConnection(t *testing.T) {
	ct, st := newPipe()
	c := startClient(ct, 0)
	defer c.Close()
	st.Write(ctxT(t), []byte{0xff, 0xff, 0xff})
	<-c.Done()
	var lost *ConnectionLostError
	if !errors.As(c.Err(), &lost) {
		t.Fatalf("Err() = %v", c.Err())
	}
}

func TestSubscribe(t *testing.T) {
	srv := newTopologyServer()
	c := srv.connect()
	defer c.Close()

	if err := c.Subscribe(ctxT(t)); err != nil {
		t.Fatal(err)
	}
	for _, nt := range []pb.NotificationType{
		pb.NotificationType_NOTIFY_ON_NEW_SESSION, pb.NotificationType_NOTIFY_ON_TERMINATE_SESSION,
		pb.NotificationType_NOTIFY_ON_LAYOUT_CHANGE, pb.NotificationType_NOTIFY_ON_FOCUS_CHANGE,
	} {
		if !srv.subscribed(nt) {
			t.Errorf("not subscribed to %v", nt)
		}
	}

	if err := c.Unsubscribe(ctxT(t), EventFocusChanged); err != nil {
		t.Fatal(err)
	}
	if srv.subscribed(pb.NotificationType_NOTIFY_ON_FOCUS_CHANGE) {
		t.Error("still subscribed to focus changes")
	}

	srv.set(func(s *fakeServer) { s.subStatus = pb.NotificationResponse_ALREADY_SUBSCRIBED })
	if err := c.Subscribe(ctxT(t), EventLayoutChanged); err != nil {
		t.Fatalf("ALREADY_SUBSCRIBED should succeed: %v", err)
	}
	if err := c.Unsubscribe(ctxT(t), EventLayoutChanged); err == nil {
		t.Fatal("unsubscribe with ALREADY_SUBSCRIBED status should fail")
	}

	if err := c.Subscribe(ctxT(t), EventResync); err == nil {
		t.Fatal("subscribing to EventResync should fail")
	}
}

func TestSubscriptionFailure(t *testing.T) {
	srv := newTopologyServer()
	srv.subStatus = pb.NotificationResponse_REQUEST_MALFORMED
	c := srv.connect()
	defer c.Close()

	err := c.Subscribe(ctxT(t), EventSessionCreated, EventFocusChanged)
	var se *SubscriptionError
	if !errors.As(err, &se) {
		t.Fatalf("err = %v, want SubscriptionError", err)
	}
	if se.Event != EventSessionCreated || se.Status != "REQUEST_MALFORMED" {
		t.Fatalf("got %+v", se)
	}
	if srv.count("notification") != 2 {
		t.Fatalf("sent %d requests, want 2 (pipelined)", srv.count("notification"))
	}
}

func TestSubscriptionRequestFields(t *testing.T) {
	ct, st := newPipe()
	c := startClient(ct, 0)
	defer c.Close()
	go func() {
		req := readRequest(t, st)
		nr := req.GetNotificationRequest()
		if nr.GetSession() != "all" || !nr.GetSubscribe() || nr.GetNotificationType() != pb.NotificationType_NOTIFY_ON_LAYOUT_CHANGE {
			t.Errorf("request = %v", nr)
		}
		writeMessage(t, st, &pb.ServerOriginatedMessage{Id: req.Id, Submessage: &pb.ServerOriginatedMessage_NotificationResponse{
			NotificationResponse: &pb.NotificationResponse{Status: pb.NotificationResponse_OK.Enum()},
		}})
	}()
	if err := c.Subscribe(ctxT(t), EventLayoutChanged); err != nil {
		t.Fatal(err)
	}
}

func TestEventOverflowProducesResync(t *testing.T) {
	ct, st := newPipe()
	c := startClient(ct, 2)
	defer c.Close()

	// roundTrip proves the read loop has handled everything sent before it.
	roundTrip := func() {
		t.Helper()
		go func() {
			req := readRequest(t, st)
			writeMessage(t, st, variableResponse(req.GetId(), "x"))
		}()
		if _, err := c.call(ctxT(t), variableRequest("s")); err != nil {
			t.Fatal(err)
		}
	}

	for i := range 5 {
		writeMessage(t, st, newSessionNotification(strconv.Itoa(i)))
	}
	roundTrip()
	for i := range 2 {
		if ev := <-c.Events(); ev.SessionID != strconv.Itoa(i) {
			t.Fatalf("event %d = %+v", i, ev)
		}
	}
	select {
	case ev := <-c.Events():
		t.Fatalf("unexpected event %+v", ev)
	default:
	}

	writeMessage(t, st, newSessionNotification("next"))
	roundTrip()
	if ev := <-c.Events(); ev.Type != EventResync {
		t.Fatalf("got %v, want resync", ev.Type)
	}
	if ev := <-c.Events(); ev.SessionID != "next" {
		t.Fatalf("got %+v, want next", ev)
	}
}

func TestCallTimesOutWithoutClosing(t *testing.T) {
	ct, st := newPipe()
	c := startClient(ct, 0)
	defer c.Close()
	go readRequest(t, st)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := c.call(ctx, variableRequest("s")); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
	if c.Err() != nil {
		t.Fatalf("client failed: %v", c.Err())
	}
}
