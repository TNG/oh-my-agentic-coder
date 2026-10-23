package vm

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
)

// StatusReport is what `omac vm status` shows.
type StatusReport struct {
	Created, Running    bool
	Mode                string
	SSHPort, DockerPort int
	Resources           Resources
	Images, Worktrees   []string
	JDKs, Packages      []string
	Sessions            []int
	Docker              *DockerVersion // nil when the probe failed or the VM is stopped
}

// Status reports the VM without changing it. A failed Docker probe only
// leaves Docker nil. The copied worktrees are read from the guest here and
// not in report, so the status that an idempotent Up prints needs no SSH.
func (m *Manager) Status(ctx context.Context) (StatusReport, error) {
	inst, err := m.instance(ctx)
	if err != nil {
		return StatusReport{}, err
	}
	st, err := LoadState(m.Layout.StatePath)
	if err != nil {
		return StatusReport{}, err
	}
	r, err := m.report(ctx, inst, st)
	if err == nil && r.Running && st != nil {
		r.Worktrees = m.guestWorktrees(ctx, st)
	}
	return r, err
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
	if st != nil {
		r.Mode, r.SSHPort, r.DockerPort = st.Mode, st.SSHPort, st.DockerPort
		r.Images, r.JDKs, r.Packages = st.Images, st.JDKs, st.Packages
	}
	if r.Sessions, err = AttachedSessions(m.Layout, m.Deps.Alive); err != nil {
		return StatusReport{}, err
	}
	if r.Running && st != nil {
		if v, err := m.Deps.Probe(ctx, st.DockerPort); err == nil {
			r.Docker = &v
		}
	}
	return r, nil
}

// guestWorktrees reads the registry that Exec keeps in the guest and returns
// "<host path> (<slug>)" entries. Status is informational, so an SSH failure
// only yields an empty list.
func (m *Manager) guestWorktrees(ctx context.Context, st *State) []string {
	var out bytes.Buffer
	if err := m.ssh(ctx, m.agentTarget(st), nil, &out, listWorktreesCmd); err != nil {
		return nil
	}
	return parseWorktreeRegistry(out.String())
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
		docker = fmt.Sprintf("Docker %s, API %s", s.Docker.Version, s.Docker.APIVersion)
	}
	fmt.Fprintf(w, "docker: tcp://127.0.0.1:%d (%s)\n", s.DockerPort, docker)
	fmt.Fprintf(w, "resources: %d cpus, %s memory, %s disk\n", s.Resources.CPUs, s.Resources.Memory, s.Resources.Disk)
	writeList(w, "packages", s.Packages)
	writeList(w, "jdks", s.JDKs)
	writeList(w, "worktrees", s.Worktrees)
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
		fmt.Fprintf(w, "  - %s\n", it)
	}
}
