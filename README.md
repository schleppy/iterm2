# iterm2 (Go)

`github.com/schleppy/iterm2` is a minimal native Go client for the iTerm2
scripting API. It discovers and monitors iTerm2 windows, tabs, and split panes
(sessions) and does not need Python at runtime.

Scope:

* Connect to an already-running iTerm2 (it never launches it).
* Take a snapshot of the window → tab → session hierarchy, including the split
  arrangement, session names, working directories, and focus.
* Subscribe to session-created, session-terminated, layout, and focus
  notifications.
* Use `Watcher` to keep a snapshot current, with debouncing, periodic
  reconciliation, and reconnects across iTerm2 restarts.
* Parse `ITERM_SESSION_ID` to correlate processes (for example Claude Code)
  with sessions.

Out of scope: screen contents, keystrokes, sending input, and creating,
closing, or otherwise changing sessions. The client only issues
`ListSessions`, `Focus`, `Variable` (get `path`), and `Notification`
requests.

Docs:

* [docs/PROTOCOL.md](docs/PROTOCOL.md) records the protocol investigation, with
  upstream references.
* [docs/CLAUDE_CODE.md](docs/CLAUDE_CODE.md) covers `ITERM_SESSION_ID`
  correlation and its edge cases.
* [proto/UPSTREAM.md](proto/UPSTREAM.md) pins the schema version and explains
  how to regenerate the bindings.

## Usage

```go
c, err := iterm2.Dial(ctx, iterm2.Options{AppName: "my-daemon"})
if err != nil { ... }
defer c.Close()

snap, err := c.Snapshot(ctx)
for _, w := range snap.Windows {
	for _, t := range w.Tabs {
		for _, s := range t.Sessions {
			fmt.Println(w.ID, t.ID, s.ID, s.Name, s.Path)
		}
	}
}
```

Long-running consumers should use the watcher:

```go
w := iterm2.NewWatcher(iterm2.WatcherOptions{
	Client: iterm2.Options{AppName: "my-daemon"},
	Logger: slog.Default(),
})
go w.Run(ctx) // returns only when ctx is canceled

for snap := range w.Updates() { // latest-value channel; closed when Run returns
	if snap.Stale { /* disconnected; showing last known state */ }
	...
}
current := w.Snapshot() // at any time
```

Map a Claude Code instance to its pane:

```go
s, window, tab := snap.Session(iterm2.NormalizeSessionID(os.Getenv("ITERM_SESSION_ID")))
```

### Low-level API

`Client.Subscribe(ctx, types...)` and `Client.Events()` expose notifications
directly. Events are triggers: reconcile with `Snapshot` rather than building
state from them. The read loop never blocks on the events channel. If the
consumer falls behind, events are dropped and an `EventResync` is delivered
instead. Requests are correlated by id, so any number may be outstanding, and
`Snapshot` pipelines them.

## Authorization

iTerm2 must have **Settings › General › Magic › Enable Python API** turned on.
The setting covers every API client, not just Python. The client connects to the
Unix socket `~/Library/Application Support/iTerm2/private/socket` (honoring
`IT2_SUITE`). Every connection then needs one of the following:

1. **A cookie (default).** `AppleScriptAuth` runs
   `tell application "iTerm2" to request cookie and key for app named "<AppName>"`
   through `/usr/bin/osascript`, as the Python library does. The first time,
   macOS asks the user to allow the calling app (the terminal, or the daemon's
   responsible process) to control iTerm2. Cookies are single-use, so this
   runs on every connection. The socket is dialed *before* AppleScript runs,
   so authentication cannot launch iTerm2.
2. **"Allow all apps to connect"** in the same settings pane. Pass
   `Authenticate: iterm2.NoAuth` to skip AppleScript.

The client sends `x-iterm2-disable-auth-ui`, so iTerm2 never shows its
"Allow this connection?" dialog. An unauthorized connection fails with
`ErrUnauthorized` (HTTP 401). If AppleScript fails, for example because
automation was denied, `Dial` still tries without a cookie and returns
`ErrUnauthorized` joined with the AppleScript error. Connections appear in
iTerm2's Script Console under `AppName`.

## Watcher behavior

* Order on each connection: dial → subscribe → full snapshot → publish.
  Subscribing first means a change during initialization produces an event
  newer than the snapshot. Events are sequence-numbered on arrival, so any
  event the snapshot already reflects is discarded.
* **Focus** notifications are applied incrementally, without RPCs, unless they
  name an unknown tab, session, or window. Those trigger a full refresh, as in
  the Python library.
* **Layout** notifications carry the full topology and are applied directly.
  Only sessions not seen before need a working-directory fetch. Layouts that
  differ only in geometry (resizes) do not publish.
* **New/terminated session** notifications and `EventResync` trigger a full
  refresh. Everything is debounced (default 100 ms, capped at 1 s).
* A full reconcile also runs every 30 s, to pick up names and working
  directories. iTerm2 sends no topology notification for those.
* On disconnect the snapshot is republished with `Stale: true`. Reconnects use
  jittered exponential backoff (500 ms → 30 s). Subscriptions are reestablished
  and a fresh snapshot is published.
* All iTerm2 I/O runs on the `Run` goroutine, one operation at a time.
  The client adds one read goroutine per connection. There is no polling
  besides the reconcile timer.
* Session names and paths are never logged.

## Limitations

* Working directory comes from the session's `path` variable, which is
  accurate with shell integration and best-effort otherwise. Changes are seen
  at the next reconcile, because the client does not subscribe to per-session
  variable-change notifications.
* Split geometry (frames and grid sizes) is not exposed; the split tree is.
* Window focus records the *current terminal window* (`Focus.CurrentWindowID`)
  and whether iTerm2 is active (`Focus.AppActive`). Whether a non-terminal
  iTerm2 window is key is not tracked.
* iTerm2 older than protocol 1.18 does not report selected tabs and active
  sessions in layouts. Those then come only from focus data. The client was
  validated against iTerm2 3.7.4 (protocol 1.18). The schema is from protocol
  1.23.
* Transactions, profiles, and other API areas are not implemented.

## Integration test against a real iTerm2

```sh
go run ./cmd/iterm2-topology            # print the topology and exit
go run ./cmd/iterm2-topology -splits    # include the split-pane tree
go run ./cmd/iterm2-topology -watch -v  # reprint on every change; Ctrl-C to stop
go run ./cmd/iterm2-topology -no-auth   # if "Allow all apps to connect" is on
```

Example output:

```text
iTerm2 (protocol 1.18, app active: true)
Window: pty-B86D0D0F-... (#1) [current]
  Tab: 31 [selected]
    Session: C4EE8210-... "zsh" /Users/me/code [active, focused]
    Session: A72D176D-... "zsh" /Users/me
```

To check notifications and reconnects with `-watch`, open and close a tab,
split a pane, switch tabs and windows, and then quit and relaunch iTerm2. The
snapshot is marked stale and then refreshed after the relaunch.

## Development

```sh
make test    # go test -race ./... (no iTerm2 needed)
make vet
make proto   # regenerate internal/iterm2pb from proto/iterm2/api.proto
```

Unit tests use an in-memory transport and a fake iTerm2 server
(`fake_test.go`). `dial_test.go` exercises the real WebSocket handshake over a
Unix socket.
