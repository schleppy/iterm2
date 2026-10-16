package iterm2

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

// Credentials are the one-time cookie and the connection key that iTerm2
// issues to a script. Both are optional. A cookie is valid for a single
// connection only.
type Credentials struct {
	Cookie string
	Key    string
}

// Authenticator obtains credentials for one connection attempt. appName is the
// name iTerm2 displays for the script.
type Authenticator func(ctx context.Context, appName string) (Credentials, error)

// NoAuth connects without a cookie. iTerm2 accepts such connections only when
// "Settings > General > Magic > Allow all apps to connect" is enabled.
func NoAuth(context.Context, string) (Credentials, error) { return Credentials{}, nil }

// automationAuthMagic and the file check mirror auth.py:applescript_auth_disabled.
const automationAuthMagic = "61DF88DC-3423-4823-B725-22570E01C027"

// AppleScriptAuth requests a cookie from iTerm2 with AppleScript, the same way
// the Python library does:
//
//	tell application "iTerm2" to request cookie and key for app named "<appName>"
//
// It first checks that iTerm2 is running, so it never launches iTerm2. The
// first use triggers the macOS Automation consent prompt for the calling
// process. It returns empty credentials if the administrator has disabled
// AppleScript authentication with the disable-automation-auth file.
//
// Set IT2_APP_PATH to target a specific iTerm2 build.
func AppleScriptAuth(ctx context.Context, appName string) (Credentials, error) {
	if automationAuthDisabled() {
		return Credentials{}, nil
	}
	target := `application "iTerm2"`
	if p := os.Getenv("IT2_APP_PATH"); p != "" {
		target = "application " + appleScriptString(p)
	}

	running, err := runAppleScript(ctx, fmt.Sprintf(`if %s is running then
	return "yes"
else
	return "no"
end if`, target))
	if err != nil {
		return Credentials{}, err
	}
	if running != "yes" {
		return Credentials{}, ErrNotRunning
	}

	out, err := runAppleScript(ctx, fmt.Sprintf("tell %s to request cookie and key for app named %s",
		target, appleScriptString(appName)))
	if err != nil {
		return Credentials{}, err
	}
	cookie, key, ok := strings.Cut(out, " ")
	if !ok || cookie == "" {
		return Credentials{}, errors.New("iterm2: unexpected cookie response from AppleScript")
	}
	return Credentials{Cookie: cookie, Key: key}, nil
}

// runAppleScript is replaced in tests.
var runAppleScript = osascript

func osascript(ctx context.Context, script string) (string, error) {
	cmd := exec.CommandContext(ctx, "/usr/bin/osascript", "-")
	cmd.Stdin = strings.NewReader(script)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", fmt.Errorf("iterm2: osascript: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(stdout.String()), nil
}

func appleScriptString(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	return `"` + strings.ReplaceAll(s, `"`, `\"`) + `"`
}

// automationAuthDisabled reports whether a root-owned
// <app support>/disable-automation-auth file holds the expected contents.
func automationAuthDisabled() bool {
	home, err := os.UserHomeDir()
	if err != nil {
		return false
	}
	path := filepath.Join(appSupportDir(home), "disable-automation-auth")
	fi, err := os.Lstat(path)
	if err != nil {
		return false
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok || st.Uid != 0 {
		return false
	}
	expected := hex.EncodeToString([]byte(path)) + " " + automationAuthMagic
	if fi.Size() != int64(len(expected)) {
		return false
	}
	data, err := os.ReadFile(path)
	return err == nil && string(data) == expected
}
