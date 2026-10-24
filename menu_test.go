package iterm2

import (
	"errors"
	"testing"

	"google.golang.org/protobuf/proto"

	pb "github.com/schleppy/iterm2/internal/iterm2pb"
)

// menuOnce runs f against a one-request server that answers a menu item request with resp,
// and returns the request it received.
func menuOnce(t *testing.T, resp *pb.MenuItemResponse, f func(*Client) error) (*pb.MenuItemRequest, error) {
	t.Helper()
	ct, st := newPipe()
	c := startClient(ct, 0)
	defer c.Close()
	got := make(chan *pb.MenuItemRequest, 1)
	go func() {
		req := readRequest(t, st)
		got <- req.GetMenuItemRequest()
		writeMessage(t, st, &pb.ServerOriginatedMessage{Id: req.Id, Submessage: &pb.ServerOriginatedMessage_MenuItemResponse{MenuItemResponse: resp}})
	}()
	err := f(c)
	return <-got, err
}

func TestSelectMenuItem(t *testing.T) {
	ok := &pb.MenuItemResponse{Status: pb.MenuItemResponse_OK.Enum()}
	req, err := menuOnce(t, ok, func(c *Client) error { return c.SelectMenuItem(ctxT(t), MenuMaximizeActivePane) })
	if err != nil {
		t.Fatal(err)
	}
	if req.GetIdentifier() != "Maximize Active Pane" || req.GetQueryOnly() {
		t.Errorf("request = %v", req)
	}
	for st, want := range map[pb.MenuItemResponse_Status]error{
		pb.MenuItemResponse_BAD_IDENTIFIER: ErrBadMenuItem,
		pb.MenuItemResponse_DISABLED:       ErrMenuItemDisabled,
	} {
		_, err := menuOnce(t, &pb.MenuItemResponse{Status: st.Enum()}, func(c *Client) error { return c.SelectMenuItem(ctxT(t), "x") })
		if !errors.Is(err, want) {
			t.Errorf("%s: err = %v, want %v", st, err, want)
		}
	}
}

func TestMenuItemState(t *testing.T) {
	var got MenuState
	resp := &pb.MenuItemResponse{Status: pb.MenuItemResponse_OK.Enum(), Checked: proto.Bool(true), Enabled: proto.Bool(true)}
	req, err := menuOnce(t, resp, func(c *Client) (err error) {
		got, err = c.MenuItemState(ctxT(t), MenuMaximizeActivePane)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if !req.GetQueryOnly() || got != (MenuState{Checked: true, Enabled: true}) {
		t.Errorf("request = %v, state = %+v", req, got)
	}
}
