package iterm2

import (
	"errors"
	"testing"

	"google.golang.org/protobuf/proto"

	pb "github.com/schleppy/iterm2/internal/iterm2pb"
)

// createTabOnce runs CreateTab against a one-request server that answers resp and returns the
// request it received.
func createTabOnce(t *testing.T, window string, o TabOptions, resp *pb.CreateTabResponse) (*pb.CreateTabRequest, string, string, error) {
	t.Helper()
	ct, st := newPipe()
	c := startClient(ct, 0)
	defer c.Close()
	got := make(chan *pb.CreateTabRequest, 1)
	go func() {
		req := readRequest(t, st)
		got <- req.GetCreateTabRequest()
		writeMessage(t, st, &pb.ServerOriginatedMessage{Id: req.Id, Submessage: &pb.ServerOriginatedMessage_CreateTabResponse{CreateTabResponse: resp}})
	}()
	tab, session, err := c.CreateTab(ctxT(t), window, o)
	return <-got, tab, session, err
}

func TestCreateTab(t *testing.T) {
	ok := &pb.CreateTabResponse{Status: pb.CreateTabResponse_OK.Enum(), WindowId: proto.String("pty-W"), TabId: proto.Int32(7), SessionId: proto.String("S9")}
	req, tab, session, err := createTabOnce(t, "pty-W", TabOptions{}, ok)
	if err != nil {
		t.Fatal(err)
	}
	if req.GetWindowId() != "pty-W" || req.ProfileName != nil || req.SelectTab != nil {
		t.Errorf("request = %v", req)
	}
	if tab != "7" || session != "S9" {
		t.Errorf("tab, session = %q, %q", tab, session)
	}
	req, _, _, err = createTabOnce(t, "pty-W", TabOptions{Profile: "Work", Background: true}, ok)
	if err != nil || req.GetProfileName() != "Work" || req.SelectTab == nil || req.GetSelectTab() {
		t.Errorf("background request = %v, %v", req, err)
	}
	if len(req.CustomProfileProperties) != 0 {
		t.Errorf("unasked-for profile properties: %v", req.CustomProfileProperties)
	}
}

func TestCreateTabNewWindow(t *testing.T) {
	ok := &pb.CreateTabResponse{Status: pb.CreateTabResponse_OK.Enum(), WindowId: proto.String("pty-N"), TabId: proto.Int32(8), SessionId: proto.String("S1")}
	req, tab, session, err := createTabOnce(t, "", TabOptions{}, ok)
	if err != nil {
		t.Fatal(err)
	}
	if req.WindowId != nil {
		t.Errorf("window_id set to %q: iTerm2 opens a new window only when it is unset", req.GetWindowId())
	}
	if tab != "8" || session != "S1" {
		t.Errorf("tab, session = %q, %q", tab, session)
	}
}

func TestCreateTabDirectoryAndInitialText(t *testing.T) {
	ok := &pb.CreateTabResponse{Status: pb.CreateTabResponse_OK.Enum(), TabId: proto.Int32(1), SessionId: proto.String("S")}
	req, _, _, err := createTabOnce(t, "", TabOptions{Directory: `/tmp/a "b"`, InitialText: "claude --resume"}, ok)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, p := range req.CustomProfileProperties {
		got[p.GetKey()] = p.GetJsonValue()
	}
	want := map[string]string{
		"Custom Directory":  `"Yes"`,
		"Working Directory": `"/tmp/a \"b\""`,
		"Initial Text":      `"claude --resume"`,
	}
	if len(got) != len(want) {
		t.Errorf("properties = %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %s, want %s", k, got[k], v)
		}
	}
}

func TestCreateTabErrors(t *testing.T) {
	_, _, _, err := createTabOnce(t, "gone", TabOptions{}, &pb.CreateTabResponse{Status: pb.CreateTabResponse_INVALID_WINDOW_ID.Enum()})
	if !errors.Is(err, ErrBadIdentifier) {
		t.Fatalf("missing window: %v", err)
	}
	_, _, _, err = createTabOnce(t, "pty-W", TabOptions{Profile: "nope"}, &pb.CreateTabResponse{Status: pb.CreateTabResponse_INVALID_PROFILE_NAME.Enum()})
	if err == nil || errors.Is(err, ErrBadIdentifier) {
		t.Fatalf("bad profile: %v", err)
	}
}
