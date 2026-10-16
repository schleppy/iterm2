package iterm2

import (
	"testing"
)

func TestParseSessionID(t *testing.T) {
	const guid = "C4EE8210-4AD8-48A4-8C65-42D426111767"
	tests := []struct {
		in      string
		want    TermSessionID
		wantErr bool
	}{
		{in: "w1t0p0:" + guid, want: TermSessionID{1, 0, 0, guid}},
		{in: "w12t3p45:" + guid, want: TermSessionID{12, 3, 45, guid}},
		{in: "  w0t0p0:" + guid + "\n", want: TermSessionID{0, 0, 0, guid}},
		{in: guid, want: TermSessionID{-1, -1, -1, guid}}, // API form
		{in: "", wantErr: true},
		{in: "w1t0p0:", wantErr: true},
		{in: "w1t0:" + guid, wantErr: true},
		{in: "x1t0p0:" + guid, wantErr: true},
		{in: "w1t0p0x:" + guid, wantErr: true},
		{in: "w1t0p0:" + guid + ":extra", wantErr: true},
		{in: "two words", wantErr: true},
	}
	for _, tt := range tests {
		got, err := ParseSessionID(tt.in)
		if (err != nil) != tt.wantErr {
			t.Errorf("ParseSessionID(%q) err = %v", tt.in, err)
			continue
		}
		if !tt.wantErr && got != tt.want {
			t.Errorf("ParseSessionID(%q) = %+v, want %+v", tt.in, got, tt.want)
		}
	}
}

func TestNormalizeAndMatch(t *testing.T) {
	const guid = "C4EE8210-4AD8-48A4-8C65-42D426111767"
	if got := NormalizeSessionID("w1t0p0:" + guid); got != guid {
		t.Errorf("NormalizeSessionID = %q", got)
	}
	if got := NormalizeSessionID("garbage value"); got != "" {
		t.Errorf("NormalizeSessionID(garbage) = %q", got)
	}
	// The location prefix is stale after a pane moves; only the GUID matters.
	if !SameSession("w1t0p0:"+guid, "w3t2p1:"+guid) {
		t.Error("same GUID, different location should match")
	}
	if !SameSession("w1t0p0:"+guid, "c4ee8210-4ad8-48a4-8c65-42d426111767") {
		t.Error("match should ignore case")
	}
	if SameSession("w1t0p0:"+guid, "w1t0p0:A72D176D-8779-4398-A4E5-D028AC366FB4") {
		t.Error("different GUIDs matched")
	}
	if SameSession("", "") {
		t.Error("empty values matched")
	}

	// Lookup against a snapshot.
	windows, _ := parseLayout(pLayout(pWindow("w1", 1, "t1", pTab("t1", guid, pSplit(false, pSession(guid, "x"))))))
	snap := Snapshot{Windows: windows}
	if s, _, _ := snap.Session(NormalizeSessionID("w1t0p0:" + guid)); s == nil {
		t.Error("session not found from ITERM_SESSION_ID")
	}
}
