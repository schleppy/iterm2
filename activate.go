package iterm2

import (
	"context"
	"errors"
	"fmt"

	"google.golang.org/protobuf/proto"

	pb "github.com/schleppy/iterm2/internal/iterm2pb"
)

// ErrBadIdentifier is returned by Activate when iTerm2 has no window, tab, or
// session with the given ID (for example because it was just closed).
var ErrBadIdentifier = errors.New("iterm2: no such window, tab, or session")

// ActivateOptions says what Activate brings forward. Set at most one of
// WindowID, TabID, and SessionID; with none set, only the app is activated
// (App must then be true).
type ActivateOptions struct {
	WindowID  string
	TabID     string
	SessionID string

	// App makes iTerm2 the active application, even if another app is
	// frontmost.
	App bool
	// RaiseAllWindows brings every iTerm2 window forward along with the app,
	// not only the target's.
	RaiseAllWindows bool
}

// Activate brings a window, tab, or session to the front, the way clicking it
// would: the target's window is ordered front, a tab or session's tab is
// selected, and a session becomes the active pane of its tab. It does not
// de-miniaturize windows.
func (c *Client) Activate(ctx context.Context, o ActivateOptions) error {
	req := &pb.ActivateRequest{}
	n := 0
	if o.WindowID != "" {
		req.Identifier = &pb.ActivateRequest_WindowId{WindowId: o.WindowID}
		n++
	}
	if o.TabID != "" {
		req.Identifier = &pb.ActivateRequest_TabId{TabId: o.TabID}
		req.SelectTab = proto.Bool(true)
		n++
	}
	if o.SessionID != "" {
		req.Identifier = &pb.ActivateRequest_SessionId{SessionId: o.SessionID}
		req.SelectTab = proto.Bool(true)
		req.SelectSession = proto.Bool(true)
		n++
	}
	switch {
	case n > 1:
		return errors.New("iterm2: Activate takes at most one of WindowID, TabID, and SessionID")
	case n == 0 && !o.App:
		return errors.New("iterm2: Activate needs a target or App")
	case n == 1:
		req.OrderWindowFront = proto.Bool(true)
	}
	if o.App {
		req.ActivateApp = &pb.ActivateRequest_App{
			RaiseAllWindows:   proto.Bool(o.RaiseAllWindows),
			IgnoringOtherApps: proto.Bool(true),
		}
	}
	r, err := c.call(ctx, &pb.ClientOriginatedMessage{
		Submessage: &pb.ClientOriginatedMessage_ActivateRequest{ActivateRequest: req},
	})
	if err != nil {
		return err
	}
	resp := r.msg.GetActivateResponse()
	if resp == nil {
		return fmt.Errorf("iterm2: unexpected response to activate request: %T", r.msg.GetSubmessage())
	}
	switch st := resp.GetStatus(); st {
	case pb.ActivateResponse_OK:
		return nil
	case pb.ActivateResponse_BAD_IDENTIFIER:
		return ErrBadIdentifier
	default:
		return fmt.Errorf("iterm2: activate failed: %s", st)
	}
}
