# Correlating Claude Code instances with iTerm2 sessions

## Summary

Correlation is straightforward in the common case: Claude Code runs directly
in an iTerm2 pane, so it inherits `ITERM_SESSION_ID`, and the GUID in that
value is the API's session ID. This package provides `ParseSessionID`,
`NormalizeSessionID`, and `SameSession`. Look a session up with
`snapshot.Session(iterm2.NormalizeSessionID(v))`.

tmux, SSH, and detached processes break the inheritance. Those cases are
documented below; this package does not try to solve them.

## Identifier formats

| Source | Example | Defined in |
|---|---|---|
| `ITERM_SESSION_ID`, `TERM_SESSION_ID` env | `w1t0p0:C4EE8210-4AD8-48A4-8C65-42D426111767` | `PTYSession.m:-sessionId`: `w<window number>t<tab number>p<pane number>:<guid>` |
| API `SessionSummary.unique_identifier` (`Session.ID`) | `C4EE8210-4AD8-48A4-8C65-42D426111767` | `PTYSession.guid` (`iTermAPIHelper.m:-newListSessionsResponse`) |
| Session variable `termid` | `w1t0p0` | iTerm2 variables reference |

**Normalization:** take everything after the first `:`. The `w/t/p` prefix is
computed when the session's process starts. It goes stale when the pane moves
or windows are renumbered. Never match on it. GUIDs are upper-case UUIDs;
`SameSession` compares case-insensitively to be safe.

This was verified live: this repository's development shell had
`ITERM_SESSION_ID=w1t0p0:C4EE8210-…`, and `iterm2-topology` listed session
`C4EE8210-…` in window #1.

## Claude Code hooks

Hook payloads (stdin JSON: `session_id`, `transcript_path`, `cwd`,
`hook_event_name`, …) **do not include** `ITERM_SESSION_ID`. Hook processes
**do inherit** Claude Code's environment, except `OTEL_*` and anything removed
by `CLAUDE_CODE_SUBPROCESS_ENV_SCRUB=1`. So a hook command can add the
variable to what it forwards, for example:

```sh
jq -c --arg iterm "${ITERM_SESSION_ID:-}" '. + {iterm_session_id: $iterm}'
```

The daemon then calls `iterm2.NormalizeSessionID(payload.ITermSessionID)` and
looks the result up in the watcher's snapshot.

The existing daemon and its hook scripts are not in this repository, so I could
not check whether they already capture the variable. That check, and the
one-line hook change if it's needed, is the only integration work left. A
`SessionStart` hook is the natural place for it, because the value does not
change for the life of the process.

## Edge cases

| Case | Behavior | Recommendation |
|---|---|---|
| Direct child of an iTerm2 shell (incl. subshells, `exec`, `nohup` in the foreground) | Inherits the correct value | Match on GUID |
| Pane moved, tab dragged to another window | GUID unchanged; prefix stale | GUID only |
| Session > Restart session | iTerm2 assigns a **new GUID** (`PTYSession.m:-replaceTerminatedShellWithNewInstance`). The old ID disappears from snapshots | Treat as a new session |
| Process outlives its session (detached, `disown`, launchd) | Variable refers to a terminated session | `Snapshot.Session` returns nil, so treat it as uncorrelated |
| Buried session (Session > Bury) | GUID kept; session is in `Snapshot.BuriedSessions` | `Snapshot.Session` finds it (window and tab are nil) |
| Plain tmux inside iTerm2 | Panes inherit the **tmux server's** environment from whichever iTerm2 session first started the server. tmux's default `update-environment` does not include `ITERM_SESSION_ID`, so the value can be stale or point at another tab. `TMUX` is set | Treat the value as unreliable when `TMUX` is set. A future option is to match `tmux list-clients -F '#{client_tty}'` against each iTerm2 session's `tty` variable |
| tmux integration (`tmux -CC`) | Each tmux pane appears as a native iTerm2 session with its own GUID (`Tab.TmuxConnectionID` is set), but processes run under the tmux server, so they see the gateway session's or a stale value | Same as plain tmux. A future option is to map `$TMUX_PANE` to the session variable `tmuxWindowPane` |
| SSH | The variable is not forwarded unless `SendEnv`/`AcceptEnv` allow it. Remote Claude Code normally has none | Uncorrelated; correlate on the local `ssh` client's session instead if needed |
| `sudo`, `env -i`, containers | Usually stripped (`env_reset`, explicit environment) | Uncorrelated |
| Several Claude Code instances in one pane (sequentially, or a subagent) | Same GUID | The GUID identifies the pane, not the Claude process; use the hook `session_id` to tell instances apart |
| iTerm2 restart with session restoration | Restored sessions appear to keep their GUID (it is restored through `-setGuid:`), but this is **not verified** | Re-resolve after the watcher reconnects; an ID that is no longer found is uncorrelated |
