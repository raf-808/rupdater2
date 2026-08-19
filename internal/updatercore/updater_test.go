package updatercore

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
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

type rejectPlanUI struct{ testUI }

func (rejectPlanUI) ConfirmPlan(Plan) bool { return false }

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
		"unsupported schema": []byte(`{"schema_version":2}`),
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
			if name == "unsupported schema" && PolicyFailureReason(err) != "policy_unsupported_schema" {
				t.Fatalf("failure reason = %s, want policy_unsupported_schema: %v", PolicyFailureReason(err), err)
			}
			assertNotExists(t, statePath(root, PolicyCandidateName))
			assertNotExists(t, statePath(root, PolicyCandidateName)+".download")
			assertNotExists(t, statePath(root, "session.json"))
		})
	}
}

func TestRunCleansRemoteCandidateOnNoChangeCommitAndUserCancel(t *testing.T) {
	policyData := []byte(`{"schema_version":1}`)
	for name, scenario := range map[string]struct {
		changed []byte
		ui      UI
	}{
		"direct commit": {changed: []byte("same"), ui: testUI{}},
		"user cancel":   {changed: []byte("new"), ui: rejectPlanUI{}},
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			writeTestFile(t, filepath.Join(root, "app.bin"), []byte("same"))
			writeTestFile(t, filepath.Join(root, PolicyFileName), policyData)
			installed := Manifest{Version: "1", Files: []FileEntry{
				entryForBytes("app.bin", []byte("same")),
				entryForBytes(PolicyFileName, policyData),
			}}
			remote := Manifest{Version: "2", Files: []FileEntry{
				entryForBytes("app.bin", scenario.changed),
				entryForBytes(PolicyFileName, policyData),
			}}
			server := updateServer(t, remote, map[string][]byte{
				"app.bin":      scenario.changed,
				PolicyFileName: policyData,
			})
			defer server.Close()
			writeInitialState(t, root, "1", installed, server.URL+"/latest.json")

			err := Run(context.Background(), Options{
				RootDir: root, AutoConfirm: true, Silent: true, UI: scenario.ui,
				ExePath: filepath.Join(root, "Updater.exe"),
			})
			if name == "direct commit" && err != nil {
				t.Fatal(err)
			}
			if name == "user cancel" && !errors.Is(err, ErrUserCancelled) {
				t.Fatalf("expected cancellation, got %v", err)
			}
			assertNotExists(t, statePath(root, PolicyCandidateName))
			assertNotExists(t, statePath(root, PolicyCandidateName)+".download")
			assertNotExists(t, statePath(root, "session.json"))
		})
	}
}

func TestRunPolicyDownloadFailureLeavesInstallAndStateUnchanged(t *testing.T) {
	root := t.TempDir()
	old := []byte("old")
	writeTestFile(t, filepath.Join(root, "app.bin"), old)
	policyData := []byte(`{"schema_version":1}`)
	installed := Manifest{Version: "1", Files: []FileEntry{entryForBytes("app.bin", old)}}
	remote := Manifest{Version: "2", Files: []FileEntry{
		entryForBytes("app.bin", []byte("new")),
		entryForBytes(PolicyFileName, policyData),
	}}
	server := updateServer(t, remote, map[string][]byte{"app.bin": []byte("new")})
	defer server.Close()
	writeInitialState(t, root, "1", installed, server.URL+"/latest.json")

	err := Run(context.Background(), Options{
		RootDir: root, AutoConfirm: true, Silent: true, UI: testUI{},
		ExePath: filepath.Join(root, "Updater.exe"),
	})
	if err == nil || PolicyFailureReason(err) != "policy_download_failed" {
		t.Fatalf("expected policy_download_failed, got %v", err)
	}
	assertFileContent(t, filepath.Join(root, "app.bin"), old)
	assertNotExists(t, statePath(root, "session.json"))
	assertNotExists(t, statePath(root, PolicyCandidateName))
}

func TestRunRecoversInterruptedSessionBeforeSelfUpdateHandoff(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "app.bin")
	backup := statePath(root, "rollback", "app.bin")
	pending := pendingSelfUpdatePath(root)
	writeTestFile(t, target, []byte("interrupted"))
	writeTestFile(t, backup, []byte("stable"))
	writeTestFile(t, pending, []byte("not an executable"))
	session := Session{
		Phase:             "Switch",
		Switched:          []MovedFile{{Path: "app.bin"}},
		BackedUp:          []MovedFile{{Path: "app.bin", BackupPath: backup}},
		PendingSelfUpdate: pending,
	}
	if err := WriteJSONAtomic(statePath(root, "session.json"), session); err != nil {
		t.Fatal(err)
	}
	if err := markSelfUpdate(root, filepath.Join(root, "Updater.exe"), pending); err != nil {
		t.Fatal(err)
	}

	err := Run(context.Background(), Options{
		RootDir: root, UI: testUI{}, ExePath: filepath.Join(root, "Updater.exe"),
	})
	if !errors.Is(err, ErrMissingConfig) {
		t.Fatalf("expected recovery followed by missing config, got %v", err)
	}
	assertFileContent(t, target, []byte("stable"))
	assertNotExists(t, statePath(root, "session.json"))
	assertNotExists(t, selfUpdateMarkerPath(root))
	assertNotExists(t, pending)
}

func TestRunStillAttemptsNormalSelfUpdateHandoffWithoutSession(t *testing.T) {
	root := t.TempDir()
	pending := pendingSelfUpdatePath(root)
	writeTestFile(t, pending, []byte("not an executable"))
	if err := markSelfUpdate(root, filepath.Join(root, "Updater.exe"), pending); err != nil {
		t.Fatal(err)
	}

	err := Run(context.Background(), Options{
		RootDir: root, UI: testUI{}, ExePath: filepath.Join(root, "Updater.exe"),
	})
	if runtime.GOOS == "windows" {
		if err == nil || !strings.Contains(err.Error(), "启动自更新辅助进程失败") {
			t.Fatalf("expected normal handoff attempt, got %v", err)
		}
		if _, ok, readErr := readSelfUpdateMarker(root); readErr != nil || !ok {
			t.Fatalf("normal handoff marker unexpectedly removed: ok=%v err=%v", ok, readErr)
		}
		assertFileContent(t, pending, []byte("not an executable"))
		return
	}
	if err != nil {
		t.Fatalf("non-Windows self-update completion failed: %v", err)
	}
	assertNotExists(t, selfUpdateMarkerPath(root))
	assertFileContent(t, filepath.Join(root, "Updater.exe"), []byte("not an executable"))
	assertFileContent(t, pending, []byte("not an executable"))
}

func TestRunDownloadsCandidateUsingManifestPathCase(t *testing.T) {
	root := t.TempDir()
	policyPath := "Updater-Policy.JSON"
	policyData := []byte(`{"schema_version":1}`)
	remote := Manifest{Version: "2", Files: []FileEntry{entryForBytes(policyPath, policyData)}}
	server := updateServer(t, remote, map[string][]byte{policyPath: policyData})
	defer server.Close()
	writeInitialState(t, root, "1", Manifest{Version: "1"}, server.URL+"/latest.json")

	if err := Run(context.Background(), Options{
		RootDir: root, AutoConfirm: true, Silent: true, UI: testUI{},
		ExePath: filepath.Join(root, "Updater.exe"),
	}); err != nil {
		t.Fatal(err)
	}
	assertFileContent(t, filepath.Join(root, policyPath), policyData)
}

func TestMigrationLatestJSONRemainsCompatibleWithStrictLegacyDecoder(t *testing.T) {
	payload, err := json.Marshal(LatestInfo{
		Version:      "2",
		ManifestURL:  "https://example.test/manifest.json",
		FilesBaseURL: "https://example.test/files/",
	})
	if err != nil {
		t.Fatal(err)
	}
	var legacy struct {
		Version      string `json:"version"`
		ManifestURL  string `json:"manifest_url"`
		FilesBaseURL string `json:"files_base_url"`
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&legacy); err != nil {
		t.Fatalf("legacy strict decoder rejected migration latest.json: %v", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		t.Fatalf("latest.json contains trailing value: %v", err)
	}
}

func TestRunRecoversSessionThenReplansWithCurrentRemotePolicy(t *testing.T) {
	root := t.TempDir()
	userChanged := []byte("user changed before interrupted update")
	writeTestFile(t, filepath.Join(root, "user.bin"), []byte("interrupted switched value"))
	backup := statePath(root, "rollback", "user.bin")
	writeTestFile(t, backup, userChanged)
	session := Session{
		Phase:    "Switch",
		Switched: []MovedFile{{Path: "user.bin"}},
		BackedUp: []MovedFile{{Path: "user.bin", BackupPath: backup}},
	}
	if err := WriteJSONAtomic(statePath(root, "session.json"), session); err != nil {
		t.Fatal(err)
	}

	oldServerValue := []byte("old server value")
	policyData := []byte(`{"schema_version":1,"preserve":{"include":{"extensions":[".bin"]}}}`)
	installed := Manifest{Version: "1", Files: []FileEntry{
		entryForBytes("user.bin", oldServerValue),
	}}
	remote := Manifest{Version: "2", Files: []FileEntry{
		entryForBytes("user.bin", []byte("new server value")),
		entryForBytes(PolicyFileName, policyData),
	}}
	server := updateServer(t, remote, map[string][]byte{
		"user.bin":     []byte("new server value"),
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
	assertFileContent(t, filepath.Join(root, "user.bin"), userChanged)
	assertFileContent(t, filepath.Join(root, PolicyFileName), policyData)
	assertNotExists(t, statePath(root, "session.json"))
}

func TestPolicyFileIsRecoveredAfterSwitchOrCommitFailure(t *testing.T) {
	t.Run("switch failure", func(t *testing.T) {
		root := t.TempDir()
		writeTestFile(t, filepath.Join(root, "z"), []byte("blocks child directory"))
		policyData := []byte(`{"schema_version":1}`)
		remote := Manifest{Version: "2", Files: []FileEntry{
			entryForBytes(PolicyFileName, policyData),
			entryForBytes("z/file.bin", []byte("payload")),
		}}
		server := updateServer(t, remote, map[string][]byte{
			PolicyFileName: policyData,
			"z/file.bin":   []byte("payload"),
		})
		defer server.Close()
		writeInitialState(t, root, "1", Manifest{Version: "1"}, server.URL+"/latest.json")

		ui := &recordingUI{}
		err := Run(context.Background(), Options{
			RootDir: root, AutoConfirm: true, Silent: true, Workers: 1, UI: ui,
			ExePath: filepath.Join(root, "Updater.exe"),
		})
		if err == nil {
			t.Fatal("expected switch failure")
		}
		sawPolicySwitch := false
		for _, event := range ui.progresses {
			if event.Phase == "Switch" && strings.EqualFold(event.CurrentFile, PolicyFileName) {
				sawPolicySwitch = true
				break
			}
		}
		if !sawPolicySwitch {
			t.Fatalf("failure occurred before policy switch: %v, events=%#v", err, ui.progresses)
		}
		assertNotExists(t, filepath.Join(root, PolicyFileName))
		assertNotExists(t, statePath(root, "session.json"))
		var version VersionState
		if err := ReadJSONFile(statePath(root, "version.json"), &version); err != nil {
			t.Fatal(err)
		}
		if version.Version != "1" {
			t.Fatalf("version changed after switch failure: %q", version.Version)
		}
	})

	t.Run("commit failure", func(t *testing.T) {
		root := t.TempDir()
		stateDir := statePath(root)
		policyData := []byte(`{"schema_version":1}`)
		staged, err := stagingPath(root, PolicyFileName)
		if err != nil {
			t.Fatal(err)
		}
		writeTestFile(t, staged, policyData)
		plan := Plan{LatestVersion: "2", Add: []FileEntry{entryForBytes(PolicyFileName, policyData)}}
		session := newSession(plan)
		if err := switchFiles(root, stateDir, plan, filepath.Join(root, "Updater.exe"), &session, testUI{}); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(stateDir, "installed_manifest.json"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := commitWithProgress(stateDir, Manifest{Version: "2", Files: plan.Add}, "2", testUI{}, false); err == nil {
			t.Fatal("expected commit failure")
		}
		if err := recoverFromSession(stateDir, root, session, testUI{}); err != nil {
			t.Fatal(err)
		}
		assertNotExists(t, filepath.Join(root, PolicyFileName))
		assertNotExists(t, statePath(root, "session.json"))
	})
}

func TestPolicyDebugEventsHaveRequiredFieldsAndAreMutuallyExclusive(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		root := t.TempDir()
		policyData := []byte(`{"schema_version":1}`)
		remote := Manifest{Version: "2", Files: []FileEntry{entryForBytes(PolicyFileName, policyData)}}
		server := updateServer(t, remote, map[string][]byte{PolicyFileName: policyData})
		defer server.Close()
		writeInitialState(t, root, "1", Manifest{Version: "1"}, server.URL+"/latest.json")

		output, runErr := captureStdout(t, func() error {
			return Run(context.Background(), Options{
				RootDir: root, AutoConfirm: true, Silent: true, Debug: true, UI: testUI{},
				ExePath: filepath.Join(root, "Updater.exe"),
			})
		})
		if runErr != nil {
			t.Fatal(runErr)
		}
		if strings.Count(output, "policy_load_success") != 1 ||
			strings.Count(output, "policy_load_failed") != 0 ||
			strings.Count(output, "policy_plan_summary") != 1 {
			t.Fatalf("unexpected event counts:\n%s", output)
		}
		for _, field := range []string{
			"source=remote", "schema_version=1", "policy_file=updater-policy.json",
			"add_count=", "modify_count=", "delete_count=",
			"preserved_modify=", "preserved_delete=",
		} {
			if !strings.Contains(output, field) {
				t.Fatalf("missing %q:\n%s", field, output)
			}
		}
	})

	t.Run("failure", func(t *testing.T) {
		root := t.TempDir()
		policyData := []byte(`{"schema_version":2}`)
		remote := Manifest{Version: "2", Files: []FileEntry{entryForBytes(PolicyFileName, policyData)}}
		server := updateServer(t, remote, map[string][]byte{PolicyFileName: policyData})
		defer server.Close()
		writeInitialState(t, root, "1", Manifest{Version: "1"}, server.URL+"/latest.json")

		output, runErr := captureStdout(t, func() error {
			return Run(context.Background(), Options{
				RootDir: root, AutoConfirm: true, Silent: true, Debug: true, UI: testUI{},
				ExePath: filepath.Join(root, "Updater.exe"),
			})
		})
		if runErr == nil {
			t.Fatal("expected invalid policy failure")
		}
		if strings.Count(output, "policy_load_failed") != 1 ||
			strings.Count(output, "policy_load_success") != 0 ||
			strings.Count(output, "policy_plan_summary") != 0 {
			t.Fatalf("unexpected event counts:\n%s", output)
		}
		for _, field := range []string{
			"source=remote", "policy_file=updater-policy.json",
			"failure_reason=policy_unsupported_schema",
		} {
			if !strings.Contains(output, field) {
				t.Fatalf("missing %q:\n%s", field, output)
			}
		}
	})
}

func captureStdout(t *testing.T, fn func() error) (string, error) {
	t.Helper()
	old := os.Stdout
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = writer
	readDone := make(chan []byte, 1)
	go func() {
		data, _ := io.ReadAll(reader)
		readDone <- data
	}()
	runErr := fn()
	_ = writer.Close()
	os.Stdout = old
	data := <-readDone
	_ = reader.Close()
	return string(data), runErr
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

func TestStagingReusesExistingFiles(t *testing.T) {
	root := t.TempDir()
	
	// 准备测试文件
	file1 := []byte("file1 content")
	file2 := []byte("file2 content")
	
	// 创建 staging 目录并预先放置 file1
	stagingDir := statePath(root, "staging")
	if err := os.MkdirAll(stagingDir, 0o755); err != nil {
		t.Fatal(err)
	}
	
	file1Entry := FileEntry{
		Path:     "file1.txt",
		Size:     int64(len(file1)),
		HeadHash: mustHash(t, file1),
		TailHash: mustHash(t, file1),
	}
	
	stagedFile1, err := stagingPath(root, file1Entry.Path)
	if err != nil {
		t.Fatal(err)
	}
	
	// 写入已存在的文件（模拟之前下载成功的文件）
	if err := os.WriteFile(stagedFile1, file1, 0o644); err != nil {
		t.Fatal(err)
	}
	
	// 创建 manifest 和服务器
	file2Entry := FileEntry{
		Path:     "file2.txt",
		Size:     int64(len(file2)),
		HeadHash: mustHash(t, file2),
		TailHash: mustHash(t, file2),
	}
	
	manifest := Manifest{
		Version: "2",
		Files:   []FileEntry{file1Entry, file2Entry},
	}
	
	files := map[string][]byte{
		"file1.txt": file1,
		"file2.txt": file2,
	}
	
	server := updateServer(t, manifest, files)
	defer server.Close()
	
	// 创建初始状态
	writeInitialState(t, root, "1", Manifest{Version: "1"}, server.URL+"/latest.json")
	
	// 记录进度事件
	ui := &recordingUI{}
	
	// 运行更新
	opts := Options{
		RootDir: root,
		Client:  server.Client(),
		Workers: 2,
		UI:      ui,
	}
	
	if err := Run(context.Background(), opts); err != nil {
		t.Fatalf("Run failed: %v", err)
	}
	
	// 验证文件已正确安装
	assertFileContent(t, filepath.Join(root, "file1.txt"), file1)
	assertFileContent(t, filepath.Join(root, "file2.txt"), file2)
	
	// 验证进度事件中包含 "(已缓存)" 标记
	hasCachedEvent := false
	for _, event := range ui.progresses {
		if strings.Contains(event.CurrentFile, "file1.txt") && strings.Contains(event.CurrentFile, "已缓存") {
			hasCachedEvent = true
			break
		}
	}
	
	if !hasCachedEvent {
		t.Errorf("expected progress event with 'file1.txt (已缓存)', got:\n%+v", ui.progresses)
	}
}

func TestStagingCleansTempFiles(t *testing.T) {
	root := t.TempDir()
	
	// 创建 staging 目录
	stagingDir := statePath(root, "staging")
	if err := os.MkdirAll(stagingDir, 0o755); err != nil {
		t.Fatal(err)
	}
	
	// 创建临时文件（模拟上次下载中断）
	tempFile := filepath.Join(stagingDir, "test.txt.download")
	if err := os.WriteFile(tempFile, []byte("temp"), 0o644); err != nil {
		t.Fatal(err)
	}
	
	// 创建不在 manifest 中的文件（模拟旧版本文件）
	orphanFile := filepath.Join(stagingDir, "old_file.txt")
	if err := os.WriteFile(orphanFile, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	
	// 准备更新
	file := []byte("test content")
	fileEntry := FileEntry{
		Path:     "test.txt",
		Size:     int64(len(file)),
		HeadHash: mustHash(t, file),
		TailHash: mustHash(t, file),
	}
	
	manifest := Manifest{
		Version: "2",
		Files:   []FileEntry{fileEntry},
	}
	
	files := map[string][]byte{
		"test.txt": file,
	}
	
	server := updateServer(t, manifest, files)
	defer server.Close()
	
	// 创建初始状态
	writeInitialState(t, root, "1", Manifest{Version: "1"}, server.URL+"/latest.json")
	
	// 运行更新
	opts := Options{
		RootDir: root,
		Client:  server.Client(),
		Workers: 2,
		UI:      testUI{},
	}
	
	if err := Run(context.Background(), opts); err != nil {
		t.Fatalf("Run failed: %v", err)
	}
	
	// 验证临时文件和孤儿文件被清理
	assertNotExists(t, tempFile)
	assertNotExists(t, orphanFile)
	
	// 验证文件已正确安装
	assertFileContent(t, filepath.Join(root, "test.txt"), file)
}

func mustHash(t *testing.T, data []byte) string {
	t.Helper()
	file, err := os.CreateTemp(t.TempDir(), "hash_*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(file.Name())
	defer file.Close()
	
	if _, err := file.Write(data); err != nil {
		t.Fatal(err)
	}
	
	size, head, tail, err := HashFile(file.Name())
	if err != nil {
		t.Fatal(err)
	}
	
	if size != int64(len(data)) {
		t.Fatalf("size mismatch: %d != %d", size, len(data))
	}
	
	if head != tail {
		t.Fatalf("head != tail for small file: %s != %s", head, tail)
	}
	
	return head
}
