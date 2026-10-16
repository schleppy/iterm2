package iterm2

import (
	"fmt"
	"strconv"
	"strings"
)

// TermSessionID is a parsed ITERM_SESSION_ID (also exported as
// TERM_SESSION_ID) value, which has the form "w1t0p0:C4EE8210-4AD8-...".
// iTerm2 builds it in PTYSession.m:-sessionId.
type TermSessionID struct {
	// Window, Tab, and Pane are the numbers at the time the session started.
	// They change when panes move, so do not use them for matching. They are
	// -1 when the value was a bare session ID.
	Window, Tab, Pane int

	// ID is the session GUID. It equals Session.ID from the API.
	ID string
}

// ParseSessionID parses an ITERM_SESSION_ID value. It also accepts a bare
// session ID as returned by the API.
func ParseSessionID(v string) (TermSessionID, error) {
	v = strings.TrimSpace(v)
	loc, id, hasLoc := strings.Cut(v, ":")
	if !hasLoc {
		id = loc
	}
	if id == "" || strings.ContainsAny(id, " \t:") {
		return TermSessionID{}, fmt.Errorf("iterm2: invalid session id %q", v)
	}
	out := TermSessionID{Window: -1, Tab: -1, Pane: -1, ID: id}
	if !hasLoc {
		return out, nil
	}
	var ok bool
	if out.Window, out.Tab, out.Pane, ok = parseLocation(loc); !ok {
		return TermSessionID{}, fmt.Errorf("iterm2: invalid session location %q", v)
	}
	return out, nil
}

// parseLocation parses "w<N>t<N>p<N>".
func parseLocation(s string) (w, t, p int, ok bool) {
	nums := [3]int{}
	for i, prefix := range []string{"w", "t", "p"} {
		rest, found := strings.CutPrefix(s, prefix)
		if !found {
			return 0, 0, 0, false
		}
		end := strings.IndexFunc(rest, func(r rune) bool { return r < '0' || r > '9' })
		if end < 0 {
			end = len(rest)
		}
		n, err := strconv.Atoi(rest[:end])
		if err != nil {
			return 0, 0, 0, false
		}
		nums[i], s = n, rest[end:]
	}
	return nums[0], nums[1], nums[2], s == ""
}

// NormalizeSessionID returns the API session ID for an ITERM_SESSION_ID value
// or a bare session ID. It returns "" for invalid input. Compare the results
// with strings.EqualFold, or use SameSession.
func NormalizeSessionID(v string) string {
	id, err := ParseSessionID(v)
	if err != nil {
		return ""
	}
	return id.ID
}

// SameSession reports whether a and b, each an ITERM_SESSION_ID value or a
// session ID, refer to the same iTerm2 session. GUIDs are compared
// case-insensitively.
func SameSession(a, b string) bool {
	na, nb := NormalizeSessionID(a), NormalizeSessionID(b)
	return na != "" && strings.EqualFold(na, nb)
}
