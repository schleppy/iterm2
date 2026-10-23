package iterm2

import (
	"context"
	"errors"
	"fmt"

	"google.golang.org/protobuf/proto"

	pb "github.com/schleppy/iterm2/internal/iterm2pb"
)

// ErrCannotSplit is returned by SplitPane when the session is too small to
// split.
var ErrCannotSplit = errors.New("iterm2: session is too small to split")

// SplitOptions says how SplitPane divides a session.
type SplitOptions struct {
	// Vertical splits with a vertical divider, so the new session sits beside
	// the old one (iTerm2's ⌘D); otherwise the divider is horizontal and the
	// new session sits below it (⌘⇧D).
	Vertical bool
	// Before puts the new session left of (or above) the old one instead of
	// right of (or below) it.
	Before bool
	// Profile names the new session's profile; empty means the default
	// profile.
	Profile string
}

// SplitPane splits a session into two, the way iTerm2's Split Vertically and
// Split Horizontally menu items do, and returns the new session's ID. iTerm2
// makes the new session the active one in its tab (a focus event follows).
func (c *Client) SplitPane(ctx context.Context, sessionID string, o SplitOptions) ([]string, error) {
	dir := pb.SplitPaneRequest_HORIZONTAL
	if o.Vertical {
		dir = pb.SplitPaneRequest_VERTICAL
	}
	req := &pb.SplitPaneRequest{Session: proto.String(sessionID), SplitDirection: dir.Enum(), Before: proto.Bool(o.Before)}
	if o.Profile != "" {
		req.ProfileName = proto.String(o.Profile)
	}
	r, err := c.call(ctx, &pb.ClientOriginatedMessage{
		Submessage: &pb.ClientOriginatedMessage_SplitPaneRequest{SplitPaneRequest: req},
	})
	if err != nil {
		return nil, err
	}
	resp := r.msg.GetSplitPaneResponse()
	if resp == nil {
		return nil, fmt.Errorf("iterm2: unexpected response to split pane request: %T", r.msg.GetSubmessage())
	}
	switch st := resp.GetStatus(); st {
	case pb.SplitPaneResponse_OK:
		return resp.GetSessionId(), nil
	case pb.SplitPaneResponse_SESSION_NOT_FOUND:
		return nil, ErrBadIdentifier
	case pb.SplitPaneResponse_CANNOT_SPLIT:
		return nil, ErrCannotSplit
	default:
		return nil, fmt.Errorf("iterm2: split pane failed: %s", st)
	}
}
