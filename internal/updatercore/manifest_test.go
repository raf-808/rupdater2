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

func TestGenerateManifestWithPolicyAppliesPublishRulesAndSystemInvariants(t *testing.T) {
	root := t.TempDir()
	for path, data := range map[string][]byte{
		"apps/app.exe":                       []byte("app"),
		"apps/cache/temp.bin":                []byte("cache"),
		"other.bin":                          []byte("other"),
		"latest.json":                        []byte("{}"),
		"manifest.json":                      []byte("{}"),
		"updater-policy.json":                []byte(`{"schema_version":1}`),
		"updater_state/forced-by-policy.exe": []byte("state"),
	} {
		writeTestFile(t, filepath.Join(root, filepath.FromSlash(path)), data)
	}
	policy, err := ParseFilePolicy([]byte(`{
		"schema_version":1,
		"publish":{
			"include":{"directories":["apps"],"files":["latest.json"],"extensions":[".json"]},
			"exclude":{"directories":["apps/cache"],"files":["updater-policy.json"]}
		}
	}`))
	if err != nil {
		t.Fatal(err)
	}

	manifest, summary, err := GenerateManifestWithPolicy(root, "7.0.0", policy)
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, entry := range manifest.Files {
		paths = append(paths, entry.Path)
	}
	got := strings.Join(paths, ",")
	if got != "apps/app.exe,updater-policy.json" {
		t.Fatalf("manifest paths = %q", got)
	}
	if summary.ScannedCount != 4 || summary.IncludedCount != 2 || summary.ExcludedCount != 2 {
		t.Fatalf("unexpected summary: %#v", summary)
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

func TestCompareManifestsWithProgressReportsPlanProgress(t *testing.T) {
	root := t.TempDir()
	remote := Manifest{
		Version: "2.0.0",
		Files: []FileEntry{
			entryForBytes("app.exe", []byte("new")),
			entryForBytes("dir/data.bin", []byte("payload")),
			entryForBytes("config.ini", []byte("k=v")),
		},
	}

	var events []ProgressEvent
	plan, err := CompareManifestsWithProgress(root, nil, remote, 4, func(event ProgressEvent) {
		events = append(events, event)
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Add) != len(remote.Files) {
		t.Fatalf("add count = %d, want %d", len(plan.Add), len(remote.Files))
	}
	if len(events) == 0 {
		t.Fatal("expected progress events")
	}
	last := events[len(events)-1]
	if last.Phase != "Plan" {
		t.Fatalf("last phase = %q", last.Phase)
	}
	if last.CompletedFiles != len(remote.Files) || last.TotalFiles != len(remote.Files) {
		t.Fatalf("unexpected progress totals: %#v", last)
	}
	if strings.TrimSpace(last.CurrentFile) == "" {
		t.Fatalf("expected last current file to be set: %#v", last)
	}
}

func TestCompareManifestsWithProgressParallelWorkersMatchSingleWorker(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "keep.bin"), []byte("same"))
	writeTestFile(t, filepath.Join(root, "change.bin"), []byte("old"))
	installed := Manifest{Version: "1.0.0", Files: []FileEntry{
		entryForBytes("keep.bin", []byte("same")),
		entryForBytes("change.bin", []byte("old")),
	}}
	remote := Manifest{Version: "2.0.0", Files: []FileEntry{
		entryForBytes("keep.bin", []byte("same")),
		entryForBytes("change.bin", []byte("new")),
		entryForBytes("add.bin", []byte("fresh")),
	}}

	single, err := CompareManifestsWithProgress(root, &installed, remote, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	parallel, err := CompareManifestsWithProgress(root, &installed, remote, 4, nil)
	if err != nil {
		t.Fatal(err)
	}

	if len(single.Add) != len(parallel.Add) || len(single.Modify) != len(parallel.Modify) || len(single.Delete) != len(parallel.Delete) {
		t.Fatalf("single=%#v parallel=%#v", single, parallel)
	}
	if single.DownloadSize != parallel.DownloadSize {
		t.Fatalf("download size single=%d parallel=%d", single.DownloadSize, parallel.DownloadSize)
	}
	if parallel.Add[0].Path != "add.bin" || parallel.Modify[0].Path != "change.bin" {
		t.Fatalf("unexpected parallel plan: %#v", parallel)
	}
}

func TestCompareManifestsWithPolicyProtectsOnlyUserModifiedFiles(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "user.ini"), []byte("user changed"))
	writeTestFile(t, filepath.Join(root, "clean.ini"), []byte("old clean"))
	writeTestFile(t, filepath.Join(root, "system.ini"), []byte("old system"))
	writeTestFile(t, filepath.Join(root, "removed.ini"), []byte("keep removed"))

	installed := Manifest{Version: "1", Files: []FileEntry{
		entryForBytes("user.ini", []byte("old user")),
		entryForBytes("clean.ini", []byte("old clean")),
		entryForBytes("system.ini", []byte("old system")),
		entryForBytes("removed.ini", []byte("old removed")),
	}}
	remote := Manifest{Version: "2", Files: []FileEntry{
		entryForBytes("user.ini", []byte("new user")),
		entryForBytes("clean.ini", []byte("new clean")),
		entryForBytes("system.ini", []byte("new system")),
	}}
	policy, err := ParseFilePolicy([]byte(`{
		"schema_version":1,
		"preserve":{
			"include":{"extensions":[".ini"]},
			"exclude":{"files":["system.ini"]}
		}
	}`))
	if err != nil {
		t.Fatal(err)
	}

	single, singleSummary, err := CompareManifestsWithPolicySummary(root, &installed, remote, policy, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	parallel, parallelSummary, err := CompareManifestsWithPolicySummary(root, &installed, remote, policy, 4, nil)
	if err != nil {
		t.Fatal(err)
	}
	for name, result := range map[string]struct {
		plan    Plan
		summary PolicyPlanSummary
	}{
		"single":   {plan: single, summary: singleSummary},
		"parallel": {plan: parallel, summary: parallelSummary},
	} {
		plan := result.plan
		if len(plan.Modify) != 2 || plan.Modify[0].Path != "clean.ini" || plan.Modify[1].Path != "system.ini" {
			t.Fatalf("%s unexpected modify: %#v", name, plan.Modify)
		}
		if len(plan.Delete) != 0 {
			t.Fatalf("%s protected delete unexpectedly planned: %#v", name, plan.Delete)
		}
		if result.summary.PreservedModify != 1 || result.summary.PreservedDelete != 1 {
			t.Fatalf("%s preserved counts = %d/%d", name, result.summary.PreservedModify, result.summary.PreservedDelete)
		}
	}
}

func TestCompareManifestsWithPolicyStillRejectsUnknownTarget(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "unknown.ini"), []byte("local"))
	policy, err := ParseFilePolicy([]byte(`{"schema_version":1,"preserve":{"include":{"extensions":[".ini"]}}}`))
	if err != nil {
		t.Fatal(err)
	}
	remote := Manifest{Version: "2", Files: []FileEntry{entryForBytes("unknown.ini", []byte("remote"))}}
	if _, err := CompareManifestsWithPolicy(root, &Manifest{Version: "1"}, remote, policy, 2, nil); err == nil ||
		!strings.Contains(err.Error(), "保护未知用户文件") {
		t.Fatalf("expected unknown target conflict, got %v", err)
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
