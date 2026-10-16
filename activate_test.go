package iterm2

import (
	"errors"
	"testing"

	pb "github.com/schleppy/iterm2/internal/iterm2pb"
)

// activateOnce runs Activate against a one-request server that answers with
// status and returns the request it received.
func activateOnce(t *testing.T, o ActivateOptions, status pb.ActivateResponse_Status) (*pb.ActivateRequest, error) {
	t.Helper()
	ct, st := newPipe()
	c := startClient(ct, 0)
	defer c.Close()
	got := make(chan *pb.ActivateRequest, 1)
	go func() {
		req := readRequest(t, st)
		got <- req.GetActivateRequest()
		writeMessage(t, st, &pb.ServerOriginatedMessage{Id: req.Id, Submessage: &pb.ServerOriginatedMessage_ActivateResponse{
			ActivateResponse: &pb.ActivateResponse{Status: status.Enum()},
		}})
	}()
	err := c.Activate(ctxT(t), o)
	return <-got, err
}

func TestActivateSession(t *testing.T) {
	req, err := activateOnce(t, ActivateOptions{SessionID: "S1", App: true}, pb.ActivateResponse_OK)
	if err != nil {
		t.Fatal(err)
	}
	if req.GetSessionId() != "S1" || !req.GetSelectTab() || !req.GetSelectSession() || !req.GetOrderWindowFront() {
		t.Errorf("request = %v", req)
	}
	if app := req.GetActivateApp(); app == nil || !app.GetIgnoringOtherApps() || app.GetRaiseAllWindows() {
		t.Errorf("activate_app = %v", app)
	}
}

func TestActivateTabAndWindow(t *testing.T) {
	req, err := activateOnce(t, ActivateOptions{TabID: "7"}, pb.ActivateResponse_OK)
	if err != nil {
		t.Fatal(err)
	}
	if req.GetTabId() != "7" || !req.GetSelectTab() || req.SelectSession != nil || req.ActivateApp != nil {
		t.Errorf("tab request = %v", req)
	}
	req, err = activateOnce(t, ActivateOptions{WindowID: "pty-1"}, pb.ActivateResponse_OK)
	if err != nil {
		t.Fatal(err)
	}
	if req.GetWindowId() != "pty-1" || req.SelectTab != nil || !req.GetOrderWindowFront() {
		t.Errorf("window request = %v", req)
	}
}

func TestActivateAppOnly(t *testing.T) {
	req, err := activateOnce(t, ActivateOptions{App: true, RaiseAllWindows: true}, pb.ActivateResponse_OK)
	if err != nil {
		t.Fatal(err)
	}
	if req.Identifier != nil || req.OrderWindowFront != nil || !req.GetActivateApp().GetRaiseAllWindows() {
		t.Errorf("request = %v", req)
	}
}

func TestActivateBadIdentifier(t *testing.T) {
	_, err := activateOnce(t, ActivateOptions{SessionID: "gone"}, pb.ActivateResponse_BAD_IDENTIFIER)
	if !errors.Is(err, ErrBadIdentifier) {
		t.Fatalf("err = %v", err)
	}
	_, err = activateOnce(t, ActivateOptions{SessionID: "x"}, pb.ActivateResponse_INVALID_OPTION)
	if err == nil || errors.Is(err, ErrBadIdentifier) {
		t.Fatalf("invalid option err = %v", err)
	}
}

func TestActivateRejectsBadOptions(t *testing.T) {
	c := startClient(func() transport { ct, _ := newPipe(); return ct }(), 0)
	defer c.Close()
	for _, o := range []ActivateOptions{{}, {WindowID: "w", SessionID: "s"}} {
		if err := c.Activate(ctxT(t), o); err == nil {
			t.Errorf("Activate(%+v) succeeded", o)
		}
	}
}

func TestActivateUnexpectedResponse(t *testing.T) {
	ct, st := newPipe()
	c := startClient(ct, 0)
	defer c.Close()
	go func() {
		req := readRequest(t, st)
		writeMessage(t, st, variableResponse(req.GetId(), `"x"`))
	}()
	if err := c.Activate(ctxT(t), ActivateOptions{App: true}); err == nil {
		t.Fatal("want error for a non-activate response")
	}
}
