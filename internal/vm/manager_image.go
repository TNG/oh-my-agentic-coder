package vm

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// imagesDir holds image tarballs between the host-side pull and docker load.
func (m *Manager) imagesDir() string {
	return filepath.Join(m.Layout.Root, "images")
}

// ImageAdd pulls ref on the host (the VM may be offline) and loads it into
// the VM's Docker. The tarball is deleted afterwards.
func (m *Manager) ImageAdd(ctx context.Context, ref string) error {
	norm, err := ValidateImageRef(ref)
	if err != nil {
		return fmt.Errorf("omac vm: %w", err)
	}
	st, err := m.runningState(ctx)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(m.imagesDir(), 0o700); err != nil {
		return fmt.Errorf("omac vm: create %s: %w", m.imagesDir(), err)
	}
	tar := filepath.Join(m.imagesDir(), sanitizeFileName(norm)+".tar")
	defer os.Remove(tar)
	fmt.Fprintf(m.Out, "omac vm: pulling %s\n", norm)
	if err := m.Deps.Pull(ctx, norm, tar); err != nil {
		return fmt.Errorf("omac vm: %w", err)
	}
	f, err := os.Open(tar)
	if err != nil {
		return fmt.Errorf("omac vm: %w", err)
	}
	defer f.Close()
	if err := m.ssh(ctx, m.agentTarget(st), f, m.Out, dockerCmd("load")); err != nil {
		return fmt.Errorf("omac vm: docker load %s: %w", norm, err)
	}
	st.Images = addSorted(st.Images, norm)
	return SaveState(m.Layout.StatePath, st)
}

// ImageList returns the images recorded in state plus, while the VM runs,
// those its Docker reports, all fully qualified, sorted and deduplicated.
func (m *Manager) ImageList(ctx context.Context) ([]string, error) {
	inst, err := m.instance(ctx)
	if err != nil {
		return nil, err
	}
	if inst == nil {
		return nil, ErrNotCreated
	}
	st, err := m.loadState()
	if err != nil {
		return nil, err
	}
	images := slices.Clone(st.Images)
	if inst.Status == StatusRunning {
		var out bytes.Buffer
		if err := m.ssh(ctx, m.agentTarget(st), nil, &out, dockerCmd("image ls --format '{{.Repository}}:{{.Tag}}'")); err != nil {
			return nil, fmt.Errorf("omac vm: docker image ls: %w", err)
		}
		sc := bufio.NewScanner(&out)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if line == "" || strings.Contains(line, "<none>") {
				continue
			}
			if norm, err := ValidateImageRef(line); err == nil {
				images = append(images, norm)
			}
		}
	}
	slices.Sort(images)
	return slices.Compact(images), nil
}

// ImageRemove deletes ref from the VM's Docker and from state.
func (m *Manager) ImageRemove(ctx context.Context, ref string) error {
	norm, err := ValidateImageRef(ref)
	if err != nil {
		return fmt.Errorf("omac vm: %w", err)
	}
	st, err := m.runningState(ctx)
	if err != nil {
		return err
	}
	if err := m.ssh(ctx, m.agentTarget(st), nil, nil, dockerCmd("image rm "+ShellQuote(norm))); err != nil {
		return fmt.Errorf("omac vm: docker image rm %s: %w", norm, err)
	}
	st.Images = slices.DeleteFunc(st.Images, func(s string) bool { return s == norm })
	return SaveState(m.Layout.StatePath, st)
}

func addSorted(list []string, s string) []string {
	list = append(list, s)
	slices.Sort(list)
	return slices.Compact(list)
}

// sanitizeFileName maps an image ref to a file name: characters outside
// [A-Za-z0-9._-] become "_".
func sanitizeFileName(ref string) string {
	return strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '.' || r == '_' || r == '-' {
			return r
		}
		return '_'
	}, ref)
}
