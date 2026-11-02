//go:build linux

package sandboxrun

import "testing"

func TestIntegrationDesktopStoresBwrapLandlock(t *testing.T) {
	requireWorkingBwrap(t)
	if !LandlockNetSupported() {
		t.Fatalf("requires Landlock ABI 4; enforcement coverage is pending (ABI %d)", LandlockABI())
	}
	omac := buildOmac(t)
	for _, present := range []bool{false, true} {
		t.Run(map[bool]string{false: "absent", true: "present"}[present], func(t *testing.T) {
			fx := newDesktopStoreFixture(t, present)
			g := fx.grants(t, false) // RO ancestors and a writable runtime sibling are supported.
			g.ReadPaths = append(g.ReadPaths, omac)
			stage2 := append([]string{omac, "sandbox", "stage2"}, Stage2Args(g)...)
			stage2 = append(stage2, "--", "/bin/sh", "-c", fx.script())
			argv, err := BuildBwrapArgv(g, stage2)
			if err != nil {
				t.Fatal(err)
			}
			fx.check(t, runDesktopStoreProbe(t, argv))
			// The same layout with an ancestor project/profile RW grant must
			// refuse before any process can mutate or move Desktop directories.
			if _, err := BuildBwrapArgv(fx.grants(t, true), stage2); err == nil {
				t.Fatal("unsafe writable ancestor launched")
			}
		})
	}
}
