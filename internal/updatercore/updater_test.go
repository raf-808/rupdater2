package updatercore

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type testUI struct{}

func (testUI) ConfirmPlan(Plan) bool                       { return true }
func (testUI) ConfirmProcessTermination([]LockedFile) bool { return true }
func (testUI) Progress(ProgressEvent)                      {}
func (testUI) Info(string)                                 {}
func (testUI) Error(string)                                {}
func (testUI) ShowVersionInfo(string, string)              {}

type recordingUI struct {
	progresses []ProgressEvent
	infos      []string
}

func (ui *recordingUI) ConfirmPlan(Plan) bool                       { return true }
func (ui *recordingUI) ConfirmProcessTermination([]LockedFile) bool { return true }
func (ui *recordingUI) Progress(event ProgressEvent)                { ui.progresses = append(ui.progresses, event) }
func (ui *recordingUI) Info(message string)                         { ui.infos = append(ui.infos, message) }
func (ui *recordingUI) Error(string)                                {}
func (ui *recordingUI) ShowVersionInfo(string, string)              {}

func TestRunEndToEndUpdatesManagedFilesAndPreservesUnknownFiles(t *testing.T) {
	root := t.TempDir()
	oldApp := []byte("old app")
	oldDelete := []byte("delete me")
	newApp := []byte("new app")
	newFile := []byte("new managed file")

	writeTestFile(t, filepath.Join(root, "app.txt"), oldApp)
	writeTestFile(t, filepath.Join(root, "delete.bin"), oldDelete)
	writeTestFile(t, filepath.Join(root, "user.txt"), []byte("user data"))

	installed := Manifest{Version: "1.0.0", Files: []FileEntry{
		entryForBytes("app.txt", oldApp),
		entryForBytes("delete.bin", oldDelete),
	}}
	remote := Manifest{Version: "2.0.0", Files: []FileEntry{
		entryForBytes("app.txt", newApp),
		entryForBytes("new.txt", newFile),
	}}
	server := updateServer(t, remote, map[string][]byte{
		"app.txt": newApp,
		"new.txt": newFile,
	})
	defer server.Close()

	writeInitialState(t, root, "1.0.0", installed, server.URL+"/latest.json")
	err := Run(context.Background(), Options{
		RootDir:     root,
		AutoConfirm: true,
		Silent:      true,
		UI:          testUI{},
		ExePath:     filepath.Join(root, "Updater.exe"),
	})
	if err != nil {
		t.Fatal(err)
	}

	assertFileContent(t, filepath.Join(root, "app.txt"), newApp)
	assertFileContent(t, filepath.Join(root, "new.txt"), newFile)
	assertFileContent(t, filepath.Join(root, "user.txt"), []byte("user data"))
	if _, err := os.Stat(filepath.Join(root, "delete.bin")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("delete.bin still exists or stat failed unexpectedly: %v", err)
	}
	var version VersionState
	if err := ReadJSONFile(statePath(root, "version.json"), &version); err != nil {
		t.Fatal(err)
	}
	if version.Version != "2.0.0" {
		t.Fatalf("version = %q", version.Version)
	}
	assertNotExists(t, statePath(root, "session.json"))
	assertNotExists(t, statePath(root, "staging"))
	assertNotExists(t, statePath(root, "rollback"))
}

func TestRunHashFailureDoesNotTouchInstallRootOrCommittedState(t *testing.T) {
	root := t.TempDir()
	oldApp := []byte("old app")
	expected := []byte("expected app")
	writeTestFile(t, filepath.Join(root, "app.txt"), oldApp)

	installed := Manifest{Version: "1.0.0", Files: []FileEntry{entryForBytes("app.txt", oldApp)}}
	remote := Manifest{Version: "2.0.0", Files: []FileEntry{entryForBytes("app.txt", expected)}}
	server := updateServer(t, remote, map[string][]byte{"app.txt": []byte("corrupt app")})
	defer server.Close()

	writeInitialState(t, root, "1.0.0", installed, server.URL+"/latest.json")
	err := Run(context.Background(), Options{
		RootDir:     root,
		AutoConfirm: true,
		Silent:      true,
		UI:          testUI{},
		ExePath:     filepath.Join(root, "Updater.exe"),
	})
	if err == nil {
		t.Fatal("expected hash failure")
	}
	assertFileContent(t, filepath.Join(root, "app.txt"), oldApp)
	var version VersionState
	if err := ReadJSONFile(statePath(root, "version.json"), &version); err != nil {
		t.Fatal(err)
	}
	if version.Version != "1.0.0" {
		t.Fatalf("version changed to %q", version.Version)
	}
	assertNotExists(t, statePath(root, "session.json"))
}

func TestRecoverFromSessionRestoresBackupsAndRemovesSwitchedFiles(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "app.txt"), []byte("new"))
	backup := filepath.Join(root, StateDirName, "rollback", "app.txt")
	writeTestFile(t, backup, []byte("old"))
	writeTestFile(t, filepath.Join(root, StateDirName, "staging", "leftover.txt"), []byte("leftover"))
	session := Session{
		Phase:    "Switch",
		Switched: []MovedFile{{Path: "app.txt"}},
		BackedUp: []MovedFile{{Path: "app.txt", BackupPath: backup}},
	}
	if err := WriteJSONAtomic(statePath(root, "session.json"), session); err != nil {
		t.Fatal(err)
	}

	if err := recoverIfNeeded(filepath.Join(root, StateDirName), root, testUI{}); err != nil {
		t.Fatal(err)
	}
	assertFileContent(t, filepath.Join(root, "app.txt"), []byte("old"))
	assertNotExists(t, statePath(root, "session.json"))
	assertNotExists(t, statePath(root, "staging"))
	assertNotExists(t, statePath(root, "rollback"))
}

func TestSwitchFilesDefersUpdaterExeReplacement(t *testing.T) {
	root := t.TempDir()
	exePath := filepath.Join(root, "Updater.exe")
	writeTestFile(t, exePath, []byte("old updater"))
	entry := entryForBytes("Updater.exe", []byte("new updater"))
	staged, err := stagingPath(root, "Updater.exe")
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, staged, []byte("new updater"))

	session := Session{}
	plan := Plan{Modify: []FileEntry{entry}}
	if err := switchFiles(root, filepath.Join(root, StateDirName), plan, exePath, &session, testUI{}); err != nil {
		t.Fatal(err)
	}
	assertFileContent(t, exePath, []byte("old updater"))
	assertFileContent(t, pendingSelfUpdatePath(root), []byte("new updater"))
	if _, ok, err := readSelfUpdateMarker(root); err != nil || !ok {
		t.Fatalf("self update marker ok=%v err=%v", ok, err)
	}
}

func TestRunNoChangePathReportsCommitProgress(t *testing.T) {
	root := t.TempDir()
	current := []byte("same app")
	writeTestFile(t, filepath.Join(root, "app.txt"), current)

	manifest := Manifest{Version: "2.0.0", Files: []FileEntry{
		entryForBytes("app.txt", current),
	}}
	server := updateServer(t, manifest, map[string][]byte{
		"app.txt": current,
	})
	defer server.Close()

	writeInitialState(t, root, "1.0.0", manifest, server.URL+"/latest.json")
	ui := &recordingUI{}
	err := Run(context.Background(), Options{
		RootDir:     root,
		AutoConfirm: true,
		Silent:      true,
		UI:          ui,
		ExePath:     filepath.Join(root, "Updater.exe"),
	})
	if err != nil {
		t.Fatal(err)
	}

	foundCommit := false
	for _, event := range ui.progresses {
		if event.Phase == "Commit" {
			foundCommit = true
			break
		}
	}
	if !foundCommit {
		t.Fatalf("expected Commit progress event, got %#v", ui.progresses)
	}
}

func TestRunUsesRemoteCandidatePolicyForCurrentPlanAndInstallsPolicy(t *testing.T) {
	root := t.TempDir()
	old := []byte("server old")
	writeTestFile(t, filepath.Join(root, "user.bin"), []byte("user changed"))
	policyData := []byte(`{"schema_version":1,"preserve":{"include":{"extensions":[".bin"]}}}`)
	installed := Manifest{Version: "1", Files: []FileEntry{entryForBytes("user.bin", old)}}
	remote := Manifest{Version: "2", Files: []FileEntry{
		entryForBytes("user.bin", []byte("server new")),
		entryForBytes(PolicyFileName, policyData),
	}}
	server := updateServer(t, remote, map[string][]byte{
		"user.bin":     []byte("server new"),
		PolicyFileName: policyData,
	})
	defer server.Close()
	writeInitialState(t, root, "1", installed, server.URL+"/latest.json")

	if err := Run(context.Background(), Options{
		RootDir: root, AutoConfirm: true, Silent: true, UI: testUI{},
		ExePath: filepath.Join(root, "Updater.exe"),
	}); err != nil {
		t.Fatal(err)
	}
	assertFileContent(t, filepath.Join(root, "user.bin"), []byte("user changed"))
	assertFileContent(t, filepath.Join(root, PolicyFileName), policyData)
	assertNotExists(t, statePath(root, PolicyCandidateName))
	assertNotExists(t, statePath(root, PolicyCandidateName)+".download")
}

func TestRunRejectsPolicyCapabilityDowngradeBeforeChangingState(t *testing.T) {
	root := t.TempDir()
	old := []byte("old")
	writeTestFile(t, filepath.Join(root, "app.bin"), old)
	installed := Manifest{Version: "1", Files: []FileEntry{
		entryForBytes("app.bin", old),
		entryForBytes(PolicyFileName, []byte(`{"schema_version":1}`)),
	}}
	remote := Manifest{Version: "2", Files: []FileEntry{entryForBytes("app.bin", []byte("new"))}}
	server := updateServer(t, remote, map[string][]byte{"app.bin": []byte("new")})
	defer server.Close()
	writeInitialState(t, root, "1", installed, server.URL+"/latest.json")

	err := Run(context.Background(), Options{
		RootDir: root, AutoConfirm: true, Silent: true, UI: testUI{},
		ExePath: filepath.Join(root, "Updater.exe"),
	})
	if err == nil || PolicyFailureReason(err) != "policy_missing_manifest_entry" {
		t.Fatalf("expected policy downgrade error, got %v (%s)", err, PolicyFailureReason(err))
	}
	assertFileContent(t, filepath.Join(root, "app.bin"), old)
	assertNotExists(t, statePath(root, "session.json"))
}

func TestRunRejectsOversizedPolicyDeclarationWithoutDownloading(t *testing.T) {
	root := t.TempDir()
	var policyRequests int
	remote := Manifest{Version: "2", Files: []FileEntry{{
		Path: PolicyFileName, Size: MaxPolicySize + 1, HeadHash: "x", TailHash: "x",
	}}}
	var serverURL string
	mux := http.NewServeMux()
	mux.HandleFunc("/latest.json", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, LatestInfo{Version: "2", ManifestURL: serverURL + "/manifest.json", FilesBaseURL: serverURL + "/files/"})
	})
	mux.HandleFunc("/manifest.json", func(w http.ResponseWriter, r *http.Request) { writeJSON(t, w, remote) })
	mux.HandleFunc("/files/"+PolicyFileName, func(w http.ResponseWriter, r *http.Request) {
		policyRequests++
		_, _ = w.Write([]byte(`{"schema_version":1}`))
	})
	server := httptest.NewServer(mux)
	serverURL = server.URL
	defer server.Close()
	writeInitialState(t, root, "1", Manifest{Version: "1"}, server.URL+"/latest.json")

	err := Run(context.Background(), Options{
		RootDir: root, AutoConfirm: true, Silent: true, UI: testUI{},
		ExePath: filepath.Join(root, "Updater.exe"),
	})
	if err == nil || PolicyFailureReason(err) != "policy_too_large" {
		t.Fatalf("expected policy_too_large, got %v", err)
	}
	if policyRequests != 0 {
		t.Fatalf("policy requests = %d, want 0", policyRequests)
	}
	assertNotExists(t, statePath(root, PolicyCandidateName))
	assertNotExists(t, statePath(root, PolicyCandidateName)+".download")
}

func TestRunRejectsOversizedOrInvalidCandidateAndCleansTemporaryFiles(t *testing.T) {
	for name, policyData := range map[string][]byte{
		"oversized response": append([]byte(`{"schema_version":1}`), make([]byte, MaxPolicySize)...),
		"hash mismatch":      []byte(`{"schema_version":1}`),
		"trailing JSON":      []byte(`{"schema_version":1} {}`),
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			entry := entryForBytes(PolicyFileName, policyData)
			if name == "oversized response" {
				entry = entryForBytes(PolicyFileName, []byte(`{"schema_version":1}`))
			} else if name == "hash mismatch" {
				entry = entryForBytes(PolicyFileName, []byte(`{"schema_version":1,"publish":{}}`))
			}
			remote := Manifest{Version: "2", Files: []FileEntry{entry}}
			server := updateServer(t, remote, map[string][]byte{PolicyFileName: policyData})
			defer server.Close()
			writeInitialState(t, root, "1", Manifest{Version: "1"}, server.URL+"/latest.json")

			err := Run(context.Background(), Options{
				RootDir: root, AutoConfirm: true, Silent: true, UI: testUI{},
				ExePath: filepath.Join(root, "Updater.exe"),
			})
			if err == nil {
				t.Fatal("expected candidate policy failure")
			}
			if name == "hash mismatch" && PolicyFailureReason(err) != "policy_hash_mismatch" {
				t.Fatalf("failure reason = %s, want policy_hash_mismatch: %v", PolicyFailureReason(err), err)
			}
			if name == "oversized response" && PolicyFailureReason(err) != "policy_too_large" {
				t.Fatalf("failure reason = %s, want policy_too_large: %v", PolicyFailureReason(err), err)
			}
			if name == "trailing JSON" && PolicyFailureReason(err) != "policy_invalid_json" {
				t.Fatalf("failure reason = %s, want policy_invalid_json: %v", PolicyFailureReason(err), err)
			}
			assertNotExists(t, statePath(root, PolicyCandidateName))
			assertNotExists(t, statePath(root, PolicyCandidateName)+".download")
			assertNotExists(t, statePath(root, "session.json"))
		})
	}
}

func updateServer(t *testing.T, manifest Manifest, files map[string][]byte) *httptest.Server {
	t.Helper()
	var serverURL string
	mux := http.NewServeMux()
	mux.HandleFunc("/latest.json", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, LatestInfo{
			Version:      manifest.Version,
			ManifestURL:  serverURL + "/manifest.json",
			FilesBaseURL: serverURL + "/files/",
		})
	})
	mux.HandleFunc("/manifest.json", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, manifest)
	})
	mux.HandleFunc("/files/", func(w http.ResponseWriter, r *http.Request) {
		rel := strings.TrimPrefix(r.URL.Path, "/files/")
		data, ok := files[rel]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(data)
	})
	server := httptest.NewServer(mux)
	serverURL = server.URL
	return server
}

func writeInitialState(t *testing.T, root, version string, manifest Manifest, latestURL string) {
	t.Helper()
	if err := WriteJSONAtomic(statePath(root, "config.json"), Config{LatestURL: latestURL}); err != nil {
		t.Fatal(err)
	}
	if err := WriteJSONAtomic(statePath(root, "version.json"), VersionState{Version: version}); err != nil {
		t.Fatal(err)
	}
	if err := WriteJSONAtomic(statePath(root, "installed_manifest.json"), manifest); err != nil {
		t.Fatal(err)
	}
}

func writeJSON(t *testing.T, w http.ResponseWriter, value any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(value); err != nil {
		t.Fatal(err)
	}
}

func assertFileContent(t *testing.T, fileName string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(fileName)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatalf("%s = %q, want %q", fileName, got, want)
	}
}

func assertNotExists(t *testing.T, fileName string) {
	t.Helper()
	if _, err := os.Stat(fileName); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("%s exists or stat failed unexpectedly: %v", fileName, err)
	}
}
