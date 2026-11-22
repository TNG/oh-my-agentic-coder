package mcpreg

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TNG/oh-my-agentic-coder/internal/mcp/mcppolicy"
	"github.com/TNG/oh-my-agentic-coder/internal/secrets"
)

func classifiedDef(t *testing.T, name string) *Definition {
	t.Helper()
	def, err := ParseDefinition([]byte(defYAML(name)))
	if err != nil {
		t.Fatal(err)
	}
	def.Tools = []mcppolicy.Tool{{Name: "get", Class: mcppolicy.Read}}
	def.Access = mcppolicy.ReadWrite
	def.Review = &mcppolicy.Review{ToolsHash: mcppolicy.ListHash(map[string]string{"get": "h"}), Tools: map[string]string{"get": "h"}}
	return def
}

func TestRegisterDefinitionMatchesRegister(t *testing.T) {
	isolate(t)
	viaFile, err := Register(GlobalScope(), writeDef(t, defYAML("one")), []string{"claude"})
	if err != nil {
		t.Fatal(err)
	}
	def, err := ParseDefinition([]byte(defYAML("two")))
	if err != nil {
		t.Fatal(err)
	}
	viaDef, err := RegisterDefinition(GlobalScope(), def, []string{"claude"})
	if err != nil {
		t.Fatal(err)
	}
	viaDef2, err := RegisterDefinition(GlobalScope(), mustParse(t, defYAML("one")), []string{"claude"})
	if err != nil {
		t.Fatal(err)
	}
	if viaFile.BundleHash != viaDef2.BundleHash {
		t.Fatalf("hash differs: %s vs %s", viaFile.BundleHash, viaDef2.BundleHash)
	}
	if viaDef.BundleHash == viaFile.BundleHash {
		t.Fatal("different names must hash differently")
	}
}

func mustParse(t *testing.T, body string) *Definition {
	t.Helper()
	def, err := ParseDefinition([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	return def
}

func TestUpdateKeepsOldSnapshotAndPolicy(t *testing.T) {
	isolate(t)
	first, err := RegisterDefinition(GlobalScope(), classifiedDef(t, "srv"), []string{"claude"})
	if err != nil {
		t.Fatal(err)
	}
	updated, err := Update(GlobalScope(), "srv", func(d *Definition) error {
		d.Server.Command = []string{"npx", "-y", "other-server"}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if updated.BundleHash == first.BundleHash || updated.SnapshotDir == first.SnapshotDir {
		t.Fatal("update must create a new snapshot")
	}
	if _, err := os.Stat(first.DefPath); err != nil {
		t.Fatalf("old snapshot must stay: %v", err)
	}
	old, err := os.ReadFile(first.DefPath)
	if err != nil {
		t.Fatal(err)
	}
	oldDef, err := ParseDefinition(old)
	if err != nil || oldDef.Server.Command[2] != "some-server" {
		t.Fatalf("old snapshot changed: %v %v", oldDef, err)
	}
	if len(updated.Def.Tools) != 1 || updated.Def.Review == nil || updated.Def.Access != mcppolicy.ReadWrite {
		t.Fatalf("classes, review and access must survive: %+v", updated.Def)
	}
	loaded, err := Load(GlobalScope(), "srv")
	if err != nil || loaded.BundleHash != updated.BundleHash {
		t.Fatalf("record not updated: %v", err)
	}
}

func TestUpdateRefusesInvalidResult(t *testing.T) {
	isolate(t)
	first, err := RegisterDefinition(GlobalScope(), classifiedDef(t, "srv"), []string{"claude"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Update(GlobalScope(), "srv", func(d *Definition) error { d.Server.Command = nil; return nil }); err == nil {
		t.Fatal("invalid definition must be refused")
	}
	if _, err := Update(GlobalScope(), "srv", func(d *Definition) error { d.Name = "renamed"; return nil }); err == nil {
		t.Fatal("rename must be refused")
	}
	loaded, err := Load(GlobalScope(), "srv")
	if err != nil || loaded.BundleHash != first.BundleHash {
		t.Fatalf("failed update must not change the registration: %v", err)
	}
}

func TestGatewayHoldingOldSnapshotSurvivesUpdate(t *testing.T) {
	isolate(t)
	first, err := RegisterDefinition(GlobalScope(), classifiedDef(t, "srv"), []string{"claude"})
	if err != nil {
		t.Fatal(err)
	}
	gatewaySnapshot := first.SnapshotDir
	if _, err := Update(GlobalScope(), "srv", func(d *Definition) error { d.Review = nil; return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := Register(GlobalScope(), writeDef(t, defYAML("srv")), []string{"claude"}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(gatewaySnapshot, snapshotFile))
	if err != nil {
		t.Fatalf("gateway lost its snapshot: %v", err)
	}
	def, err := ParseDefinition(raw)
	if err != nil || def.Name != "srv" || def.Review == nil {
		t.Fatalf("gateway snapshot unreadable: %v", err)
	}
}

func TestDeregisterRemovesAllSnapshots(t *testing.T) {
	isolate(t)
	first, err := RegisterDefinition(GlobalScope(), classifiedDef(t, "srv"), []string{"claude"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Update(GlobalScope(), "srv", func(d *Definition) error { d.Review = nil; return nil }); err != nil {
		t.Fatal(err)
	}
	if err := Deregister(GlobalScope(), "srv"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Dir(filepath.Dir(first.SnapshotDir))); !os.IsNotExist(err) {
		t.Fatalf("registration directory must be gone: %v", err)
	}
}

func TestInspectIsolatesCorruptEntry(t *testing.T) {
	isolate(t)
	for _, n := range []string{"alpha", "bravo", "charlie"} {
		if _, err := RegisterDefinition(GlobalScope(), mustParse(t, defYAML(n)), []string{"claude"}); err != nil {
			t.Fatal(err)
		}
	}
	bravo, err := Load(GlobalScope(), "bravo")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bravo.DefPath, []byte("tampered"), 0o600); err != nil {
		t.Fatal(err)
	}
	charlieDir, err := serverDir(GlobalScope(), "charlie")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(charlieDir, recordFile), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	res := Inspect(GlobalScope())
	if len(res) != 3 || res[0].Name != "alpha" || res[1].Name != "bravo" || res[2].Name != "charlie" {
		t.Fatalf("unexpected results: %+v", res)
	}
	if res[0].Err != nil || res[0].Entry == nil {
		t.Fatalf("healthy entry must load: %v", res[0].Err)
	}
	if res[1].Err == nil || res[2].Err == nil {
		t.Fatal("corrupt entries must report errors")
	}
	if _, err := List(GlobalScope()); err == nil {
		t.Fatal("List keeps failing closed")
	}
}

func TestSecretsNeverWrittenToDisk(t *testing.T) {
	isolate(t)
	const secret = "s3cr3t-value-xyz"
	e, err := RegisterDefinition(GlobalScope(), mustParse(t, defYAML("srv")), []string{"claude"})
	if err != nil {
		t.Fatal(err)
	}
	if err := SetSecret(GlobalScope(), e.Name, "API_TOKEN", secrets.NewSecretString(secret)); err != nil {
		t.Fatal(err)
	}
	dir, _ := serverDir(GlobalScope(), "srv")
	err = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		raw, rerr := os.ReadFile(p)
		if rerr != nil {
			return rerr
		}
		if strings.Contains(string(raw), secret) {
			t.Errorf("secret found in %s", p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
