package iterm2

import (
	"slices"
	"strconv"
	"testing"

	"google.golang.org/protobuf/proto"

	pb "github.com/schleppy/iterm2/internal/iterm2pb"
)

func color(r, g, b float64) string {
	return `{"Red Component":` + ftoa(r) + `,"Green Component":` + ftoa(g) + `,"Blue Component":` + ftoa(b) + `,"Color Space":"sRGB"}`
}

func ftoa(f float64) string {
	switch f {
	case 0:
		return "0"
	case 1:
		return "1"
	}
	return "0.5"
}

// paletteOnce runs ProfileColors against a server that answers with props and returns the
// keys it was asked for.
func paletteOnce(t *testing.T, props map[string]string) ([]string, *Palette) {
	t.Helper()
	ct, st := newPipe()
	c := startClient(ct, 0)
	defer c.Close()
	got := make(chan []string, 1)
	go func() {
		req := readRequest(t, st)
		got <- req.GetGetProfilePropertyRequest().GetKeys()
		resp := &pb.GetProfilePropertyResponse{Status: pb.GetProfilePropertyResponse_OK.Enum()}
		for k, v := range props {
			resp.Properties = append(resp.Properties, &pb.ProfileProperty{Key: proto.String(k), JsonValue: proto.String(v)})
		}
		writeMessage(t, st, &pb.ServerOriginatedMessage{Id: req.Id, Submessage: &pb.ServerOriginatedMessage_GetProfilePropertyResponse{GetProfilePropertyResponse: resp}})
	}()
	p, err := c.ProfileColors(ctxT(t), "S1")
	if err != nil {
		t.Fatal(err)
	}
	return <-got, p
}

func TestProfileColors(t *testing.T) {
	keys, p := paletteOnce(t, map[string]string{
		"Foreground Color": color(0, 0, 0),
		"Background Color": color(1, 1, 1),
		"Ansi 1 Color":     color(1, 0, 0),
		"Cursor Color":     "null", // unset keys come back as JSON null
	})
	for _, k := range []string{"Background Color", "Background Color (Dark)", "Background Color (Light)", "Ansi 15 Color (Dark)", "Use Separate Colors for Light and Dark Mode"} {
		if !slices.Contains(keys, k) {
			t.Errorf("not asked for %q", k)
		}
	}
	if p.Background != (RGB{255, 255, 255}) || p.Foreground != (RGB{}) || p.ANSI[1] != (RGB{255, 0, 0}) || !p.Has["background"] || p.Has["cursor"] || p.Has["ansi"] {
		t.Errorf("palette = %+v", p)
	}
	if p.Separate || p.Dark != nil || p.Light != nil {
		t.Errorf("no light/dark colors were set: %+v", p)
	}
}

func TestProfileColorsLightAndDark(t *testing.T) {
	props := map[string]string{
		"Use Separate Colors for Light and Dark Mode": "true",
		"Background Color":         color(1, 1, 1),
		"Background Color (Light)": color(1, 1, 1),
		"Background Color (Dark)":  color(0, 0, 0),
		"Foreground Color (Dark)":  color(1, 1, 1),
	}
	for i := range 16 {
		props["Ansi "+itoa(i)+" Color (Dark)"] = color(0.5, 0.5, 0.5)
	}
	_, p := paletteOnce(t, props)
	if !p.Separate || p.Dark == nil || p.Light == nil {
		t.Fatalf("palette = %+v", p)
	}
	if p.Background != (RGB{255, 255, 255}) || p.Dark.Background != (RGB{}) || p.Dark.Foreground != (RGB{255, 255, 255}) || !p.Dark.Has["ansi"] || p.Dark.ANSI[7] != (RGB{128, 128, 128}) {
		t.Errorf("dark = %+v", p.Dark)
	}
	if p.Light.Background != (RGB{255, 255, 255}) || p.Light.Has["foreground"] || p.Light.Dark != nil {
		t.Errorf("light = %+v", p.Light)
	}
}

func itoa(i int) string { return strconv.Itoa(i) }
