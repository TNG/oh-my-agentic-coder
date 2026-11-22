package config

import (
	"strings"
	"testing"
)

func TestRequireSecret_Parsed(t *testing.T) {
	m, err := loadApprovalsMeta(t, "  require_secret: true\n")
	if err != nil {
		t.Fatal(err)
	}
	if !m.Sidecar.RequireSecret {
		t.Error("RequireSecret = false, want true")
	}
	m, err = loadApprovalsMeta(t, "")
	if err != nil {
		t.Fatal(err)
	}
	if m.Sidecar.RequireSecret {
		t.Error("RequireSecret defaults to true, want false")
	}
}

func TestRequireSecret_NearMissIsRefused(t *testing.T) {
	for _, k := range []string{"require_secrets", "requiresecret", "require-secret", "Require_Secret_"} {
		_, err := loadApprovalsMeta(t, "  "+k+": true\n")
		if err == nil || !strings.Contains(err.Error(), "did you mean require_secret") {
			t.Errorf("%s: err = %v", k, err)
		}
	}
	if _, err := loadApprovalsMeta(t, "  x-require_secret: &a true\n"); err != nil {
		t.Errorf("extension key refused: %v", err)
	}
}

func TestRequireSecret_ReservedNameRefused(t *testing.T) {
	for name, block := range map[string]string{
		"secrets":         "  secrets:\n    - name: OMAC_SIDECAR_SECRET\n",
		"config":          "  config:\n    - name: OMAC_SIDECAR_SECRET\n      type: string\n",
		"env_passthrough": "  env_passthrough: [OMAC_SIDECAR_SECRET]\n",
	} {
		_, err := loadApprovalsMeta(t, block)
		if err == nil || !strings.Contains(err.Error(), "reserved") {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}
