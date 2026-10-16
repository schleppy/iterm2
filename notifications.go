package iterm2

import (
	"context"
	"fmt"

	"google.golang.org/protobuf/proto"

	pb "github.com/schleppy/iterm2/internal/iterm2pb"
)

// EventType identifies a notification.
type EventType int

const (
	// EventSessionCreated is NOTIFY_ON_NEW_SESSION. SessionID is set.
	EventSessionCreated EventType = iota + 1
	// EventSessionTerminated is NOTIFY_ON_TERMINATE_SESSION. SessionID is set.
	EventSessionTerminated
	// EventLayoutChanged is NOTIFY_ON_LAYOUT_CHANGE. It fires when windows,
	// tabs, or panes are added, removed, moved, or resized.
	EventLayoutChanged
	// EventFocusChanged is NOTIFY_ON_FOCUS_CHANGE. Focus is set.
	EventFocusChanged
	// EventResync is generated locally, not by iTerm2. It means events were
	// dropped because the Events channel was full, so the consumer should
	// refresh its state. It cannot be subscribed to.
	EventResync
	// EventScreenUpdated is NOTIFY_ON_SCREEN_UPDATE: the screen of SessionID
	// changed. Subscribe with Client.SubscribeScreen, not Subscribe.
	EventScreenUpdated
)

// TopologyEvents are the event types needed to track windows, tabs, sessions,
// and focus.
var TopologyEvents = []EventType{
	EventSessionCreated, EventSessionTerminated, EventLayoutChanged, EventFocusChanged,
}

func (t EventType) String() string {
	switch t {
	case EventSessionCreated:
		return "session-created"
	case EventSessionTerminated:
		return "session-terminated"
	case EventLayoutChanged:
		return "layout-changed"
	case EventFocusChanged:
		return "focus-changed"
	case EventResync:
		return "resync"
	case EventScreenUpdated:
		return "screen-updated"
	}
	return fmt.Sprintf("EventType(%d)", int(t))
}

func (t EventType) notificationType() (pb.NotificationType, bool) {
	switch t {
	case EventSessionCreated:
		return pb.NotificationType_NOTIFY_ON_NEW_SESSION, true
	case EventSessionTerminated:
		return pb.NotificationType_NOTIFY_ON_TERMINATE_SESSION, true
	case EventLayoutChanged:
		return pb.NotificationType_NOTIFY_ON_LAYOUT_CHANGE, true
	case EventFocusChanged:
		return pb.NotificationType_NOTIFY_ON_FOCUS_CHANGE, true
	}
	return 0, false
}

// Event is a notification from iTerm2. Treat events as triggers: reconcile
// with Client.Snapshot rather than rebuilding state from events alone.
type Event struct {
	Type EventType

	// SessionID is set for EventSessionCreated, EventSessionTerminated, and
	// EventScreenUpdated.
	SessionID string

	// Focus is set for EventFocusChanged.
	Focus FocusChange

	// seq is the message's arrival order on the connection. It lets the
	// watcher discard events already reflected in a newer snapshot.
	seq uint64
	// layout is the topology carried by EventLayoutChanged.
	layout *pb.ListSessionsResponse
}

// FocusKind says which field of a FocusChange is meaningful.
type FocusKind int

const (
	// FocusApp: AppActive is set.
	FocusApp FocusKind = iota + 1
	// FocusWindow: WindowID and WindowStatus are set.
	FocusWindow
	// FocusTab: TabID became the selected tab of its window.
	FocusTab
	// FocusSession: SessionID became the active session of its tab.
	FocusSession
)

// WindowStatus mirrors FocusChangedNotification.Window.WindowStatus.
type WindowStatus int

const (
	// WindowBecameKey: the window became the key window.
	WindowBecameKey WindowStatus = iota
	// WindowIsCurrent: the window is the current terminal window, but some
	// other non-terminal window is key.
	WindowIsCurrent
	// WindowResignedKey: the window is no longer key.
	WindowResignedKey
)

// FocusChange is one FocusChangedNotification.
type FocusChange struct {
	Kind         FocusKind
	AppActive    bool
	WindowID     string
	WindowStatus WindowStatus
	TabID        string
	SessionID    string
}

func focusChangeFromProto(n *pb.FocusChangedNotification) (FocusChange, bool) {
	switch e := n.GetEvent().(type) {
	case *pb.FocusChangedNotification_ApplicationActive:
		return FocusChange{Kind: FocusApp, AppActive: e.ApplicationActive}, true
	case *pb.FocusChangedNotification_Window_:
		return FocusChange{
			Kind:         FocusWindow,
			WindowID:     e.Window.GetWindowId(),
			WindowStatus: WindowStatus(e.Window.GetWindowStatus()),
		}, true
	case *pb.FocusChangedNotification_SelectedTab:
		return FocusChange{Kind: FocusTab, TabID: e.SelectedTab}, true
	case *pb.FocusChangedNotification_Session:
		return FocusChange{Kind: FocusSession, SessionID: e.Session}, true
	}
	return FocusChange{}, false
}

// eventFromNotification converts the notification types this package
// subscribes to and ignores all others.
func eventFromNotification(n *pb.Notification) (Event, bool) {
	switch {
	case n.NewSessionNotification != nil:
		return Event{Type: EventSessionCreated, SessionID: n.NewSessionNotification.GetSessionId()}, true
	case n.TerminateSessionNotification != nil:
		return Event{Type: EventSessionTerminated, SessionID: n.TerminateSessionNotification.GetSessionId()}, true
	case n.LayoutChangedNotification != nil:
		return Event{Type: EventLayoutChanged, layout: n.LayoutChangedNotification.GetListSessionsResponse()}, true
	case n.FocusChangedNotification != nil:
		fc, ok := focusChangeFromProto(n.FocusChangedNotification)
		return Event{Type: EventFocusChanged, Focus: fc}, ok
	case n.ScreenUpdateNotification != nil:
		return Event{Type: EventScreenUpdated, SessionID: n.ScreenUpdateNotification.GetSession()}, true
	}
	return Event{}, false
}

// SubscriptionError means iTerm2 refused a subscription request.
type SubscriptionError struct {
	Event  EventType
	Status string // NotificationResponse.Status name, e.g. "REQUEST_MALFORMED"
}

func (e *SubscriptionError) Error() string {
	return fmt.Sprintf("iterm2: subscribing to %s: %s", e.Event, e.Status)
}

// Subscribe asks iTerm2 to send the given event types (default
// TopologyEvents) on Events. Subscriptions last as long as the connection.
// iTerm2 drops them when the connection closes. Requests are pipelined. The
// first failure is returned, and the other requests still take effect.
func (c *Client) Subscribe(ctx context.Context, types ...EventType) error {
	return c.notificationRequest(ctx, true, types)
}

// Unsubscribe reverses Subscribe.
func (c *Client) Unsubscribe(ctx context.Context, types ...EventType) error {
	return c.notificationRequest(ctx, false, types)
}

func (c *Client) notificationRequest(ctx context.Context, subscribe bool, types []EventType) error {
	if len(types) == 0 {
		types = TopologyEvents
	}
	reqs := make([]*pb.ClientOriginatedMessage, len(types))
	for i, t := range types {
		nt, ok := t.notificationType()
		if !ok {
			return fmt.Errorf("iterm2: cannot subscribe to %s", t)
		}
		reqs[i] = &pb.ClientOriginatedMessage{
			Submessage: &pb.ClientOriginatedMessage_NotificationRequest{
				NotificationRequest: &pb.NotificationRequest{
					// These notification types ignore the session, but Python
					// sends "all", and so does this package.
					Session:          proto.String("all"),
					Subscribe:        proto.Bool(subscribe),
					NotificationType: nt.Enum(),
				},
			},
		}
	}
	var errs []error
	for i, r := range c.callMany(ctx, reqs) {
		if r.err != nil {
			errs = append(errs, r.err)
			continue
		}
		status := r.msg.GetNotificationResponse().GetStatus()
		switch {
		case r.msg.GetNotificationResponse() == nil:
			errs = append(errs, fmt.Errorf("iterm2: unexpected response to notification request: %T", r.msg.GetSubmessage()))
		case status == pb.NotificationResponse_OK,
			subscribe && status == pb.NotificationResponse_ALREADY_SUBSCRIBED:
		default:
			errs = append(errs, &SubscriptionError{Event: types[i], Status: status.String()})
		}
	}
	if len(errs) > 0 {
		return errs[0]
	}
	return nil
}
