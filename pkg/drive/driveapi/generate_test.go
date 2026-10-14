package driveapi

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestGeneratedMatchesSpec: api.gen.go is what the spec generates now
// (run go generate ./pkg/drive/driveapi after changing the spec).
func TestGeneratedMatchesSpec(t *testing.T) {
	if testing.Short() {
		t.Skip("runs the code generator")
	}
	dir := t.TempDir()
	out := filepath.Join(dir, "api.gen.go")
	cfg, err := os.ReadFile("cfg.yaml")
	if err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(dir, "cfg.yaml")
	cfg = bytes.Replace(cfg, []byte("output: api.gen.go"), []byte("output: "+out), 1)
	if err := os.WriteFile(cfgPath, cfg, 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("go", "run", "github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen",
		"--config="+cfgPath, "../../../spec/openapi/drive.yaml")
	if msg, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("oapi-codegen: %v\n%s", err, msg)
	}
	want, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	have, err := os.ReadFile("api.gen.go")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(want, have) {
		t.Fatal("api.gen.go is stale: run go generate ./pkg/drive/driveapi")
	}
}
