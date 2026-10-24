package iterm2

import (
	"context"
	"errors"
	"fmt"

	"google.golang.org/protobuf/proto"

	pb "github.com/schleppy/iterm2/internal/iterm2pb"
)

// Menu item identifiers for SelectMenuItem and MenuItemState. iTerm2 lists them all at
// https://iterm2.com/python-api/menu_ids.html.
const (
	// MenuMaximizeActivePane toggles whether the active pane of iTerm2's current window fills
	// its tab (View > Maximize Active Pane, ⌘⇧↵).
	MenuMaximizeActivePane = "Maximize Active Pane"
)

var (
	// ErrBadMenuItem is returned when iTerm2 has no menu item with the identifier.
	ErrBadMenuItem = errors.New("iterm2: no such menu item")
	// ErrMenuItemDisabled is returned by SelectMenuItem when the menu item is disabled (for
	// Maximize Active Pane: the current tab has a single pane).
	ErrMenuItemDisabled = errors.New("iterm2: menu item is disabled")
)

// MenuState is a menu item's state.
type MenuState struct {
	Checked bool
	Enabled bool
}

// SelectMenuItem invokes a menu item of iTerm2's main menu, as choosing it would. Items that
// act on a window or pane act on iTerm2's current window.
func (c *Client) SelectMenuItem(ctx context.Context, identifier string) error {
	_, err := c.menuItem(ctx, identifier, false)
	return err
}

// MenuItemState reports whether a menu item is checked and enabled, without invoking it.
func (c *Client) MenuItemState(ctx context.Context, identifier string) (MenuState, error) {
	return c.menuItem(ctx, identifier, true)
}

func (c *Client) menuItem(ctx context.Context, identifier string, queryOnly bool) (MenuState, error) {
	r, err := c.call(ctx, &pb.ClientOriginatedMessage{
		Submessage: &pb.ClientOriginatedMessage_MenuItemRequest{MenuItemRequest: &pb.MenuItemRequest{
			Identifier: proto.String(identifier),
			QueryOnly:  proto.Bool(queryOnly),
		}},
	})
	if err != nil {
		return MenuState{}, err
	}
	resp := r.msg.GetMenuItemResponse()
	if resp == nil {
		return MenuState{}, fmt.Errorf("iterm2: unexpected response to menu item request: %T", r.msg.GetSubmessage())
	}
	switch st := resp.GetStatus(); st {
	case pb.MenuItemResponse_OK:
		return MenuState{Checked: resp.GetChecked(), Enabled: resp.GetEnabled()}, nil
	case pb.MenuItemResponse_BAD_IDENTIFIER:
		return MenuState{}, ErrBadMenuItem
	case pb.MenuItemResponse_DISABLED:
		return MenuState{}, ErrMenuItemDisabled
	default:
		return MenuState{}, fmt.Errorf("iterm2: menu item failed: %s", st)
	}
}
