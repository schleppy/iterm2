package iterm2

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func fakeAppleScript(t *testing.T, running string, cookieOut string, cookieErr error) *[]string {
	t.Helper()
	var scripts []string
	orig := runAppleScript
	t.Cleanup(func() { runAppleScript = orig })
	runAppleScript = func(_ context.Context, script string) (string, error) {
		scripts = append(scripts, script)
		if strings.Contains(script, "is running") {
			return running, nil
		}
		return cookieOut, cookieErr
	}
	return &scripts
}

func TestAppleScriptAuth(t *testing.T) {
	t.Setenv("IT2_APP_PATH", "")
	scripts := fakeAppleScript(t, "yes", "c00kie k3y", nil)
	creds, err := AppleScriptAuth(ctxT(t), `my "app"`)
	if err != nil {
		t.Fatal(err)
	}
	if creds != (Credentials{Cookie: "c00kie", Key: "k3y"}) {
		t.Fatalf("creds = %+v", creds)
	}
	if len(*scripts) != 2 {
		t.Fatalf("ran %d scripts", len(*scripts))
	}
	want := `tell application "iTerm2" to request cookie and key for app named "my \"app\""`
	if (*scripts)[1] != want {
		t.Fatalf("script = %s\nwant     %s", (*scripts)[1], want)
	}
}

func TestAppleScriptAuthDoesNotLaunchITerm2(t *testing.T) {
	scripts := fakeAppleScript(t, "no", "", nil)
	if _, err := AppleScriptAuth(ctxT(t), "x"); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("err = %v", err)
	}
	if len(*scripts) != 1 {
		t.Fatal("requested a cookie from an iTerm2 that is not running")
	}
}

func TestAppleScriptAuthErrors(t *testing.T) {
	denied := errors.New("not authorized to send Apple events")
	fakeAppleScript(t, "yes", "", denied)
	if _, err := AppleScriptAuth(ctxT(t), "x"); !errors.Is(err, denied) {
		t.Fatalf("err = %v", err)
	}
	fakeAppleScript(t, "yes", "", nil)
	if _, err := AppleScriptAuth(ctxT(t), "x"); err == nil {
		t.Fatal("empty response accepted")
	}
}

func TestAppleScriptAuthAppPath(t *testing.T) {
	t.Setenv("IT2_APP_PATH", `/Apps/iTerm "Beta".app`)
	scripts := fakeAppleScript(t, "yes", "c k", nil)
	if _, err := AppleScriptAuth(ctxT(t), "x"); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix((*scripts)[0], `if application "/Apps/iTerm \"Beta\".app" is running`) {
		t.Fatalf("script = %s", (*scripts)[0])
	}
}
