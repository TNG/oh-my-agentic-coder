//go:build linux

package sandboxrun

import (
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestIntegrationPlaywrightSmoke runs a real `npx playwright test` suite
// inside `omac sandbox run`, with Landlock open_port for the local
// webServer. This is the end-to-end counterpart to
// TestIntegrationBrowserHeadlessLocalPage (raw Chromium): here the
// Playwright runner starts both the Node server and Chromium as children
// of the sandboxed process tree.
//
// Hermetic enough for local/CI without LLM: the page is served from
// loopback inside the sandbox; Chromium is taken from the host Playwright
// cache (executablePath) so no browser download is required. `npm install`
// needs registry access once — if it fails, the test skips.
//
// Skips (not skipOrFailCI) when node/npm/chromium/bwrap are missing.
func TestIntegrationPlaywrightSmoke(t *testing.T) {
	requireBwrap(t)
	if !LandlockNetSupported() {
		t.Skipf("Landlock ABI %d < 4", LandlockABI())
	}
	for _, bin := range []string{"node", "npm", "npx"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not installed", bin)
		}
	}
	chrome := findChromiumForTest()
	if chrome == "" {
		t.Skip("chromium not installed (PATH or ~/.cache/ms-playwright)")
	}

	port := freeTCPPort(t)
	wd := t.TempDir()
	copyPlaywrightFixture(t, wd)
	writePlaywrightConfig(t, wd, port, chrome)

	npm := exec.Command("npm", "install", "--no-fund", "--no-audit")
	npm.Dir = wd
	npm.Env = os.Environ()
	if out, err := npm.CombinedOutput(); err != nil {
		t.Skipf("npm install failed (registry/network required once): %v\n%s", err, out)
	}

	omac := filepath.Join(t.TempDir(), "omac")
	build := exec.Command("go", "build", "-o", omac, "github.com/TNG/oh-my-agentic-coder/cmd/omac")
	build.Env = os.Environ()
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build omac: %v\n%s", err, out)
	}

	// open_port is the grant under test: without it the in-sandbox webServer
	// cannot bind/connect on `port`. Browser binaries are granted read-only.
	chromeDir := filepath.Dir(chrome)
	// The Playwright cache root is only meaningful when chrome actually lives
	// under it (…/ms-playwright/chromium-N/chrome-linux/chrome). A distro
	// package resolves to something like /opt/google/chrome/chrome, where
	// walking up two levels would grant an unrelated parent such as /opt;
	// executablePath in the generated config makes the cache root optional.
	msPlaywright := ""
	if home, err := os.UserHomeDir(); err == nil {
		cache := filepath.Join(home, ".cache", "ms-playwright")
		if strings.HasPrefix(chrome, cache+string(filepath.Separator)) {
			msPlaywright = cache
		}
	}

	args := []string{"sandbox", "run",
		"--profile", "default",
		"--open-port", strconv.Itoa(port),
		"--read", chromeDir,
	}
	if msPlaywright != "" {
		args = append(args, "--read", msPlaywright)
	}
	args = append(args,
		"--allow-env", "PLAYWRIGHT_*",
		"--allow-env", "PORT",
		"--",
		"npx", "playwright", "test", "--reporter=line",
	)
	cmd := exec.Command(omac, args...)
	cmd.Dir = wd
	cmd.Env = append(os.Environ(), "PORT="+strconv.Itoa(port))
	if msPlaywright != "" {
		cmd.Env = append(cmd.Env, "PLAYWRIGHT_BROWSERS_PATH="+msPlaywright)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("playwright inside omac sandbox failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "passed") {
		t.Fatalf("playwright output missing pass marker:\n%s", out)
	}
}

func freeTCPPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	return port
}

func copyPlaywrightFixture(t *testing.T, dst string) {
	t.Helper()
	src := filepath.Join("testdata", "playwright-smoke")
	entries, err := os.ReadDir(src)
	if err != nil {
		t.Fatalf("read fixture %s: %v", src, err)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if name == "README.md" {
			continue
		}
		in, err := os.Open(filepath.Join(src, name))
		if err != nil {
			t.Fatal(err)
		}
		outPath := filepath.Join(dst, name)
		out, err := os.Create(outPath)
		if err != nil {
			in.Close()
			t.Fatal(err)
		}
		_, err = io.Copy(out, in)
		in.Close()
		cerr := out.Close()
		if err != nil {
			t.Fatal(err)
		}
		if cerr != nil {
			t.Fatal(cerr)
		}
	}
}

func writePlaywrightConfig(t *testing.T, wd string, port int, chrome string) {
	t.Helper()
	// Escape backslashes for Windows paths; on Linux this is a no-op for
	// typical paths, but keeps the generated TS valid if chrome has quotes.
	chromeEsc := strings.ReplaceAll(chrome, `\`, `\\`)
	chromeEsc = strings.ReplaceAll(chromeEsc, `'`, `\'`)
	cfg := fmt.Sprintf(`import { defineConfig } from '@playwright/test';

export default defineConfig({
  testDir: '.',
  reporter: 'line',
  webServer: {
    command: 'node server.js',
    url: 'http://127.0.0.1:%d/',
    reuseExistingServer: false,
    env: { PORT: '%d' },
  },
  use: {
    headless: true,
    launchOptions: { executablePath: '%s' },
  },
});
`, port, port, chromeEsc)
	if err := os.WriteFile(filepath.Join(wd, "playwright.config.ts"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
}
