package manifest

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestManifestEveryForbiddenRune(t *testing.T) {
	runes := []rune{'\u061c', '\u200b', '\u200c', '\u200d', '\u200e', '\u200f', '\u2028', '\u2029', '\u202a', '\u202b', '\u202c', '\u202d', '\u202e', '\u2060', '\u2066', '\u2067', '\u2068', '\u2069', '\ufeff'}
	for r := rune(0); r <= 0x9f; r++ {
		if r < 0x20 || r >= 0x7f {
			runes = append(runes, r)
		}
	}
	for _, r := range runes {
		t.Run(fmt.Sprintf("U+%04X", r), func(t *testing.T) {
			payload := "café 東京 عربي left" + string(r) + "right"
			raw, err := json.Marshal(activateResponse{Dir: "/work", Skills: []skill{
				{Name: payload, Scope: payload, State: "broken", Detail: payload},
				{Name: "ok", Scope: "workdir", State: "pending-credentials", Missing: []string{payload, "TOKEN"}},
			}})
			if err != nil {
				t.Fatal(err)
			}
			text := Render(string(raw), ".opencode/skills")
			want := "café 東京 عربي leftright"
			if r == '\n' || r == '\r' {
				want = "café 東京 عربي left right"
			}
			if strings.Contains(text, payload) || strings.Count(text, want) != 4 || !strings.Contains(text, "omac secrets set ok TOKEN") {
				t.Errorf("unsafe or missing field/hint: %q", text)
			}
		})
	}
}
