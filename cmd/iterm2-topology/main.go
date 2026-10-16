// Command iterm2-topology connects to a running iTerm2 and prints its windows,
// tabs, and sessions. With -watch it keeps running and reprints the topology
// whenever it changes, reconnecting across iTerm2 restarts.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/schleppy/iterm2"
)

func main() {
	var (
		watch   = flag.Bool("watch", false, "keep running and print topology changes")
		splits  = flag.Bool("splits", false, "show the split-pane tree of each tab")
		socket  = flag.String("socket", "", "API socket path (default: iTerm2's Application Support socket)")
		noAuth  = flag.Bool("no-auth", false, `skip the AppleScript cookie request (requires "Allow all apps to connect")`)
		timeout = flag.Duration("timeout", 30*time.Second, "connect and fetch timeout (one-shot mode)")
		verbose = flag.Bool("v", false, "log connection activity to stderr (watch mode)")
	)
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	opts := iterm2.Options{SocketPath: *socket, AppName: "iterm2-topology"}
	if *noAuth {
		opts.Authenticate = iterm2.NoAuth
	}
	p := printer{w: os.Stdout, splits: *splits}

	if !*watch {
		ctx, cancel := context.WithTimeout(ctx, *timeout)
		defer cancel()
		c, err := iterm2.Dial(ctx, opts)
		if err != nil {
			fatal(err)
		}
		defer c.Close()
		snap, err := c.Snapshot(ctx)
		if err != nil {
			fatal(err)
		}
		p.print(snap)
		return
	}

	runWatch(ctx, opts, p, *verbose)
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "iterm2-topology:", err)
	os.Exit(1)
}

type printer struct {
	w      io.Writer
	splits bool
}

func (p printer) print(s *iterm2.Snapshot) {
	focused := s.FocusedSessionID()
	fmt.Fprintf(p.w, "iTerm2 (protocol %s, app active: %t)\n", or(s.ProtocolVersion, "unknown"), s.Focus.AppActive)
	for _, w := range s.Windows {
		fmt.Fprintf(p.w, "Window: %s (#%d)%s\n", w.ID, w.Number, tag(w.ID == s.Focus.CurrentWindowID, "current"))
		for _, t := range w.Tabs {
			fmt.Fprintf(p.w, "  Tab: %s%s%s\n", t.ID, tag(t.ID == w.SelectedTabID, "selected"),
				tag(t.TmuxConnectionID != "", "tmux "+t.TmuxConnectionID))
			byID := map[string]iterm2.Session{}
			for _, sess := range t.Sessions {
				byID[sess.ID] = sess
			}
			if p.splits && t.Root != nil {
				p.split(t.Root, byID, t.ActiveSessionID, focused, "    ")
				for _, sess := range t.Sessions {
					if sess.Minimized || sess.Floating {
						p.session("    ", sess, t.ActiveSessionID, focused)
					}
				}
				continue
			}
			for _, sess := range t.Sessions {
				p.session("    ", sess, t.ActiveSessionID, focused)
			}
		}
	}
	for _, sess := range s.BuriedSessions {
		p.session("", sess, "", "", "buried")
	}
}

func (p printer) split(sp *iterm2.Split, byID map[string]iterm2.Session, active, focused, indent string) {
	dir := "horizontal"
	if sp.Vertical {
		dir = "vertical"
	}
	fmt.Fprintf(p.w, "%sSplit: %s\n", indent, dir)
	for _, c := range sp.Children {
		if c.Split != nil {
			p.split(c.Split, byID, active, focused, indent+"  ")
		} else {
			p.session(indent+"  ", byID[c.SessionID], active, focused)
		}
	}
}

func (p printer) session(indent string, s iterm2.Session, active, focused string, extra ...string) {
	var tags []string
	if s.ID == active {
		tags = append(tags, "active")
	}
	if s.ID == focused {
		tags = append(tags, "focused")
	}
	if s.Minimized {
		tags = append(tags, "minimized")
	}
	if s.Floating {
		tags = append(tags, "floating")
	}
	tags = append(tags, extra...)
	fmt.Fprintf(p.w, "%sSession: %s %q %s%s\n", indent, s.ID, s.Name, or(s.Path, "-"), tag(len(tags) > 0, strings.Join(tags, ", ")))
}

func tag(cond bool, s string) string {
	if !cond {
		return ""
	}
	return " [" + s + "]"
}

func or(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
