package vm

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite golden")

var goldenInputs = LimaInputs{
	Resources: Resources{CPUs: 8, Memory: "16GiB", Disk: "60GiB"},
	SSHPort:   42022, AgentUser: "alice",
	AdminPubKey: "ssh-ed25519 AAAAadmin omac-vm-admin",
	AgentPubKey: "ssh-ed25519 AAAAagent omac-vm-agent",
	Packages:    []string{"jq", "unzip"},
	Ruleset:     Ruleset(),
}

func TestRenderLimaConfigGolden(t *testing.T) {
	got, err := RenderLimaConfig(goldenInputs)
	if err != nil {
		t.Fatal(err)
	}
	if *update {
		if err := os.WriteFile("testdata/lima.golden", []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile("testdata/lima.golden")
	if err != nil {
		t.Fatal(err)
	}
	// A Windows checkout (the WSL2 CI job) turns the golden's LF into CRLF.
	if got != strings.ReplaceAll(string(want), "\r\n", "\n") {
		t.Errorf("rendered config differs from testdata/lima.golden:\n%s", diffLines(string(want), got))
	}
}

// diffLines shows want and got side by side from the first differing line
// on, for up to 20 lines.
func diffLines(want, got string) string {
	w, g := strings.Split(want, "\n"), strings.Split(got, "\n")
	first := 0
	for first < len(w) && first < len(g) && w[first] == g[first] {
		first++
	}
	var b strings.Builder
	for i := first; i < first+20 && (i < len(w) || i < len(g)); i++ {
		wl, gl := "<missing>", "<missing>"
		if i < len(w) {
			wl = w[i]
		}
		if i < len(g) {
			gl = g[i]
		}
		mark := " "
		if wl != gl {
			mark = "!"
		}
		fmt.Fprintf(&b, "%s %4d want: %s\n%s %4d got:  %s\n", mark, i+1, wl, mark, i+1, gl)
	}
	return b.String()
}

func renderGolden(t *testing.T) string {
	t.Helper()
	got, err := RenderLimaConfig(goldenInputs)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestRenderLimaConfigStructure(t *testing.T) {
	got := renderGolden(t)
	for _, want := range []string{
		"base:\n  - template:_images/ubuntu-24.04",
		"mounts: []",
		"containerd: {system: false, user: false}",
		"user:\n  name: alice",
		"ssh:\n  localPort: 42022",
		"cpus: 8",
		"memory: 16GiB",
		"disk: 60GiB",
		// Lima uses the first matching rule. After the published range, the
		// ignore rules switch off Lima's default forwarding of every other
		// guest listener to the host's loopback.
		"portForwards:\n" +
			"  - guestIP: 127.0.0.1\n    guestPortRange: [30000, 30099]\n    hostIP: 127.0.0.1\n    hostPortRange: [30000, 30099]\n    proto: tcp\n" +
			"  - guestIP: 127.0.0.1\n    guestPortRange: [1, 65535]\n    proto: any\n    ignore: true\n" +
			"  - guestIP: 0.0.0.0\n    guestPortRange: [1, 65535]\n    proto: any\n    ignore: true\n" +
			"provision:\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("config lacks %q", want)
		}
	}
	// Docker is reached only over ssh: no TCP listener and no forward for it.
	for _, bad := range []string{"location: ~", "writable: true", "2375", "guestPort:", "override.conf", "tcp://"} {
		if strings.Contains(got, bad) {
			t.Errorf("config must not contain %q", bad)
		}
	}
}

// provisionBlocks splits the rendered config into the boot, system and user
// provisioning scripts.
func provisionBlocks(t *testing.T, cfg string) (boot, system, user string) {
	t.Helper()
	b := strings.Index(cfg, "mode: boot")
	s := strings.Index(cfg, "mode: system")
	u := strings.Index(cfg, "mode: user")
	if b < 0 || s < 0 || u < 0 || !(b < s && s < u) {
		t.Fatalf("provision order wrong: boot=%d system=%d user=%d", b, s, u)
	}
	return cfg[b:s], cfg[s:u], cfg[u:]
}

func TestRenderLimaConfigProvisionOrder(t *testing.T) {
	boot, system, user := provisionBlocks(t, renderGolden(t))
	checks := []struct {
		name, block string
		wants       []string
	}{
		{"boot", boot, []string{
			"ln -sf /run/systemd/resolve/resolv.conf /etc/resolv.conf",
			"nft -f /etc/omac-vmguard.nft",
			"/etc/omac-vm/mode",
			"/var/lib/omac-vm/provisioned",
			// The script spells paths literally; these keep the Go
			// constants in sync with it.
			"nft -f " + RulesetFile,
			"cat > " + RulesetFile,
			"[ -f " + ModeFile + " ]",
			"[ -f " + ProvisionedMarker + " ]",
			"nft list table inet " + GuardTable,
		}},
		{"system", system, []string{
			"download.docker.com",
			"packages.adoptium.net",
			"docker-ce docker-ce-cli containerd.io docker-ce-rootless-extras",
			"90-cloud-init-users",
			"127.0.1.1",
			"/root/.ssh/authorized_keys",
			"ssh-ed25519 AAAAadmin",
			"loginctl enable-linger alice",
			"jq unzip",
			// A boot stopped during apt leaves dpkg half-configured.
			"dpkg --configure -a || true\n      apt-get update\n",
			"> " + PackagesFile,
			"> " + ProvisionedMarker,
		}},
		{"user", user, []string{
			"dockerd-rootless-setuptool.sh install",
			"systemctl --user enable --now docker",
			"ssh-ed25519 AAAAagent",
			"testcontainers.properties",
		}},
	}
	for _, c := range checks {
		for _, want := range c.wants {
			if !strings.Contains(c.block, want) {
				t.Errorf("%s block lacks %q", c.name, want)
			}
		}
	}
	if strings.Contains(boot, "AAAAadmin") || strings.Contains(boot, "AAAAagent") {
		t.Error("boot block must not carry pub keys")
	}
}

// Lima runs the user script on every boot. A failed Docker start must not
// make later boots skip Docker: only the setuptool install is guarded, and
// the Docker start runs every time, after it.
func TestUserScriptRecoversAcrossBoots(t *testing.T) {
	_, _, user := provisionBlocks(t, renderGolden(t))
	steps := []string{
		"systemctl --user start dbus || true\n",
		"[ -f ~/.config/systemd/user/docker.service ] || dockerd-rootless-setuptool.sh install\n",
		"systemctl --user enable --now docker\n",
	}
	pos := 0
	for _, s := range steps {
		i := strings.Index(user[pos:], "      "+s)
		if i < 0 {
			t.Fatalf("user block lacks %q after offset %d:\n%s", s, pos, user)
		}
		pos += i + len(s)
	}
}

// The cloud-init drop-in must be written on every boot, before the early
// exit for provisioned VMs, or a VM provisioned without it never gets it.
func TestSystemScriptKeepsSSHHostKeys(t *testing.T) {
	system := renderedScripts(t, renderGolden(t))["system"]
	dropIn := strings.Index(system, "printf 'ssh_deletekeys: false\\n' > /etc/cloud/cloud.cfg.d/99-omac-keep-hostkeys.cfg\n")
	exit := strings.Index(system, "[ -f "+ProvisionedMarker+" ] && exit 0")
	if dropIn < 0 || exit < 0 || dropIn > exit {
		t.Fatalf("system script must write the ssh_deletekeys drop-in before the provisioned exit (drop-in at %d, exit at %d):\n%s", dropIn, exit, system)
	}
}

// renderedScripts returns each provisioning script of cfg with its YAML
// indentation removed, keyed by its mode.
func renderedScripts(t *testing.T, cfg string) map[string]string {
	t.Helper()
	scripts := map[string]string{}
	lines := strings.Split(cfg, "\n")
	for i := 0; i < len(lines); i++ {
		mode, ok := strings.CutPrefix(lines[i], "  - mode: ")
		if !ok {
			continue
		}
		if i+1 >= len(lines) || lines[i+1] != "    script: |" {
			t.Fatalf("mode %s is not followed by a script block", mode)
		}
		var b strings.Builder
		for i += 2; i < len(lines) && (lines[i] == "" || strings.HasPrefix(lines[i], "      ")); i++ {
			b.WriteString(strings.TrimPrefix(lines[i], "      ") + "\n")
		}
		i--
		scripts[mode] = b.String()
	}
	if len(scripts) != 3 {
		t.Fatalf("found scripts for %d modes, want boot, system and user", len(scripts))
	}
	return scripts
}

// The golden file freezes the script text but never parses it. Lima runs
// the boot script with /bin/sh whatever its shebang says, so it must also
// pass a POSIX shell's syntax check.
func TestRenderedScriptsParse(t *testing.T) {
	scripts := renderedScripts(t, renderGolden(t))
	check := func(shell, mode string) {
		t.Helper()
		path, err := exec.LookPath(shell)
		if err != nil {
			t.Skipf("%s not found: %v", shell, err)
		}
		f := filepath.Join(t.TempDir(), mode+".sh")
		if err := os.WriteFile(f, []byte(scripts[mode]), 0o600); err != nil {
			t.Fatal(err)
		}
		if out, err := exec.Command(path, "-n", f).CombinedOutput(); err != nil {
			t.Errorf("%s -n on the %s script: %v\n%s", shell, mode, err, out)
		}
	}
	for _, mode := range []string{"boot", "system", "user"} {
		check("bash", mode)
	}
	check("sh", "boot")
}

func TestRenderLimaConfigEmbedsRuleset(t *testing.T) {
	boot, _, _ := provisionBlocks(t, renderGolden(t))
	for _, line := range strings.Split(Ruleset(), "\n") {
		if line == "" {
			continue
		}
		if !strings.Contains(boot, "\n      "+line+"\n") {
			t.Errorf("boot block lacks indented ruleset line %q", line)
		}
	}
}

func TestRenderLimaConfigValidation(t *testing.T) {
	cases := map[string]func(*LimaInputs){
		"ssh port zero":           func(in *LimaInputs) { in.SSHPort = 0 },
		"ssh port too large":      func(in *LimaInputs) { in.SSHPort = 65536 },
		"empty agent user":        func(in *LimaInputs) { in.AgentUser = "" },
		"agent user with shell":   func(in *LimaInputs) { in.AgentUser = "a;reboot" },
		"empty admin key":         func(in *LimaInputs) { in.AdminPubKey = "" },
		"empty agent key":         func(in *LimaInputs) { in.AgentPubKey = "" },
		"empty ruleset":           func(in *LimaInputs) { in.Ruleset = "" },
		"admin key with newline":  func(in *LimaInputs) { in.AdminPubKey = "ssh-ed25519 AAAA\nrm -rf /" },
		"agent key with newline":  func(in *LimaInputs) { in.AgentPubKey = "ssh-ed25519 AAAA\rx" },
		"admin key with quote":    func(in *LimaInputs) { in.AdminPubKey = "ssh-ed25519 AAAA'; reboot; '" },
		"agent key with quote":    func(in *LimaInputs) { in.AgentPubKey = "x'y" },
		"package with space":      func(in *LimaInputs) { in.Packages = []string{"jq unzip"} },
		"package with semicolon":  func(in *LimaInputs) { in.Packages = []string{"jq;reboot"} },
		"package with quote":      func(in *LimaInputs) { in.Packages = []string{"jq'"} },
		"package as option":       func(in *LimaInputs) { in.Packages = []string{"-oAPT::x=y"} },
		"empty package":           func(in *LimaInputs) { in.Packages = []string{""} },
		"ruleset ends heredoc":    func(in *LimaInputs) { in.Ruleset = "table x {}\nOMACVMGUARD\nreboot\n" },
		"cpus zero":               func(in *LimaInputs) { in.Resources.CPUs = 0 },
		"memory with yaml escape": func(in *LimaInputs) { in.Resources.Memory = "8GiB\nmounts: [{location: ~}]" },
		"empty disk":              func(in *LimaInputs) { in.Resources.Disk = "" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			in := goldenInputs
			in.Packages = append([]string(nil), goldenInputs.Packages...)
			mutate(&in)
			if _, err := RenderLimaConfig(in); err == nil {
				t.Fatal("want error, got nil")
			}
		})
	}
}

func TestRenderLimaConfigAcceptsNoPackages(t *testing.T) {
	in := goldenInputs
	in.Packages = nil
	got, err := RenderLimaConfig(in)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "printf '%s\\n' '' > /etc/omac-vm/packages") {
		t.Error("empty package list should write an empty packages file")
	}
}

func TestRenderLimaConfigDeterministic(t *testing.T) {
	first, second := renderGolden(t), renderGolden(t)
	if first != second {
		t.Fatal("two renders differ")
	}
}

func TestAgentUserName(t *testing.T) {
	for in, want := range map[string]string{
		"Alice":                 "alice",
		"sajjad.ahmad":          "sajjad-ahmad",
		"9x":                    "_9x",
		"":                      "omac",
		"root":                  "omac",
		"ROOT":                  "omac",
		"lima":                  "lima",
		".hidden":               "_-hidden",
		"under_score-dash":      "under_score-dash",
		strings.Repeat("a", 40): strings.Repeat("a", 32),
	} {
		if got := AgentUserName(in); got != want {
			t.Errorf("AgentUserName(%q) = %q, want %q", in, got, want)
		}
	}
}
