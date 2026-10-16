package iterm2

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"

	"google.golang.org/protobuf/proto"

	pb "github.com/schleppy/iterm2/internal/iterm2pb"
)

// Screen contents, screen-update notifications, profile colors, and sending
// text. These use GetBuffer, NotificationRequest (NOTIFY_ON_SCREEN_UPDATE),
// GetProfileProperty, Variable (columns, rows), and SendText.

// ColorKind says how a Color is specified.
type ColorKind uint8

const (
	// ColorDefault is the profile's default foreground (for a foreground) or
	// background (for a background) color.
	ColorDefault ColorKind = iota
	// ColorReversedDefault is the other default: the default background used
	// as a foreground, or the default foreground used as a background.
	ColorReversedDefault
	// ColorIndexed is entry Index of the 256-color palette. 0-15 are the
	// profile's ANSI colors.
	ColorIndexed
	// ColorRGB is a 24-bit color.
	ColorRGB
)

// Color is a cell's foreground or background color.
type Color struct {
	Kind    ColorKind
	Index   uint8 // ColorIndexed
	R, G, B uint8 // ColorRGB
}

// Style is a cell's appearance. It is comparable, so equal styles can be
// merged into runs.
type Style struct {
	FG, BG        Color
	Bold          bool
	Faint         bool
	Italic        bool
	Underline     bool
	Strikethrough bool
	Inverse       bool
	Invisible     bool
	Blink         bool

	// URL is the cell's OSC 8 hyperlink target, or "" if it is not a link.
	URL string
	// LinkID is the OSC 8 id= parameter. Cells with the same URL and LinkID
	// belong to one link even when they are not adjacent (for example, a link
	// that wraps or is split by other text).
	LinkID string
}

// StyleRun is Count adjacent cells with the same style.
type StyleRun struct {
	Style Style
	Count int
}

// Line is one line of the screen.
type Line struct {
	// Cells holds the text of each cell: usually one character, more with
	// combining marks, "" for an uninitialized cell. Trailing uninitialized
	// cells are omitted, so a line can be shorter than the screen is wide.
	Cells []string
	// Styles covers Cells in order (run-length encoded). It is empty unless
	// styles were requested.
	Styles []StyleRun
	// Wrapped means the next line continues this one (a soft line break).
	Wrapped bool
}

// Screen is what a session shows: its visible lines and the cursor.
type Screen struct {
	Lines []Line
	// Columns and Rows are the session's grid size.
	Columns, Rows int
	// CursorX and CursorY are the cursor's cell on screen (0-based); CursorY
	// is -1 if the cursor is not on the visible screen.
	CursorX, CursorY int
}

// ScreenOptions controls Client.Screen.
type ScreenOptions struct {
	// Styles includes colors and attributes (Line.Styles).
	Styles bool
}

// Screen reads what session sessionID shows now, with its grid size. It
// returns ErrBadIdentifier if iTerm2 has no such session.
func (c *Client) Screen(ctx context.Context, sessionID string, o ScreenOptions) (*Screen, error) {
	if sessionID == "" || sessionID == "all" {
		return nil, errors.New("iterm2: Screen needs one session ID")
	}
	reqs := []*pb.ClientOriginatedMessage{
		{Submessage: &pb.ClientOriginatedMessage_GetBufferRequest{GetBufferRequest: &pb.GetBufferRequest{
			Session:       proto.String(sessionID),
			LineRange:     &pb.LineRange{ScreenContentsOnly: proto.Bool(true)},
			IncludeStyles: proto.Bool(o.Styles),
		}}},
		sessionVariable(sessionID, "columns"),
		sessionVariable(sessionID, "rows"),
	}
	res := c.callMany(ctx, reqs)
	for _, r := range res {
		if r.err != nil {
			return nil, r.err
		}
	}
	buf := res[0].msg.GetGetBufferResponse()
	if buf == nil {
		return nil, fmt.Errorf("iterm2: unexpected response to get-buffer request: %T", res[0].msg.GetSubmessage())
	}
	switch st := buf.GetStatus(); st {
	case pb.GetBufferResponse_OK:
	case pb.GetBufferResponse_SESSION_NOT_FOUND:
		return nil, ErrBadIdentifier
	default:
		return nil, fmt.Errorf("iterm2: get-buffer failed: %s", st)
	}
	s := &Screen{Lines: make([]Line, 0, len(buf.GetContents())), CursorY: -1}
	for _, lc := range buf.GetContents() {
		s.Lines = append(s.Lines, lineFromProto(lc))
	}
	if cur := buf.GetCursor(); cur != nil {
		first := buf.GetWindowedCoordRange().GetCoordRange().GetStart().GetY()
		if y := cur.GetY() - first; y >= 0 && y < int64(len(s.Lines)) {
			s.CursorX, s.CursorY = int(cur.GetX()), int(y)
		}
	}
	s.Columns = intVariable(res[1])
	s.Rows = intVariable(res[2])
	if s.Rows == 0 {
		s.Rows = len(s.Lines)
	}
	if s.Columns == 0 {
		for _, l := range s.Lines {
			s.Columns = max(s.Columns, len(l.Cells))
		}
	}
	return s, nil
}

func sessionVariable(sessionID, name string) *pb.ClientOriginatedMessage {
	return &pb.ClientOriginatedMessage{Submessage: &pb.ClientOriginatedMessage_VariableRequest{
		VariableRequest: &pb.VariableRequest{
			Scope: &pb.VariableRequest_SessionId{SessionId: sessionID},
			Get:   []string{name},
		},
	}}
}

// intVariable decodes a numeric variable response; 0 if it is missing.
func intVariable(r result) int {
	vr := r.msg.GetVariableResponse()
	if vr.GetStatus() != pb.VariableResponse_OK || len(vr.GetValues()) == 0 {
		return 0
	}
	var n float64
	if err := json.Unmarshal([]byte(vr.GetValues()[0]), &n); err != nil || n < 0 || n > math.MaxInt32 {
		return 0
	}
	return int(n)
}

func lineFromProto(lc *pb.LineContents) Line {
	runes := []rune(lc.GetText())
	l := Line{Wrapped: lc.GetContinuation() == pb.LineContents_CONTINUATION_SOFT_EOL}
	cpps := lc.GetCodePointsPerCell()
	if len(cpps) == 0 {
		// One code point per cell.
		l.Cells = make([]string, len(runes))
		for i, r := range runes {
			l.Cells[i] = cellText(string(r))
		}
	}
	i := 0
	for _, cpp := range cpps {
		n := int(cpp.GetNumCodePoints())
		for range cpp.GetRepeats() {
			end := min(i+n, len(runes))
			l.Cells = append(l.Cells, cellText(string(runes[i:end])))
			i = end
		}
	}
	for _, st := range lc.GetStyle() {
		n := int(st.GetRepeats())
		if n <= 0 {
			n = 1
		}
		s := styleFromProto(st)
		if k := len(l.Styles) - 1; k >= 0 && l.Styles[k].Style == s {
			l.Styles[k].Count += n
		} else {
			l.Styles = append(l.Styles, StyleRun{Style: s, Count: n})
		}
	}
	return l
}

// cellText normalizes a cell: iTerm2 sends an uninitialized cell inside a line (where a program
// moved the cursor instead of writing spaces) as a NUL code point, not as a cell with none.
func cellText(s string) string {
	if s == "\x00" {
		return ""
	}
	return s
}

func styleFromProto(st *pb.CellStyle) Style {
	s := Style{
		Bold: st.GetBold(), Faint: st.GetFaint(), Italic: st.GetItalic(), Underline: st.GetUnderline(),
		Strikethrough: st.GetStrikethrough(), Inverse: st.GetInverse(), Invisible: st.GetInvisible(), Blink: st.GetBlink(),
		URL: st.GetUrl().GetUrl(), LinkID: st.GetUrl().GetIdentifier(),
	}
	switch c := st.GetFgColor().(type) {
	case *pb.CellStyle_FgStandard:
		s.FG = Color{Kind: ColorIndexed, Index: uint8(c.FgStandard)}
	case *pb.CellStyle_FgRgb:
		s.FG = rgb(c.FgRgb)
	case *pb.CellStyle_FgAlternate:
		s.FG = alternate(c.FgAlternate)
	}
	switch c := st.GetBgColor().(type) {
	case *pb.CellStyle_BgStandard:
		s.BG = Color{Kind: ColorIndexed, Index: uint8(c.BgStandard)}
	case *pb.CellStyle_BgRgb:
		s.BG = rgb(c.BgRgb)
	case *pb.CellStyle_BgAlternate:
		s.BG = alternate(c.BgAlternate)
	}
	return s
}

func rgb(c *pb.RGBColor) Color {
	return Color{Kind: ColorRGB, R: uint8(c.GetRed()), G: uint8(c.GetGreen()), B: uint8(c.GetBlue())}
}

func alternate(a pb.AlternateColor) Color {
	if a == pb.AlternateColor_REVERSED_DEFAULT {
		return Color{Kind: ColorReversedDefault}
	}
	return Color{} // DEFAULT, SYSTEM_MESSAGE
}

// RGB is a color from a profile.
type RGB struct{ R, G, B uint8 }

// Palette is a session's profile colors. Missing entries are zero; Has says
// which were set.
type Palette struct {
	Foreground, Background, Cursor, Bold, Selection RGB
	ANSI                                            [16]RGB
	Has                                             map[string]bool // "foreground", "background", "cursor", "bold", "selection", "ansi"
}

var paletteKeys = func() []string {
	keys := []string{"Foreground Color", "Background Color", "Cursor Color", "Bold Color", "Selection Color"}
	for i := range 16 {
		keys = append(keys, "Ansi "+strconv.Itoa(i)+" Color")
	}
	return keys
}()

// ProfileColors reads the colors of session sessionID's profile. It returns
// ErrBadIdentifier if iTerm2 has no such session.
func (c *Client) ProfileColors(ctx context.Context, sessionID string) (*Palette, error) {
	r, err := c.call(ctx, &pb.ClientOriginatedMessage{Submessage: &pb.ClientOriginatedMessage_GetProfilePropertyRequest{
		GetProfilePropertyRequest: &pb.GetProfilePropertyRequest{Session: proto.String(sessionID), Keys: paletteKeys},
	}})
	if err != nil {
		return nil, err
	}
	resp := r.msg.GetGetProfilePropertyResponse()
	if resp == nil {
		return nil, fmt.Errorf("iterm2: unexpected response to get-profile-property request: %T", r.msg.GetSubmessage())
	}
	switch st := resp.GetStatus(); st {
	case pb.GetProfilePropertyResponse_OK:
	case pb.GetProfilePropertyResponse_SESSION_NOT_FOUND:
		return nil, ErrBadIdentifier
	default:
		return nil, fmt.Errorf("iterm2: get-profile-property failed: %s", st)
	}
	p := &Palette{Has: map[string]bool{}}
	ansi := 0
	for _, prop := range resp.GetProperties() {
		col, ok := parseProfileColor(prop.GetJsonValue())
		if !ok {
			continue
		}
		switch k := prop.GetKey(); k {
		case "Foreground Color":
			p.Foreground, p.Has["foreground"] = col, true
		case "Background Color":
			p.Background, p.Has["background"] = col, true
		case "Cursor Color":
			p.Cursor, p.Has["cursor"] = col, true
		case "Bold Color":
			p.Bold, p.Has["bold"] = col, true
		case "Selection Color":
			p.Selection, p.Has["selection"] = col, true
		default:
			var i int
			if _, err := fmt.Sscanf(k, "Ansi %d Color", &i); err == nil && i >= 0 && i < 16 {
				p.ANSI[i] = col
				ansi++
			}
		}
	}
	p.Has["ansi"] = ansi == 16
	return p, nil
}

// parseProfileColor decodes iTerm2's color dictionary
// ({"Red Component": 0.5, ...}, components 0-1).
func parseProfileColor(js string) (RGB, bool) {
	var v struct {
		R *float64 `json:"Red Component"`
		G *float64 `json:"Green Component"`
		B *float64 `json:"Blue Component"`
	}
	if err := json.Unmarshal([]byte(js), &v); err != nil || v.R == nil || v.G == nil || v.B == nil {
		return RGB{}, false
	}
	c := func(f float64) uint8 { return uint8(math.Round(math.Max(0, math.Min(1, f)) * 255)) }
	return RGB{c(*v.R), c(*v.G), c(*v.B)}, true
}

// SendText types text into session sessionID as if from the keyboard. It is
// not broadcast to other sessions when input broadcasting is on. It returns
// ErrBadIdentifier if iTerm2 has no such session.
func (c *Client) SendText(ctx context.Context, sessionID, text string) error {
	if sessionID == "" || sessionID == "all" {
		return errors.New("iterm2: SendText needs one session ID")
	}
	r, err := c.call(ctx, &pb.ClientOriginatedMessage{Submessage: &pb.ClientOriginatedMessage_SendTextRequest{
		SendTextRequest: &pb.SendTextRequest{Session: proto.String(sessionID), Text: proto.String(text), SuppressBroadcast: proto.Bool(true)},
	}})
	if err != nil {
		return err
	}
	resp := r.msg.GetSendTextResponse()
	if resp == nil {
		return fmt.Errorf("iterm2: unexpected response to send-text request: %T", r.msg.GetSubmessage())
	}
	switch st := resp.GetStatus(); st {
	case pb.SendTextResponse_OK:
		return nil
	case pb.SendTextResponse_SESSION_NOT_FOUND:
		return ErrBadIdentifier
	default:
		return fmt.Errorf("iterm2: send-text failed: %s", st)
	}
}

// SubscribeScreen asks for EventScreenUpdated whenever one of the sessions'
// screens changes. A session iTerm2 doesn't have yields ErrBadIdentifier
// (the others are still subscribed).
func (c *Client) SubscribeScreen(ctx context.Context, sessionIDs ...string) error {
	return c.screenRequest(ctx, true, sessionIDs)
}

// UnsubscribeScreen stops screen updates for the sessions. Sessions that are
// not subscribed (or no longer exist) are ignored.
func (c *Client) UnsubscribeScreen(ctx context.Context, sessionIDs ...string) error {
	return c.screenRequest(ctx, false, sessionIDs)
}

func (c *Client) screenRequest(ctx context.Context, subscribe bool, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	reqs := make([]*pb.ClientOriginatedMessage, len(ids))
	for i, id := range ids {
		reqs[i] = &pb.ClientOriginatedMessage{Submessage: &pb.ClientOriginatedMessage_NotificationRequest{
			NotificationRequest: &pb.NotificationRequest{
				Session:          proto.String(id),
				Subscribe:        proto.Bool(subscribe),
				NotificationType: pb.NotificationType_NOTIFY_ON_SCREEN_UPDATE.Enum(),
			},
		}}
	}
	var firstErr error
	for _, r := range c.callMany(ctx, reqs) {
		err := r.err
		if err == nil {
			nr := r.msg.GetNotificationResponse()
			switch st := nr.GetStatus(); {
			case nr == nil:
				err = fmt.Errorf("iterm2: unexpected response to notification request: %T", r.msg.GetSubmessage())
			case st == pb.NotificationResponse_OK,
				subscribe && st == pb.NotificationResponse_ALREADY_SUBSCRIBED,
				!subscribe && (st == pb.NotificationResponse_NOT_SUBSCRIBED || st == pb.NotificationResponse_SESSION_NOT_FOUND):
			case st == pb.NotificationResponse_SESSION_NOT_FOUND, st == pb.NotificationResponse_INVALID_IDENTIFIER:
				err = ErrBadIdentifier
			default:
				err = &SubscriptionError{Event: EventScreenUpdated, Status: st.String()}
			}
		}
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}
