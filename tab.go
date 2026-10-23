package iterm2

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"

	"google.golang.org/protobuf/proto"

	pb "github.com/schleppy/iterm2/internal/iterm2pb"
)

// TabOptions says how CreateTab makes a tab.
type TabOptions struct {
	// Profile names the new session's profile; empty means the default profile.
	Profile string
	// Background creates the tab without selecting it or bringing its window forward.
	Background bool
	// Directory starts the session in this directory instead of the profile's.
	Directory string
	// InitialText is typed into the session once it starts, followed by a newline (the
	// profile's "Send text at start"), replacing the profile's own.
	InitialText string
}

// CreateTab opens a new tab in window windowID, the way iTerm2's New Tab menu item does, or
// in a new window if windowID is empty (New Window), and returns the new tab's and session's
// IDs. Unless o.Background, the tab is selected and its window ordered front among iTerm2's
// (the app itself isn't activated). It returns ErrBadIdentifier if iTerm2 has no such window.
func (c *Client) CreateTab(ctx context.Context, windowID string, o TabOptions) (tabID, sessionID string, err error) {
	req := &pb.CreateTabRequest{}
	if windowID != "" {
		req.WindowId = proto.String(windowID)
	}
	if o.Profile != "" {
		req.ProfileName = proto.String(o.Profile)
	}
	if o.Background {
		req.SelectTab = proto.Bool(false)
	}
	if o.Directory != "" {
		req.CustomProfileProperties = append(req.CustomProfileProperties,
			profileProperty("Custom Directory", "Yes"), profileProperty("Working Directory", o.Directory))
	}
	if o.InitialText != "" {
		req.CustomProfileProperties = append(req.CustomProfileProperties, profileProperty("Initial Text", o.InitialText))
	}
	r, err := c.call(ctx, &pb.ClientOriginatedMessage{
		Submessage: &pb.ClientOriginatedMessage_CreateTabRequest{CreateTabRequest: req},
	})
	if err != nil {
		return "", "", err
	}
	resp := r.msg.GetCreateTabResponse()
	if resp == nil {
		return "", "", fmt.Errorf("iterm2: unexpected response to create tab request: %T", r.msg.GetSubmessage())
	}
	switch st := resp.GetStatus(); st {
	case pb.CreateTabResponse_OK, pb.CreateTabResponse_INVALID_TAB_INDEX: // created, just elsewhere
		return strconv.Itoa(int(resp.GetTabId())), resp.GetSessionId(), nil
	case pb.CreateTabResponse_INVALID_WINDOW_ID:
		return "", "", ErrBadIdentifier
	default:
		return "", "", fmt.Errorf("iterm2: create tab failed: %s", st)
	}
}

// profileProperty is a custom profile property setting key to the string v.
func profileProperty(key, v string) *pb.ProfileProperty {
	b, _ := json.Marshal(v) // a string always marshals
	return &pb.ProfileProperty{Key: proto.String(key), JsonValue: proto.String(string(b))}
}
