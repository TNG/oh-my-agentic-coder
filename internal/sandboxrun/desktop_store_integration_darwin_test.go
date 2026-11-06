//go:build darwin

package sandboxrun

import "testing"

func TestIntegrationDesktopStoresSeatbelt(t *testing.T) {
	for _, present := range []bool{false, true} {
		t.Run(map[bool]string{false: "absent", true: "present"}[present], func(t *testing.T) {
			fx := newDesktopStoreFixture(t, present)
			g := fx.grants(t, true) // Broad writable home must not defeat protection.
			argv, err := BuildChildArgv(g, []string{"/bin/sh", "-c", fx.script()})
			if err != nil {
				t.Fatal(err)
			}
			fx.check(t, runDesktopStoreProbe(t, argv))
		})
	}
}
