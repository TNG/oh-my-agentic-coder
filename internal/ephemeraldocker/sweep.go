package ephemeraldocker

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// LimaCtl runs one limactl invocation with LIMA_HOME set to limaHome.
// Each session owns a private LIMA_HOME, so every limactl call must name
// the unit it addresses; the process env alone is not enough. Tests
// inject a fake; production uses DefaultLimaCtl.
type LimaCtl func(limaHome string, args ...string) error

// Reaper kills stray QEMU processes of one VM unit after limactl could
// not remove the instance. Matched on the instance name embedded in the
// process argv (lima puts the instance name in the disk/data paths);
// unrelated processes never match. Tests inject a fake.
type Reaper func(vmName string) error

// SweepResult reports what one sweep pass found.
type SweepResult struct {
	// Reaped lists the VM names of removed orphan units.
	Reaped []string
	// Active lists the session ids of locked (live) units, untouched.
	Active []string
}

// Sweep reaps orphaned ephemeral-docker units under root (the feature's
// directory in the cache scope). A unit is orphaned when its liveness
// flock is free — a crashed parent drops the lock implicitly, which is
// what makes reaping safe. Active units (lock held) are never touched,
// and neither are foreign entries (the shared image, non-unit names):
// the sweep only manages what it can positively identify.
//
// A failed limactl delete does not keep the unit dir: the QEMU reaper
// gets a chance and the error carries the VM name so the user can clean
// up manually. Per-unit errors are collected; one broken unit never
// blocks the others.
func Sweep(root string, run LimaCtl, reap Reaper) (SweepResult, []error) {
	var (
		res  SweepResult
		errs []error
	)
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return res, nil
		}
		return res, []error{fmt.Errorf("sweep: read %s: %w", root, err)}
	}
	for _, entry := range entries {
		if !entry.IsDir() || !isSessionID(entry.Name()) {
			continue // foreign entries are never touched
		}
		sess := entry.Name()
		unit := filepath.Join(root, sess)
		acquired, cleanup := tryLock(filepath.Join(unit, lockName))
		if !acquired {
			res.Active = append(res.Active, sess)
			continue
		}
		vmName, garbageErr := reapUnit(sess, unit, run, reap, &errs)
		cleanup()
		if garbageErr != nil {
			errs = append(errs, garbageErr)
			continue
		}
		if vmName != "" {
			res.Reaped = append(res.Reaped, vmName)
		}
	}
	errs = append(errs, sweepDanglingAliases()...)
	return res, errs
}

// reapUnit removes one orphaned unit: VM first, then alias, then dir. It
// returns the unit's VM name ("" for unmarked garbage) and an error when
// the unit dir could not be removed.
func reapUnit(sess, unit string, run LimaCtl, reap Reaper, errs *[]error) (vmName string, dirErr error) {
	var marker SessionMarker
	vmName = ""
	if raw, err := os.ReadFile(filepath.Join(unit, markerName)); err == nil {
		if json.Unmarshal(raw, &marker) == nil {
			vmName = marker.VMName
		}
		// An unreadable/corrupt marker means the unit crashed before
		// finishing layout: no VM was ever started, there is nothing to
		// delete, the dir is garbage.
	}

	if vmName != "" {
		if err := run(marker.Symlink, "delete", "-f", vmName); err != nil {
			*errs = append(*errs, fmt.Errorf("sweep: unit %s: limactl delete %s failed: %w", sess, vmName, err))
			if rerr := reap(vmName); rerr != nil {
				*errs = append(*errs, fmt.Errorf("sweep: unit %s: QEMU reap of %s failed: %w", sess, vmName, rerr))
			}
		}
	}

	// Remove the alias by its derived name, not by the marker field: a
	// corrupted marker must not make the sweep remove arbitrary paths.
	if err := os.Remove(filepath.Join(symlinkDir, symlinkPrefix+sess)); err != nil && !os.IsNotExist(err) {
		return "", fmt.Errorf("sweep: unit %s: remove alias: %w", sess, err)
	}
	if err := os.RemoveAll(unit); err != nil {
		return "", fmt.Errorf("sweep: unit %s: remove dir: %w", sess, err)
	}
	return vmName, nil
}

// sweepDanglingAliases removes /tmp LIMA_HOME aliases whose target is
// gone (a teardown that died between dir removal and alias removal).
// Aliases with a live target belong to active units and survive.
func sweepDanglingAliases() []error {
	var errs []error
	entries, err := os.ReadDir(symlinkDir)
	if err != nil {
		return nil
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.Type()&os.ModeSymlink == 0 || !strings.HasPrefix(name, symlinkPrefix) {
			continue
		}
		if !isSessionID(strings.TrimPrefix(name, symlinkPrefix)) {
			continue
		}
		target, err := os.Readlink(filepath.Join(symlinkDir, name))
		if err != nil {
			continue
		}
		if _, err := os.Stat(target); os.IsNotExist(err) {
			if rmErr := os.Remove(filepath.Join(symlinkDir, name)); rmErr != nil && !os.IsNotExist(rmErr) {
				errs = append(errs, fmt.Errorf("sweep: remove dangling alias %s: %w", name, rmErr))
			}
		}
	}
	return errs
}

// isSessionID reports whether name has the shape of a session id this
// feature generates (12 lower-case hex chars).
func isSessionID(name string) bool {
	if len(name) != 12 {
		return false
	}
	for _, c := range name {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}
