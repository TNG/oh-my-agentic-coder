package vm

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const guestListTimeout = 10 * time.Second

// StatusReport is what `omac vm status` shows.
type StatusReport struct {
	Created, Running  bool
	Mode              string
	SSHPort           int
	Resources         Resources
	Images, Worktrees []string
	JDKs, Packages    []string
	Sessions          []int
	Docker            *DockerVersion // nil when the probe failed or the VM is stopped
	// WorktreesNote replaces the worktree list when it was not read.
	WorktreesNote string
}

// Status reports the VM state without modifying the VM. A failed Docker probe
// only leaves Docker nil. An existing instance without state.json is an
// error: its mode and ports are unknown, so the report would be misleading.
// Copied worktrees are read here, not in report, so an idempotent Up needs only
// the Docker check over SSH.
func (m *Manager) Status(ctx context.Context) (StatusReport, error) {
	inst, err := m.instance(ctx)
	if err != nil {
		return StatusReport{}, err
	}
	st, err := LoadState(m.Layout.StatePath)
	if err != nil {
		return StatusReport{}, err
	}
	if st == nil && inst != nil {
		return StatusReport{}, fmt.Errorf("omac vm: %s is missing; run `omac vm reset`", m.Layout.StatePath)
	}
	r, err := m.report(ctx, inst, st)
	if err != nil || !r.Running {
		return r, err
	}
	if wts, ok := m.guestWorktrees(ctx, st); ok {
		r.Worktrees, r.WorktreesNote = wts, ""
	} else {
		r.WorktreesNote = "unknown (could not read the list from the VM)"
	}
	return r, nil
}

func (m *Manager) report(ctx context.Context, inst *Instance, st *State) (StatusReport, error) {
	cfg, err := LoadUserConfig(m.ConfigPath)
	if err != nil {
		return StatusReport{}, fmt.Errorf("omac vm: %w", err)
	}
	r := StatusReport{
		Created:   inst != nil,
		Running:   inst != nil && inst.Status == StatusRunning,
		Resources: ResolveResources(cfg, m.Deps.Host),
	}
	r.WorktreesNote = "not read (see `omac vm status`)"
	if !r.Running {
		r.WorktreesNote = "unknown (VM not running)"
	}
	if st != nil {
		r.Mode, r.SSHPort = st.Mode, st.SSHPort
		r.Images, r.JDKs, r.Packages = st.Images, st.JDKs, st.Packages
	}
	if r.Sessions, err = AttachedSessions(m.Layout, m.Deps.Alive); err != nil {
		return StatusReport{}, err
	}
	if r.Running && st != nil {
		if v, err := m.dockerVersion(ctx, st); err == nil {
			r.Docker = &v
		}
	}
	return r, nil
}

// guestWorktrees reads the registry that Exec keeps in the guest and returns
// "<host path> (<slug>)" entries. ok is false when SSH failed or timed out.
func (m *Manager) guestWorktrees(ctx context.Context, st *State) ([]string, bool) {
	// A wedged guest can accept the connection and never answer.
	ctx, cancel := context.WithTimeout(ctx, guestListTimeout)
	defer cancel()
	var out bytes.Buffer
	if err := m.ssh(ctx, m.agentTarget(st), nil, &out, listWorktreesCmd); err != nil {
		return nil, false
	}
	return parseWorktreeRegistry(out.String()), true
}

// parseWorktreeRegistry turns "<slug> <host path>" lines into
// "<host path> (<slug>)" entries, skipping malformed and repeated lines.
func parseWorktreeRegistry(registry string) []string {
	var entries []string
	for line := range strings.Lines(registry) {
		slug, path, ok := strings.Cut(strings.TrimRight(line, "\r\n"), " ")
		if !ok || slug == "" || path == "" {
			continue
		}
		if e := path + " (" + slug + ")"; !slices.Contains(entries, e) {
			entries = append(entries, e)
		}
	}
	return entries
}

// Write prints the report as "key: value" lines; lists as "  - item".
func (s StatusReport) Write(w io.Writer) {
	status := "not created"
	switch {
	case s.Running:
		status = "running"
	case s.Created:
		status = "stopped"
	}
	fmt.Fprintf(w, "status: %s\n", status)
	if !s.Created {
		return
	}
	fmt.Fprintf(w, "mode: %s\n", s.Mode)
	fmt.Fprintf(w, "ssh: 127.0.0.1:%d\n", s.SSHPort)
	docker := "not answering"
	if s.Docker != nil {
		docker = fmt.Sprintf("Docker %s, API %s", printable(s.Docker.Version), printable(s.Docker.APIVersion))
	}
	fmt.Fprintf(w, "docker: %s\n", docker)
	fmt.Fprintf(w, "resources: %d cpus, %s memory, %s disk\n", s.Resources.CPUs, s.Resources.Memory, s.Resources.Disk)
	writeList(w, "packages", s.Packages)
	writeList(w, "jdks", s.JDKs)
	if s.WorktreesNote != "" {
		fmt.Fprintf(w, "worktrees: %s\n", s.WorktreesNote)
	} else {
		writeList(w, "worktrees", s.Worktrees)
	}
	writeList(w, "images", s.Images)
	pids := make([]string, len(s.Sessions))
	for i, p := range s.Sessions {
		pids[i] = strconv.Itoa(p)
	}
	writeList(w, "sessions", pids)
}

func writeList(w io.Writer, key string, items []string) {
	if len(items) == 0 {
		fmt.Fprintf(w, "%s: none\n", key)
		return
	}
	fmt.Fprintf(w, "%s:\n", key)
	for _, it := range items {
		fmt.Fprintf(w, "  - %s\n", printable(it))
	}
}

// printable escapes non-printable characters, so that text from inside
// the VM cannot send terminal escape sequences or forge extra lines:
// "a\x1b[2J" → `a\x1b[2J`.
func printable(s string) string {
	if utf8.ValidString(s) && strings.IndexFunc(s, func(r rune) bool { return !unicode.IsPrint(r) }) < 0 {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == utf8.RuneError && size == 1:
			fmt.Fprintf(&b, `\x%02x`, s[i])
		case unicode.IsPrint(r):
			b.WriteRune(r)
		case r == '\n':
			b.WriteString(`\n`)
		case r < 0x100:
			fmt.Fprintf(&b, `\x%02x`, r)
		default:
			fmt.Fprintf(&b, `\u%04x`, r)
		}
		i += size
	}
	return b.String()
}
