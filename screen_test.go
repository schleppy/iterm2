package iterm2

import (
	"errors"
	"reflect"
	"testing"

	"google.golang.org/protobuf/proto"

	pb "github.com/schleppy/iterm2/internal/iterm2pb"
)

func TestScreen(t *testing.T) {
	ct, st := newPipe()
	c := startClient(ct, 0)
	defer c.Close()
	go func() {
		// Screen pipelines get-buffer, then the columns and rows variables.
		buf := readRequest(t, st)
		cols, rows := readRequest(t, st), readRequest(t, st)
		if g := buf.GetGetBufferRequest(); g.GetSession() != "S1" || !g.GetIncludeStyles() || !g.GetLineRange().GetScreenContentsOnly() {
			t.Errorf("get-buffer request = %v", g)
		}
		if cols.GetVariableRequest().GetGet()[0] != "columns" || rows.GetVariableRequest().GetGet()[0] != "rows" {
			t.Errorf("variable requests = %v, %v", cols, rows)
		}
		red := &pb.CellStyle{FgColor: &pb.CellStyle_FgStandard{FgStandard: 1}, Bold: proto.Bool(true), Repeats: proto.Uint32(2)}
		writeMessage(t, st, &pb.ServerOriginatedMessage{Id: buf.Id, Submessage: &pb.ServerOriginatedMessage_GetBufferResponse{GetBufferResponse: &pb.GetBufferResponse{
			Contents: []*pb.LineContents{
				{
					// "ok" in bold red, an uninitialized cell, then "ñ" (n + combining tilde) on true-color blue.
					Text: proto.String("okñ"),
					CodePointsPerCell: []*pb.CodePointsPerCell{
						{NumCodePoints: proto.Int32(1), Repeats: proto.Int32(2)},
						{NumCodePoints: proto.Int32(0), Repeats: proto.Int32(1)},
						{NumCodePoints: proto.Int32(2), Repeats: proto.Int32(1)},
					},
					Style: []*pb.CellStyle{red, {Repeats: proto.Uint32(1)},
						{BgColor: &pb.CellStyle_BgRgb{BgRgb: &pb.RGBColor{Red: proto.Uint32(0), Green: proto.Uint32(0), Blue: proto.Uint32(255)}}, Repeats: proto.Uint32(1)}},
					Continuation: pb.LineContents_CONTINUATION_SOFT_EOL.Enum(),
				},
				{Text: proto.String("$ ")},
				// Cursor movement leaves NUL cells between words.
				{Text: proto.String("a\x00b"), CodePointsPerCell: []*pb.CodePointsPerCell{{NumCodePoints: proto.Int32(1), Repeats: proto.Int32(3)}}},
			},
			Cursor:             &pb.Coord{X: proto.Int32(2), Y: proto.Int64(101)},
			WindowedCoordRange: &pb.WindowedCoordRange{CoordRange: &pb.CoordRange{Start: &pb.Coord{Y: proto.Int64(100)}}},
		}}})
		writeMessage(t, st, variableResponse(cols.GetId(), "80"))
		writeMessage(t, st, variableResponse(rows.GetId(), "24"))
	}()
	s, err := c.Screen(ctxT(t), "S1", ScreenOptions{Styles: true})
	if err != nil {
		t.Fatal(err)
	}
	if s.Columns != 80 || s.Rows != 24 || s.CursorX != 2 || s.CursorY != 1 || len(s.Lines) != 3 {
		t.Fatalf("screen = %+v", s)
	}
	l := s.Lines[0]
	if !reflect.DeepEqual(l.Cells, []string{"o", "k", "", "ñ"}) || !l.Wrapped || s.Lines[1].Wrapped {
		t.Fatalf("line 0 = %+v", l)
	}
	want := []StyleRun{
		{Style{FG: Color{Kind: ColorIndexed, Index: 1}, Bold: true}, 2},
		{Style{}, 1},
		{Style{BG: Color{Kind: ColorRGB, B: 255}}, 1},
	}
	if !reflect.DeepEqual(l.Styles, want) {
		t.Fatalf("styles = %+v", l.Styles)
	}
	if !reflect.DeepEqual(s.Lines[1].Cells, []string{"$", " "}) {
		t.Fatalf("line 1 = %+v", s.Lines[1])
	}
	if !reflect.DeepEqual(s.Lines[2].Cells, []string{"a", "", "b"}) {
		t.Fatalf("NUL cells = %q", s.Lines[2].Cells)
	}
}

func TestScreenHyperlinks(t *testing.T) {
	link := func(url, id string, n uint32) *pb.CellStyle {
		return &pb.CellStyle{Url: &pb.URL{Url: proto.String(url), Identifier: proto.String(id)}, Repeats: proto.Uint32(n)}
	}
	// "go ab cd": "go " plain, "ab" and "cd" links to the same URL. "ab" is
	// sent as two style entries (iTerm2 may split runs), which merge; "cd" has
	// a different id, so it is a separate link and a separate run.
	l := lineFromProto(&pb.LineContents{
		Text: proto.String("go ab cd"),
		Style: []*pb.CellStyle{
			{Repeats: proto.Uint32(3)},
			link("https://x.test", "1", 1), link("https://x.test", "1", 1),
			{Repeats: proto.Uint32(1)},
			link("https://x.test", "2", 2),
		},
	})
	want := []StyleRun{
		{Style{}, 3},
		{Style{URL: "https://x.test", LinkID: "1"}, 2},
		{Style{}, 1},
		{Style{URL: "https://x.test", LinkID: "2"}, 2},
	}
	if !reflect.DeepEqual(l.Styles, want) {
		t.Fatalf("styles = %+v", l.Styles)
	}
}

func TestScreenSessionNotFound(t *testing.T) {
	ct, st := newPipe()
	c := startClient(ct, 0)
	defer c.Close()
	go func() {
		buf, cols, rows := readRequest(t, st), readRequest(t, st), readRequest(t, st)
		writeMessage(t, st, &pb.ServerOriginatedMessage{Id: buf.Id, Submessage: &pb.ServerOriginatedMessage_GetBufferResponse{
			GetBufferResponse: &pb.GetBufferResponse{Status: pb.GetBufferResponse_SESSION_NOT_FOUND.Enum()}}})
		for _, r := range []*pb.ClientOriginatedMessage{cols, rows} {
			writeMessage(t, st, &pb.ServerOriginatedMessage{Id: r.Id, Submessage: &pb.ServerOriginatedMessage_VariableResponse{
				VariableResponse: &pb.VariableResponse{Status: pb.VariableResponse_SESSION_NOT_FOUND.Enum()}}})
		}
	}()
	if _, err := c.Screen(ctxT(t), "gone", ScreenOptions{}); !errors.Is(err, ErrBadIdentifier) {
		t.Fatalf("err = %v", err)
	}
}

func TestSendText(t *testing.T) {
	for _, tc := range []struct {
		status pb.SendTextResponse_Status
		want   error
	}{{pb.SendTextResponse_OK, nil}, {pb.SendTextResponse_SESSION_NOT_FOUND, ErrBadIdentifier}} {
		ct, st := newPipe()
		c := startClient(ct, 0)
		go func() {
			req := readRequest(t, st)
			if r := req.GetSendTextRequest(); r.GetSession() != "S1" || r.GetText() != "ls\r" || !r.GetSuppressBroadcast() {
				t.Errorf("send-text request = %v", r)
			}
			writeMessage(t, st, &pb.ServerOriginatedMessage{Id: req.Id, Submessage: &pb.ServerOriginatedMessage_SendTextResponse{
				SendTextResponse: &pb.SendTextResponse{Status: tc.status.Enum()}}})
		}()
		if err := c.SendText(ctxT(t), "S1", "ls\r"); !errors.Is(err, tc.want) {
			t.Errorf("status %s: err = %v", tc.status, err)
		}
		c.Close()
	}
}

func TestSubscribeScreenAndEvents(t *testing.T) {
	ct, st := newPipe()
	c := startClient(ct, 0)
	defer c.Close()
	go func() {
		a, b := readRequest(t, st), readRequest(t, st)
		for _, r := range []*pb.ClientOriginatedMessage{a, b} {
			nr := r.GetNotificationRequest()
			if nr.GetNotificationType() != pb.NotificationType_NOTIFY_ON_SCREEN_UPDATE || !nr.GetSubscribe() {
				t.Errorf("notification request = %v", nr)
			}
			status := pb.NotificationResponse_OK
			if nr.GetSession() == "gone" {
				status = pb.NotificationResponse_SESSION_NOT_FOUND
			}
			writeMessage(t, st, &pb.ServerOriginatedMessage{Id: r.Id, Submessage: &pb.ServerOriginatedMessage_NotificationResponse{
				NotificationResponse: &pb.NotificationResponse{Status: status.Enum()}}})
		}
		writeMessage(t, st, &pb.ServerOriginatedMessage{Submessage: &pb.ServerOriginatedMessage_Notification{Notification: &pb.Notification{
			ScreenUpdateNotification: &pb.ScreenUpdateNotification{Session: proto.String("S1")}}}})
	}()
	if err := c.SubscribeScreen(ctxT(t), "S1", "gone"); !errors.Is(err, ErrBadIdentifier) {
		t.Fatalf("err = %v", err)
	}
	select {
	case ev := <-c.Events():
		if ev.Type != EventScreenUpdated || ev.SessionID != "S1" {
			t.Fatalf("event = %+v", ev)
		}
	case <-ctxT(t).Done():
		t.Fatal("no screen event")
	}
}

func TestParseProfileColor(t *testing.T) {
	c, ok := parseProfileColor(`{"Red Component": 1, "Green Component": 0.5, "Blue Component": 0, "Color Space": "sRGB"}`)
	if !ok || c != (RGB{255, 128, 0}) {
		t.Fatalf("color = %v %v", c, ok)
	}
	if _, ok := parseProfileColor(`null`); ok {
		t.Fatal("parsed null")
	}
}
