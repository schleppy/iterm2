package iterm2

import (
	"errors"
	"reflect"
	"testing"

	pb "github.com/schleppy/iterm2/internal/iterm2pb"
)

// splitOnce runs SplitPane against a one-request server that answers with
// status and ids, and returns the request it received.
func splitOnce(t *testing.T, session string, o SplitOptions, status pb.SplitPaneResponse_Status, ids []string) (*pb.SplitPaneRequest, []string, error) {
	t.Helper()
	ct, st := newPipe()
	c := startClient(ct, 0)
	defer c.Close()
	got := make(chan *pb.SplitPaneRequest, 1)
	go func() {
		req := readRequest(t, st)
		got <- req.GetSplitPaneRequest()
		writeMessage(t, st, &pb.ServerOriginatedMessage{Id: req.Id, Submessage: &pb.ServerOriginatedMessage_SplitPaneResponse{
			SplitPaneResponse: &pb.SplitPaneResponse{Status: status.Enum(), SessionId: ids},
		}})
	}()
	out, err := c.SplitPane(ctxT(t), session, o)
	return <-got, out, err
}

func TestSplitPane(t *testing.T) {
	req, ids, err := splitOnce(t, "S1", SplitOptions{Vertical: true}, pb.SplitPaneResponse_OK, []string{"S2"})
	if err != nil {
		t.Fatal(err)
	}
	if req.GetSession() != "S1" || req.GetSplitDirection() != pb.SplitPaneRequest_VERTICAL || req.GetBefore() || req.ProfileName != nil {
		t.Errorf("request = %v", req)
	}
	if !reflect.DeepEqual(ids, []string{"S2"}) {
		t.Errorf("ids = %v", ids)
	}
	req, _, err = splitOnce(t, "S1", SplitOptions{Before: true, Profile: "Work"}, pb.SplitPaneResponse_OK, []string{"S3"})
	if err != nil {
		t.Fatal(err)
	}
	if req.GetSplitDirection() != pb.SplitPaneRequest_HORIZONTAL || !req.GetBefore() || req.GetProfileName() != "Work" {
		t.Errorf("horizontal request = %v", req)
	}
}

func TestSplitPaneErrors(t *testing.T) {
	if _, _, err := splitOnce(t, "gone", SplitOptions{}, pb.SplitPaneResponse_SESSION_NOT_FOUND, nil); !errors.Is(err, ErrBadIdentifier) {
		t.Fatalf("missing session: %v", err)
	}
	if _, _, err := splitOnce(t, "tiny", SplitOptions{}, pb.SplitPaneResponse_CANNOT_SPLIT, nil); !errors.Is(err, ErrCannotSplit) {
		t.Fatalf("too small: %v", err)
	}
	if _, _, err := splitOnce(t, "S1", SplitOptions{Profile: "nope"}, pb.SplitPaneResponse_INVALID_PROFILE_NAME, nil); err == nil || errors.Is(err, ErrBadIdentifier) {
		t.Fatalf("bad profile: %v", err)
	}
}
