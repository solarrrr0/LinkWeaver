package main

import (
	"archive/zip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestLocalUpdateIntegrityAndTamperDetection(t *testing.T) {
	w := fixtureWorkspace(t, false)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	binary, err := os.ReadFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(binary)
	hash := hex.EncodeToString(digest[:])
	m := releaseManifest{Product: "LinkWeaver", Version: "0.3.1", Files: map[string]string{"LinkWeaver.exe": hash, "bin/mihomo.exe": hash}}
	manifest, _ := json.Marshal(m)
	path := filepath.Join(t.TempDir(), "fixture.zip")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	archive := zip.NewWriter(f)
	for name, b := range map[string][]byte{"fixture/release.json": manifest, "fixture/LinkWeaver.exe": binary, "fixture/bin/mihomo.exe": binary, "fixture/../../outside.txt": []byte("must not extract")} {
		entry, err := archive.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = entry.Write(b)
	}
	_ = archive.Close()
	_ = f.Close()
	sum, err := fileSHA(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.prepareUpdate(path, "bad"); err == nil {
		t.Fatal("invalid SHA accepted")
	}
	if _, err := w.prepareUpdate(path, string(make([]byte, 64))); err == nil {
		t.Fatal("wrong SHA accepted")
	}
	plan, err := w.prepareUpdate(path, sum)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Version != "0.3.1" {
		t.Fatal("version lost")
	}
	if _, err = w.preparedUpdate(); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(filepath.Join(w.root, "runtime", "outside.txt")); !os.IsNotExist(err) {
		t.Fatal("zip traversal entry extracted")
	}
	_ = os.WriteFile(filepath.Join(plan.Stage, "LinkWeaver.exe"), []byte("tampered"), 0600)
	if _, err = w.preparedUpdate(); err == nil {
		t.Fatal("staged tampering accepted")
	}
}
