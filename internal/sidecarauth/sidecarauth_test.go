package sidecarauth

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestMint(t *testing.T) {
	a, err := Mint()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := Mint()
	if len(a) != 64 {
		t.Fatalf("len = %d, want 64", len(a))
	}
	if a == b {
		t.Fatal("two mints returned the same secret")
	}
}

func TestValid(t *testing.T) {
	tests := []struct {
		name   string
		values []string
		secret string
		want   bool
	}{
		{"match", []string{"s3"}, "s3", true},
		{"mismatch", []string{"other"}, "s3", false},
		{"missing", nil, "s3", false},
		{"empty secret and missing header", nil, "", false},
		{"empty secret and empty header", []string{""}, "", false},
		{"two values", []string{"s3", "s3"}, "s3", false},
		{"two values one wrong", []string{"x", "s3"}, "s3", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			for _, v := range tc.values {
				r.Header.Add(Header, v)
			}
			if got := Valid(r, tc.secret); got != tc.want {
				t.Fatalf("Valid = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestRequire(t *testing.T) {
	if _, err := Require("", http.NotFoundHandler()); err == nil {
		t.Fatal("empty secret must be an error")
	}
	h, err := Require("s3", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, value string
		set         bool
		want        int
	}{
		{"missing", "", false, http.StatusUnauthorized},
		{"wrong", "nope", true, http.StatusUnauthorized},
		{"right", "s3", true, http.StatusNoContent},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			if tc.set {
				r.Header.Set(Header, tc.value)
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != tc.want {
				t.Fatalf("status = %d, want %d", w.Code, tc.want)
			}
		})
	}
}

func TestStripInbound(t *testing.T) {
	h := http.Header{}
	h.Set(Header, "a")
	h.Set(FacadeTokenHeader, "b")
	h["x-omac-sidecar-secret"] = []string{"c"}
	h["X_Omac_Sidecar_Secret"] = []string{"d"}
	h["X_OMAC_FACADE_TOKEN"] = []string{"e"}
	h.Set("X-Keep", "k")
	StripInbound(h)
	if len(h) != 1 || h.Get("X-Keep") != "k" {
		t.Fatalf("headers after strip = %v", h)
	}
}

func TestReserved(t *testing.T) {
	for name, want := range map[string]bool{
		EnvVar:                 true,
		"omac_sidecar_secret":  true,
		"OMAC_SIDECAR_SECRET2": false,
		"API_TOKEN":            false,
		"":                     false,
	} {
		if got := Reserved(name); got != want {
			t.Errorf("Reserved(%q) = %v, want %v", name, got, want)
		}
	}
}
