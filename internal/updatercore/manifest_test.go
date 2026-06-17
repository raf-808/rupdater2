package updatercore

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGenerateManifestSkipsUpdaterStateAndUsesSHA256Samples(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "app.txt"), []byte("abc"))
	writeTestFile(t, filepath.Join(root, StateDirName, "config.json"), []byte("{}"))

	manifest, err := GenerateManifest(root, "6.0.0")
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Version != "6.0.0" {
		t.Fatalf("version = %q", manifest.Version)
	}
	if len(manifest.Files) != 1 {
		t.Fatalf("files = %d, want 1: %#v", len(manifest.Files), manifest.Files)
	}
	wantHash := sha256Hex([]byte("abc"))
	got := manifest.Files[0]
	if got.Path != "app.txt" || got.Size != 3 || got.HeadHash != wantHash || got.TailHash != wantHash {
		t.Fatalf("unexpected entry: %#v", got)
	}
}

func TestGenerateManifestSkipsMetadataFiles(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "app.txt"), []byte("abc"))
	writeTestFile(t, filepath.Join(root, "latest.json"), []byte(`{"version":"1.0.0"}`))
	writeTestFile(t, filepath.Join(root, "manifest.json"), []byte(`{"version":"1.0.0","files":[]}`))

	manifest, err := GenerateManifest(root, "6.0.0")
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest.Files) != 1 {
		t.Fatalf("files = %d, want 1: %#v", len(manifest.Files), manifest.Files)
	}
	if manifest.Files[0].Path != "app.txt" {
		t.Fatalf("unexpected file path: %q", manifest.Files[0].Path)
	}
}

func TestCompareManifestsLargeSetAndUnknownFileProtection(t *testing.T) {
	root := t.TempDir()
	installed := Manifest{Version: "1.0.0"}
	remote := Manifest{Version: "2.0.0"}
	for i := 0; i < 10000; i++ {
		entry := FileEntry{
			Path:     "dir/file-" + padInt(i) + ".bin",
			Size:     10,
			HeadHash: "old",
			TailHash: "old",
		}
		installed.Files = append(installed.Files, entry)
		if i == 9999 {
			continue
		}
		if i == 1234 {
			entry.Size = 11
			entry.HeadHash = "new"
			entry.TailHash = "new"
		}
		remote.Files = append(remote.Files, entry)
	}
	remote.Files = append(remote.Files, FileEntry{Path: "new.bin", Size: 1, HeadHash: "n", TailHash: "n"})

	plan, err := CompareManifests(root, &installed, remote)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Add) != 1 || len(plan.Modify) != 1 || len(plan.Delete) != 1 {
		t.Fatalf("counts add/modify/delete = %d/%d/%d", len(plan.Add), len(plan.Modify), len(plan.Delete))
	}
	if plan.Add[0].Path != "new.bin" || plan.Modify[0].Path != "dir/file-01234.bin" || plan.Delete[0].Path != "dir/file-09999.bin" {
		t.Fatalf("unexpected plan: %#v", plan)
	}

	writeTestFile(t, filepath.Join(root, "conflict.txt"), []byte("local user data"))
	remoteConflict := Manifest{Version: "2.0.0", Files: []FileEntry{entryForBytes("conflict.txt", []byte("remote data"))}}
	_, err = CompareManifests(root, &Manifest{Version: "1.0.0"}, remoteConflict)
	if err == nil || !strings.Contains(err.Error(), "保护未知用户文件") {
		t.Fatalf("expected unknown file protection error, got %v", err)
	}
}

func writeTestFile(t *testing.T, fileName string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(fileName), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fileName, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func entryForBytes(path string, data []byte) FileEntry {
	hash := sha256Hex(data)
	return FileEntry{Path: path, Size: int64(len(data)), HeadHash: hash, TailHash: hash}
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func padInt(value int) string {
	text := "00000" + strconvItoa(value)
	return text[len(text)-5:]
}

func strconvItoa(value int) string {
	if value == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for value > 0 {
		i--
		buf[i] = byte('0' + value%10)
		value /= 10
	}
	return string(buf[i:])
}
