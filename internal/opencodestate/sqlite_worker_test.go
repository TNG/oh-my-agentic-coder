package opencodestate

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Instrumentation belongs to this test binary only. Production dispatch never
// reads these fixture controls or selects a security mode from the environment.
func prepareSQLiteTestWorker(args []string) (bool, int) {
	if len(args) != 3 || args[0] != sqliteWorkerFlag {
		return false, 0
	}
	path := args[2]
	control, err := os.ReadFile(path + ".test-control")
	if err != nil {
		return false, 0
	}
	if os.WriteFile(path+".test-pid", []byte(strconv.Itoa(os.Getpid())), 0o600) != nil {
		return true, 1
	}
	mode := string(control)
	if strings.HasPrefix(mode, "replace:") {
		if prepareSQLiteReplacement(path, strings.TrimPrefix(mode, "replace:")) != nil {
			return true, 1
		}
		return false, 0
	}
	switch mode {
	case "env":
		data, _ := json.Marshal(os.Environ())
		if os.WriteFile(path+".test-env", data, 0o600) != nil {
			return true, 1
		}
		return false, 0
	case "deny-entropy":
		if prepareSQLiteReplacement(path, mode) != nil {
			return true, 1
		}
		return false, 0
	case "entropy-contract":
		return true, runSQLiteEntropyContract(path)
	case "hang":
		time.Sleep(10 * time.Second)
		return true, 1
	case "bad":
		_, _ = io.WriteString(os.Stdout, "nope")
	case "oversized":
		_, _ = io.WriteString(os.Stdout, sqliteProtocolMagic)
		var header [9]byte
		header[0] = 1
		binary.BigEndian.PutUint32(header[1:5], metadataMaxBytes+1)
		_, _ = os.Stdout.Write(header[:])
	case "truncated", "nonzero", "slow-end":
		_, _ = io.WriteString(os.Stdout, sqliteProtocolMagic)
		_ = writeSQLiteRow(os.Stdout, "good", "/must-not-grant")
		if mode == "truncated" {
			return true, 0
		}
		_, _ = os.Stdout.Write([]byte{0})
		if mode == "nonzero" {
			return true, 1
		}
	default:
		return true, 2
	}
	// Parent must kill and reap on either bad framing or its single deadline.
	time.Sleep(10 * time.Second)
	return true, 1
}

func TestSQLiteReaderRealBinary(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "omac")
	build := exec.Command("go", "build", "-o", binary, "./cmd/omac")
	build.Dir = "../.."
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build real omac: %v\n%s", err, output)
	}
	home := hardeningHome(t)
	writeOpenCodeDB(t, home, [][2]string{{"good", "/good"}})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, "--omac-internal-sqlite-reader-v1", "project", filepath.Join(home, ".local/share/opencode/opencode.db"))
	cmd.Env = []string{"TMPDIR=" + os.TempDir()}
	start := time.Now()
	output, err := cmd.Output()
	if err != nil {
		t.Fatalf("private real-binary dispatch: %v", err)
	}
	want := "OSR1\x01\x00\x00\x00\x04\x00\x00\x00\x05good/good\x00"
	if string(output) != want {
		t.Fatalf("private dispatch returned %q, want bounded SQLite row protocol", output)
	}
	t.Logf("real worker start/read/exit: %s", time.Since(start))
	for _, args := range [][]string{
		{sqliteWorkerFlag, "SELECT worktree FROM project", filepath.Join(home, ".local/share/opencode/opencode.db")},
		{sqliteWorkerFlag, "project", "relative.sqlite"},
		{sqliteWorkerFlag, "project", filepath.Join(home, ".local/share/opencode/opencode.db"), "extra"},
	} {
		invalidCtx, stop := context.WithTimeout(context.Background(), time.Second)
		invalid := exec.CommandContext(invalidCtx, binary, args...)
		invalid.Env = []string{"TMPDIR=" + os.TempDir()}
		output, err := invalid.Output()
		stop()
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 2 || len(output) != 0 {
			t.Fatalf("private dispatcher accepted arbitrary SQL/invalid arguments: %v", err)
		}
	}
}

func TestSQLiteProtocolBounds(t *testing.T) {
	var valid bytes.Buffer
	valid.WriteString("OSR1")
	if err := writeSQLiteRow(&valid, "good", "/good"); err != nil {
		t.Fatal(err)
	}
	valid.WriteByte(0)
	oversized := append([]byte("OSR1\x01"), make([]byte, 8)...)
	binary.BigEndian.PutUint32(oversized[5:9], 1048577)
	tooMany := []byte("OSR1")
	for i := 0; i < 10001; i++ {
		tooMany = append(tooMany, make([]byte, 9)...)
		tooMany[len(tooMany)-9] = 1
	}
	tooMany = append(tooMany, 0)
	for _, test := range []struct {
		name string
		data []byte
		ok   bool
	}{
		{"valid", valid.Bytes(), true},
		{"bad magic", []byte("nope\x00"), false},
		{"truncated", valid.Bytes()[:len(valid.Bytes())-1], false},
		{"oversized before allocation", oversized, false},
		{"too many rows", tooMany, false},
		{"trailing output", append(append([]byte{}, valid.Bytes()...), 'x'), false},
		{"unknown frame", []byte("OSR1\x02"), false},
		{"invalid UTF8", []byte("OSR1\x01\x00\x00\x00\x01\x00\x00\x00\x00\xff\x00"), false},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := decodeSQLiteRows(bytes.NewReader(test.data), func(string, string) error { return nil })
			if (err == nil) != test.ok {
				t.Fatalf("protocol acceptance = %v", err)
			}
		})
	}
}

func TestSQLiteWorkerFailureAtomicAndReaped(t *testing.T) {
	for _, mode := range []string{"bad", "oversized", "truncated", "nonzero", "hang", "hang", "slow-end"} {
		t.Run(mode, func(t *testing.T) {
			home := hardeningHome(t)
			writeOpenCodeDB(t, home, [][2]string{{"good", "/good"}})
			path := filepath.Join(home, ".local/share/opencode/opencode.db")
			writeFile(t, path+".test-control", mode)
			start := time.Now()
			if got, err := readDBProjectRecords(path); err == nil || len(got) != 0 {
				t.Fatalf("worker failure retained partial source: %d %v", len(got), err)
			}
			if elapsed := time.Since(start); elapsed > 1500*time.Millisecond {
				t.Fatalf("worker exceeded parent budget with margin: %s", elapsed)
			}
			assertSQLiteWorkerReaped(t, path)
		})
	}
}

func assertSQLiteWorkerReaped(t *testing.T, path string) {
	t.Helper()
	data, err := os.ReadFile(path + ".test-pid")
	if err != nil {
		t.Fatal("worker did not run")
	}
	pid, err := strconv.Atoi(string(data))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := syscall.Wait4(pid, nil, syscall.WNOHANG, nil); !errors.Is(err, syscall.ECHILD) {
		t.Fatalf("worker was not already reaped: %v", err)
	}
}

func TestSQLiteWorkerCancellationReaps(t *testing.T) {
	for i := 0; i < 3; i++ {
		t.Run(strconv.Itoa(i), func(t *testing.T) {
			home := hardeningHome(t)
			writeOpenCodeDB(t, home, [][2]string{{"good", "/good"}})
			path := filepath.Join(home, ".local/share/opencode/opencode.db")
			writeFile(t, path+".test-control", "hang")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() {
				done <- readSQLiteSourceContext(ctx, path, "project", func(string, string) error { return nil })
			}()
			tick := time.NewTicker(5 * time.Millisecond)
			defer tick.Stop()
			deadline := time.NewTimer(900 * time.Millisecond)
			defer deadline.Stop()
			for {
				if _, err := os.Stat(path + ".test-pid"); err == nil {
					break
				}
				select {
				case <-tick.C:
				case <-deadline.C:
					cancel()
					t.Fatal("worker did not reach the cancellation point")
				}
			}
			cancel()
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("worker ignored caller cancellation: %v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("cancelled worker did not return")
			}
			assertSQLiteWorkerReaped(t, path)
		})
	}
}

func TestSQLiteWorkerEnvironmentIsOperationalOnly(t *testing.T) {
	home := hardeningHome(t)
	for _, key := range []string{"OMAC_SERVE_PASSWORD", "OPENCODE_SERVER_PASSWORD", "OPENAI_API_KEY", "OMAC_CONTROL_TOKEN", "OMAC_SOCKET", "GODEBUG", "OMAC_TEST_SQLITE_REPLACEMENT"} {
		t.Setenv(key, "must-not-reach-worker")
	}
	writeOpenCodeDB(t, home, [][2]string{{"good", "/good"}})
	path := filepath.Join(home, ".local/share/opencode/opencode.db")
	writeFile(t, path+".test-control", "env")
	if got := dbProjectRecords(path); len(got) != 1 {
		t.Fatal("worker failed with unrelated parent credentials present")
	}
	data, err := os.ReadFile(path + ".test-env")
	if err != nil {
		t.Fatal(err)
	}
	var env []string
	if err := json.Unmarshal(data, &env); err != nil {
		t.Fatal(err)
	}
	if len(env) != 2 {
		t.Fatalf("unexpected operational environment: %v", env)
	}
	for _, entry := range env {
		if !strings.HasPrefix(entry, "TMPDIR=") && entry != "GORACE=atexit_sleep_ms=0" {
			t.Fatalf("worker inherited a non-operational variable: %s", strings.SplitN(entry, "=", 2)[0])
		}
	}
}
