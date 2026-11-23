package mcppolicy

import (
	"strings"
	"testing"
)

func TestSummarizeArgs(t *testing.T) {
	cases := []struct {
		name, args, want string
		max              int
	}{
		{"empty", ``, "", 100},
		{"null", `null`, "", 100},
		{"zero max", `{"a":1}`, "", 0},
		{"simple sorted", `{"b":true,"a":"x","c":null,"d":1.5}`, `a="x", b=true, c=null, d=1.5`, 200},
		{"nested reduced to size", `{"o":{"token":"leak"},"l":[1,2,3]}`, `l=[3 items], o={1 keys}`, 200},
		{"credential keys masked", `{"API_KEY":"k","password":"p","Authorization":"Bearer x","my_secret":1,"accessToken":"t","bearer":"b","credentials":{}}`,
			`API_KEY=***, Authorization=***, accessToken=***, bearer=***, credentials=***, my_secret=***, password=***`, 300},
		{"long string cut", `{"a":"` + strings.Repeat("x", 100) + `"}`, `a="` + strings.Repeat("x", 40) + `..."`, 200},
		{"non ascii encoded", `{"n":"café"}`, `n="caf%C3%A9"`, 200},
		{"control and percent encoded", `{"n":"a\nb%"}`, `n="a%0Ab%25"`, 200},
		{"key encoded", "{\"kéy2\":1}", `k%C3%A9y2=1`, 200},
		{"not an object", `[1]`, `(arguments are not an object)`, 200},
		{"invalid", `{`, `(arguments are not an object)`, 200},
	}
	for _, tc := range cases {
		got := SummarizeArgs([]byte(tc.args), tc.max)
		if got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestSummarizeArgsCap(t *testing.T) {
	args := `{"a":"` + strings.Repeat("é", 30) + `","b":"` + strings.Repeat("y", 30) + `"}`
	for max := 1; max < 120; max++ {
		got := SummarizeArgs([]byte(args), max)
		if len(got) > max {
			t.Fatalf("max %d: len %d: %q", max, len(got), got)
		}
		for i := 0; i < len(got); i++ {
			if got[i] < 0x20 || got[i] > 0x7e {
				t.Fatalf("max %d: non printable byte in %q", max, got)
			}
			if got[i] == '%' && (i+2 >= len(got) || !isHex(got[i+1]) || !isHex(got[i+2])) {
				t.Fatalf("max %d: cut inside percent escape: %q", max, got)
			}
		}
	}
	if got := SummarizeArgs([]byte(args), 1024); strings.HasSuffix(got, "...") && len(got) < 1024 && len(got) > 90 {
		t.Errorf("unexpected cut: %q", got)
	}
}

func TestSummarizeArgsMasksValueNotKeyName(t *testing.T) {
	got := SummarizeArgs([]byte(`{"token":"supersecretvalue"}`), 100)
	if strings.Contains(got, "supersecretvalue") {
		t.Errorf("value leaked: %s", got)
	}
}

func isHex(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'A' && c <= 'F'
}
