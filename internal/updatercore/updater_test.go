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
	writeTestFile(t, filepath.Join(root, "delete.txt"), oldDelete)
	writeTestFile(t, filepath.Join(root, "user.txt"), []byte("user data"))

	installed := Manifest{Version: "1.0.0", Files: []FileEntry{
		entryForBytes("app.txt", oldApp),
		entryForBytes("delete.txt", oldDelete),
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
	if _, err := os.Stat(filepath.Join(root, "delete.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("delete.txt still exists or stat failed unexpectedly: %v", err)
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
