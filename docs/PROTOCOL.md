# iTerm2 API protocol investigation

Findings that the Go client is built on. Python references are to
`iterm2-python/iterm2/` (library 2.30). Server references are to
`gnachman/iTerm2@fe8cbff` (`sources/API/`). Where the Python library and the
server differ, the server is treated as authoritative.

## 1. Connection

### Transport

| Item | Value | Source |
|---|---|---|
| Socket | Unix domain socket `~/Library/Application Support/<suite>/private/socket`, `<suite>` = `$IT2_SUITE` or `iTerm2` | `connection.py:_unix_domain_socket_path`; server `iTermAPIServer.m:+unixSocketPath`, `-listenOnUnixSocket` |
| TCP | Python still falls back to `ws://localhost:1912`, but the current server only listens on the Unix socket and asserts `AF_UNIX` for every connection | `connection.py:_get_tcp_connect_coro`; `iTermAPIServer.m:555` |
| Protocol | WebSocket (RFC 6455) upgrade on path `/`. Binary frames | `iTermAPIServer.m` (rejects any other path) |
| Subprotocol | `Sec-WebSocket-Protocol` must equal exactly `api.iterm2.com` | `connection.py:_subprotocols`; `iTermWebSocketConnection.m:kProtocolName` |
| Ping | Python disables client pings (`ping_interval=None`). The server answers pings | `connection.py` |
| Message size | Python sets `max_size=None` on the Unix socket. `ListSessionsResponse` grows with the number of sessions | `connection.py:_get_unix_connect_coro` |

### Handshake headers

All checks are in `iTermWebSocketConnection.m:+newWebSocketConnectionForRequest:`.
The server lower-cases header names (`iTermHTTPConnection.m:201`).

| Header | Requirement |
|---|---|
| `origin` | Required. Its host must be `localhost`. Python sends `ws://localhost/` |
| `host` | Must be `localhost`, `127.0.0.1` or `[::1]` |
| `x-iterm2-library-version` | **Required.** It must have the form `"<name> <version>"` (exactly one space). Only `python` has a minimum (`0.24`). A Python version below the minimum gets HTTP 406. Any other name passes. Non-`python` names are also treated as understanding floating panes (`iTermAPIHelper.m:+libraryVersionUnderstandsFloatingPanes:`). The upstream Swift `it2` CLI sends `swift 1.0` |
| `x-iterm2-cookie` | Optional, single-use (`iTermWebSocketCookieJar consumeCookie:`). A valid cookie preauthorizes the connection |
| `x-iterm2-key` | Optional. Identifies the connection in the Script Console and keys server-side subscriptions. A random UUID is used if absent |
| `x-iterm2-advisory-name` | Optional display name |
| `x-iterm2-disable-auth-ui` | If present, the server never shows the "Allow connection?" dialog. It returns 401 instead. Python always sends it |

Response: `101` with `X-iTerm2-Protocol-Version: 1.23`
(`iTermWebSocketConnection.m:-sendUpgradeResponseWithKey:`).

### Authorization

The server-side decision is `iTermAPIHelper.m:-apiServerAuthorizeProcesses:...`:

1. A valid cookie means the connection is allowed ("Script launched by user action").
2. Otherwise, if *Settings › General › Magic › Allow all apps to connect* is enabled
   (`requireApplescriptAuth == NO`), the connection is allowed.
3. Otherwise, if `x-iterm2-disable-auth-ui` is set, the server returns **401**.
4. Otherwise it shows a modal dialog.

Python gets a cookie by running AppleScript (`auth.py:request_cookie_and_key`):

```applescript
if application "iTerm2" is running then return "yes" else return "no"   -- avoids launching
tell application "iTerm2" to request cookie and key for app named "<name>"
```

The result is `"<cookie> <key>"`. Python skips AppleScript when
`~/Library/Application Support/<suite>/disable-automation-auth` exists, is
root-owned, and contains `hex(path) + " 61DF88DC-3423-4823-B725-22570E01C027"`
(`auth.py:applescript_auth_disabled`). Cookies are single-use, so **every
reconnect needs a fresh cookie**. Python removes `ITERM2_COOKIE`/`ITERM2_KEY`
after each attempt.

Running AppleScript triggers the macOS Automation (TCC) consent prompt once for
the calling app.

### Failure handling in Python (`connection.py:async_connect`)

* **401:** Python requests a fresh cookie once and retries. With `retry=True` it
  polls every 0.5 s.
* **406:** library too old. Fatal.
* **`ConnectionRefusedError`/`OSError`:** iTerm2 is not running or the API is
  disabled. Python polls every 0.5 s if `retry`, otherwise fails.
* There is no reconnect after an established connection drops. The dispatch
  loop ends and the program exits. Reconnecting is left to the caller.

### Framing and correlation

* Each WebSocket binary message is one serialized protobuf: client→server
  `ClientOriginatedMessage`, server→client `ServerOriginatedMessage`.
* The client sets `id` (int64). The server copies it into the response
  (`iTermAPIServer.m:-newResponseForRequest:`). Responses can arrive out of
  order (handlers are asynchronous), so correlation is by `id` only. Multiple
  outstanding requests are allowed. Python allocates ids from a process-wide
  counter (`rpc.py:_alloc_id`) and matches with
  `connection.py:async_dispatch_until_id`.
* A response with the `error` oneof set means the request was malformed
  (`rpc.py:_async_call` raises `RPCException`).
* Notifications are `ServerOriginatedMessage.notification` (field 1000) with
  **no id**. Python routes any message that no pending receiver claims to the
  notification helpers (`connection.py:_async_dispatch_forever`).
* Transactions (`TransactionRequest`) lock the whole app. They are not needed
  here and not implemented.

## 2. Schema

* Authoritative schema: `proto/api.proto` in `gnachman/iTerm2`. It is proto2,
  package `iterm2`, and has no `go_package`. The Python package ships only the
  generated `api_pb2.py`/`.pyi` (embedded serialized descriptor), not the
  `.proto` file.
* Go bindings: `protoc` + `protoc-gen-go` with
  `--go_opt=Miterm2/api.proto=<import path>` (see `Makefile`, `proto/UPSTREAM.md`).
* Versioning: there is no in-band negotiation. The server reports
  `X-iTerm2-Protocol-Version` and Python reads it
  (`connection.py:iterm2_protocol_version`) to gate features. Relevant
  thresholds: `Window.selected_tab_id` and `Tab.active_session_id` were added in
  1.18, floating panes in 1.21. Older servers leave these fields unset, so
  focus must also be fetched with `FocusRequest`.

## 3. Operations used

| Need | RPC / notification | Python reference |
|---|---|---|
| Topology | `ListSessionsRequest{}` → `ListSessionsResponse{windows[], buried_sessions[]}` | `rpc.py:async_list_sessions`, `app.py:App.async_construct` |
| Tab layout | `Tab.root: SplitTreeNode{vertical, links[]}`, with each link a `SessionSummary` or a nested node. Also `Tab.minimized_sessions` (maximized tab) and `Tab.floating_panes` (1.21+, not in `root`) | `session.py:Splitter.from_node`, `window.py:create_from_proto` |
| Session name | `SessionSummary.title` (server sets it to `session.name`) | `session.py:Session.__init__` |
| Working directory | `VariableRequest{session_id, get:["path"]}` → `VariableResponse.values[]` (JSON, `null` if unset). This takes one RPC per session; `"all"` is rejected for gets | `rpc.py:async_variable`, `session.py:async_get_variable` |
| Focus | `FocusRequest{}` → `FocusResponse.notifications[]`: the app-active flag, the current window (status key/current/resigned), one `selected_tab` per window, and one `session` per tab | `rpc.py:async_get_focus_info`, `app.py:async_refresh_focus`, server `iTermAPIHelper.m:-apiServerFocus:` |
| Subscribe | `NotificationRequest{session:"all", subscribe:true, notification_type}` → `NotificationResponse.status`. `OK` and `ALREADY_SUBSCRIBED` are success. Anything else is an error | `notifications.py:_async_subscribe` |
| Unsubscribe | Same request with `subscribe:false`. `OK` is success | `notifications.py:async_unsubscribe` |
| Session created | `NOTIFY_ON_NEW_SESSION` → `NewSessionNotification{session_id}` | `async_subscribe_to_new_session_notification` |
| Session ended | `NOTIFY_ON_TERMINATE_SESSION` → `TerminateSessionNotification{session_id}` | `async_subscribe_to_terminate_session_notification` |
| Layout | `NOTIFY_ON_LAYOUT_CHANGE` → `LayoutChangedNotification{list_sessions_response}`, which carries a **full topology**. The server coalesces changes on the main queue and also posts on session resize (`iTermAPIHelper.m:-handleLayoutChange`, `-sessionDidResize:`) | `async_subscribe_to_layout_change_notification` |
| Focus change | `NOTIFY_ON_FOCUS_CHANGE` → `FocusChangedNotification` (oneof `application_active`, `window`, `selected_tab`, `session`) | `app.py:_async_focus_change` |

### What the Python `App` does (behavior, not structure)

* **Startup** (`App.async_construct`): `ListSessions`, then subscribe to
  new/terminate/layout/focus/broadcast, then `FocusRequest`. Python subscribes
  *after* the first list, so it can miss changes in between. The Go watcher
  subscribes first.
* **New/terminate session:** these trigger a full refresh (`async_refresh`:
  `ListSessions` + `FocusRequest`).
* **Layout change:** Python rebuilds the hierarchy from the embedded
  `ListSessionsResponse`, then calls `FocusRequest`.
* **Focus change:** applied incrementally. A `window` event with any status
  except `TERMINAL_WINDOW_RESIGNED_KEY` sets the current window. If
  `selected_tab` or `session` names an unknown tab or session, Python does a
  full refresh, guarded against recursion when the server reports an empty
  window.
* **Server-side cleanup:** subscriptions are keyed by connection and removed
  when it closes (`iTermAPIHelper.m:-removeAllSubscriptionsForConnectionKey:`).
  After a reconnect, the client must subscribe again.

## 4. Special initialization / extra RPCs

* There is no session-level handshake RPC. The first request can be sent
  right after the upgrade.
* Focus state needs `FocusRequest` in addition to `ListSessions`, because
  `current window` and `app active` are not in `ListSessionsResponse`.
* The working directory needs a `VariableRequest` per session. No notification
  fires on a directory change unless you subscribe to
  `NOTIFY_ON_VARIABLE_CHANGE` per variable. That is deliberately out of scope
  here; periodic reconciliation picks up directory changes.
* A `SESSION_NOT_FOUND` variable status is expected when a session closes
  between `ListSessions` and the variable fetch. Treat it as "unknown".

## 5. `ITERM_SESSION_ID`

Set by `PTYSession.m:-sessionId` for every child process (both
`ITERM_SESSION_ID` and `TERM_SESSION_ID`):

```
w<window number>t<tab number>p<pane number>:<session GUID>
e.g. w1t0p0:C4EE8210-4AD8-48A4-8C65-42D426111767
```

The GUID part is exactly `SessionSummary.unique_identifier` returned by the
API. See `docs/CLAUDE_CODE.md` for correlation details and edge cases.
