package updatercore

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type skipFailureTestUI struct {
	testUI
	mu         sync.Mutex
	decisions  []FileFailureDecision
	failures   []FileFailure
	decisionAt chan struct{}
}

func (ui *skipFailureTestUI) DecideFileFailure(_ context.Context, failure FileFailure) FileFailureDecision {
	ui.mu.Lock()
	ui.failures = append(ui.failures, failure)
	var decision FileFailureDecision
	if len(ui.decisions) > 0 {
		decision = ui.decisions[0]
		ui.decisions = ui.decisions[1:]
	}
	ui.mu.Unlock()
	if ui.decisionAt != nil {
		select {
		case ui.decisionAt <- struct{}{}:
		default:
		}
	}
	if decision == "" {
		return FileFailureDecisionCancel
	}
	return decision
}

func TestIsImportantFileUsesNormalizedPathAndFinalExtension(t *testing.T) {
	tests := map[string]bool{
		"bin/core.dll":           true,
		"bin/CORE.DLL":           true,
		"Updater.EXE":            true,
		"Shaders/common.usf":     true,
		`Shaders\\common.ush`:    true,
		"archive.dll.bak":        false,
		"README":                 false,
		"uploads/a.wav":          false,
		"manifest.json":          true,
		"latest.json":            true,
		"updater-policy.json":    true,
		"nested/manifest.json":   false,
		"nested/Updater.exe.bak": false,
	}
	for path, want := range tests {
		t.Run(path, func(t *testing.T) {
			if got := isImportantFile(path); got != want {
				t.Fatalf("isImportantFile(%q) = %v, want %v", path, got, want)
			}
		})
	}
}

func TestStageRetryReusesDownloadAndVerifyAndThenSucceeds(t *testing.T) {
	root := t.TempDir()
	want := []byte("valid payload after retry")
	bad := []byte("corrupt payload")
	entry := skipFailedEntry(t, "asset.dat", want)
	var requests int
	server := skipFailedPayloadServer(t, func(_ string) []byte {
		requests++
		if requests <= 2 {
			return bad
		}
		return want
	})
	defer server.Close()

	ui := &skipFailureTestUI{decisions: []FileFailureDecision{FileFailureDecisionRetry}}
	result, err := stageFiles(context.Background(), root, statePath(root), server.URL+"/files/", server.Client(), 1, Plan{
		LatestVersion: "2",
		Add:           []FileEntry{entry},
	}, ui)
	if err != nil {
		t.Fatal(err)
	}
	if len(ui.failures) != 1 || ui.failures[0].Path != entry.Path {
		t.Fatalf("failures = %#v, want one failure for %q", ui.failures, entry.Path)
	}
	if requests != 3 {
		t.Fatalf("requests = %d, want two automatic attempts before and one after retry", requests)
	}
	if len(result.Skipped) != 0 || len(result.EffectivePlan.Add) != 1 {
		t.Fatalf("stage result = %#v, want successful add", result)
	}
	assertFileContent(t, filepath.Join(root, StateDirName, "staging", entry.Path), want)
}

func TestStageRetryFailureKeepsCurrentFileBeforeAdvancing(t *testing.T) {
	root := t.TempDir()
	first := skipFailedEntry(t, "a.wav", []byte("first"))
	second := skipFailedEntry(t, "b.wav", []byte("second"))
	server := skipFailedPayloadServer(t, func(string) []byte { return []byte("wrong") })
	defer server.Close()

	ui := &skipFailureTestUI{decisions: []FileFailureDecision{
		FileFailureDecisionRetry,
		FileFailureDecisionSkip,
		FileFailureDecisionSkip,
	}}
	result, err := stageFiles(context.Background(), root, statePath(root), server.URL+"/files/", server.Client(), 2, Plan{
		Add: []FileEntry{first, second},
	}, ui)
	if err != nil {
		t.Fatal(err)
	}
	if len(ui.failures) != 3 {
		t.Fatalf("failure decisions = %#v, want retry and then one decision per file", ui.failures)
	}
	if ui.failures[0].Path != first.Path || ui.failures[1].Path != first.Path || ui.failures[2].Path != second.Path {
		t.Fatalf("failure order = %#v, want %q, %q, %q", ui.failures, first.Path, first.Path, second.Path)
	}
	if len(result.Skipped) != 2 || len(result.EffectivePlan.Add) != 0 {
		t.Fatalf("stage result = %#v, want both files skipped", result)
	}
}

func TestStageRequiredFailuresAreDecidedBeforeOrdinaryFailures(t *testing.T) {
	root := t.TempDir()
	ordinary := skipFailedEntry(t, "assets/sound.wav", []byte("ordinary"))
	required := skipFailedEntry(t, "bin/core.dll", []byte("required"))
	server := skipFailedPayloadServer(t, func(path string) []byte {
		if path == ordinary.Path || path == required.Path {
			return []byte("wrong")
		}
		return nil
	})
	defer server.Close()

	ui := &skipFailureTestUI{decisions: []FileFailureDecision{FileFailureDecisionCancel}}
	_, err := stageFiles(context.Background(), root, statePath(root), server.URL+"/files/", server.Client(), 2, Plan{
		LatestVersion: "2",
		Add:           []FileEntry{ordinary},
		Modify:        []FileEntry{required},
	}, ui)
	if !errors.Is(err, ErrUserCancelled) {
		t.Fatalf("stage error = %v, want cancellation", err)
	}
	if len(ui.failures) != 1 {
		t.Fatalf("failure decisions = %#v, want exactly one", ui.failures)
	}
	if ui.failures[0].Path != required.Path || !ui.failures[0].Important {
		t.Fatalf("first failure = %#v, want important %q", ui.failures[0], required.Path)
	}
}

func TestStageSkipAllRemovesOnlyDownloadsAndDefersDeletes(t *testing.T) {
	root := t.TempDir()
	first := skipFailedEntry(t, "a.wav", []byte("first"))
	second := skipFailedEntry(t, "b.json", []byte("second"))
	goodPayloadData := []byte("good")
	good := skipFailedEntry(t, "good.txt", goodPayloadData)
	server := skipFailedPayloadServer(t, func(path string) []byte {
		switch path {
		case first.Path, second.Path:
			return []byte("wrong")
		case good.Path:
			return goodPayloadData
		default:
			return nil
		}
	})
	defer server.Close()

	ui := &skipFailureTestUI{decisions: []FileFailureDecision{FileFailureDecisionSkipAll}}
	result, err := stageFiles(context.Background(), root, statePath(root), server.URL+"/files/", server.Client(), 3, Plan{
		LatestVersion: "2",
		Add:           []FileEntry{first, good},
		Modify:        []FileEntry{second},
		Delete:        []FileEntry{{Path: "old.bin"}},
	}, ui)
	if err != nil {
		t.Fatal(err)
	}
	if len(ui.failures) != 1 {
		t.Fatalf("failure decisions = %#v, want one decision", ui.failures)
	}
	if len(result.Skipped) != 2 || result.Skipped[0].Path != first.Path || result.Skipped[1].Path != second.Path {
		t.Fatalf("skipped = %#v, want %q then %q", result.Skipped, first.Path, second.Path)
	}
	if len(result.EffectivePlan.Add) != 1 || result.EffectivePlan.Add[0].Path != good.Path || len(result.EffectivePlan.Modify) != 0 || len(result.EffectivePlan.Delete) != 0 {
		t.Fatalf("effective plan = %#v, want only successful add and no deletes", result.EffectivePlan)
	}
}

func TestStageCancellationWhileWaitingForFailureDecisionReturns(t *testing.T) {
	root := t.TempDir()
	entry := skipFailedEntry(t, "asset.wav", []byte("expected"))
	server := skipFailedPayloadServer(t, func(string) []byte { return []byte("wrong") })
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	decisionStarted := make(chan struct{}, 1)
	ui := &blockingFailureTestUI{started: decisionStarted}
	resultCh := make(chan error, 1)
	go func() {
		_, err := stageFiles(ctx, root, statePath(root), server.URL+"/files/", server.Client(), 1, Plan{Add: []FileEntry{entry}}, ui)
		resultCh <- err
	}()
	select {
	case <-decisionStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("failure decision did not start")
	}
	cancel()
	select {
	case err := <-resultCh:
		if !errors.Is(err, ErrUserCancelled) {
			t.Fatalf("stage error = %v, want cancellation", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stage did not return after cancellation")
	}
}

type blockingFailureTestUI struct {
	testUI
	started chan<- struct{}
}

func (ui *blockingFailureTestUI) DecideFileFailure(ctx context.Context, _ FileFailure) FileFailureDecision {
	select {
	case ui.started <- struct{}{}:
	default:
	}
	<-ctx.Done()
	return FileFailureDecisionCancel
}

func TestRunRebuildsSessionWithEffectivePlanBeforeRootChanges(t *testing.T) {
	root := t.TempDir()
	oldApp := []byte("old app")
	newApp := []byte("new app")
	writeTestFile(t, filepath.Join(root, "app.bin"), oldApp)

	installed := Manifest{Version: "1", Files: []FileEntry{skipFailedEntry(t, "app.bin", oldApp)}}
	remote := Manifest{Version: "2", Files: []FileEntry{
		skipFailedEntry(t, "app.bin", newApp),
		skipFailedEntry(t, "optional.wav", []byte("optional")),
	}}
	files := map[string][]byte{
		"app.bin":      newApp,
		"optional.wav": []byte("wrong"),
	}
	server := updateServer(t, remote, files)
	defer server.Close()
	writeInitialState(t, root, "1", installed, server.URL+"/latest.json")

	ui := &skipFailedSessionObserverUI{
		skipFailureTestUI: &skipFailureTestUI{decisions: []FileFailureDecision{FileFailureDecisionSkip}},
		stateDir:          statePath(root),
	}
	if err := Run(context.Background(), Options{
		RootDir: root, AutoConfirm: true, Silent: true, UI: ui,
		ExePath: filepath.Join(root, "Updater.exe"), Workers: 2,
	}); !errors.Is(err, ErrUpdatePartial) {
		t.Fatalf("run error = %v, want partial result", err)
	}
	if !ui.sawSession {
		t.Fatal("did not observe effective session before occupancy check")
	}
	if len(ui.session.Add) != 0 || len(ui.session.Modify) != 1 || ui.session.Modify[0] != "app.bin" || len(ui.session.Delete) != 0 {
		t.Fatalf("effective session = %#v, want only app.bin modify", ui.session)
	}
}

type skipFailedSessionObserverUI struct {
	*skipFailureTestUI
	stateDir   string
	session    Session
	sawSession bool
}

func (ui *skipFailedSessionObserverUI) Progress(event ProgressEvent) {
	if event.Phase != "OccupancyCheck" {
		return
	}
	if err := ReadJSONFile(filepath.Join(ui.stateDir, "session.json"), &ui.session); err != nil {
		return
	}
	ui.sawSession = true
}

func TestRunPartialCommitPreservesOldVersionAndCompletesOnNextRun(t *testing.T) {
	root := t.TempDir()
	oldApp := []byte("old app")
	newApp := []byte("new app")
	newFile := []byte("new file")
	oldDelete := []byte("old delete")
	deletePath := "delete.bin"
	writeTestFile(t, filepath.Join(root, "app.txt"), oldApp)
	writeTestFile(t, filepath.Join(root, deletePath), oldDelete)

	installed := Manifest{Version: "1", Files: []FileEntry{
		skipFailedEntry(t, "app.txt", oldApp),
		skipFailedEntry(t, deletePath, oldDelete),
	}}
	remote := Manifest{Version: "2", Files: []FileEntry{
		skipFailedEntry(t, "app.txt", newApp),
		skipFailedEntry(t, "new.txt", newFile),
	}}
	files := map[string][]byte{
		"app.txt": []byte("bad app"),
		"new.txt": newFile,
	}
	server := updateServer(t, remote, files)
	defer server.Close()
	writeInitialState(t, root, "1", installed, server.URL+"/latest.json")

	ui := &skipFailureTestUI{decisions: []FileFailureDecision{FileFailureDecisionSkip}}
	err := Run(context.Background(), Options{
		RootDir: root, AutoConfirm: true, Silent: true, UI: ui,
		ExePath: filepath.Join(root, "Updater.exe"), Workers: 2,
	})
	if !errors.Is(err, ErrUpdatePartial) {
		t.Fatalf("first run error = %v, want partial result", err)
	}
	var partialErr *UpdatePartialError
	if !errors.As(err, &partialErr) || len(partialErr.Skipped) != 1 || !strings.Contains(err.Error(), "app.txt") {
		t.Fatalf("partial error = %v, want skipped summary for app.txt", err)
	}
	assertFileContent(t, filepath.Join(root, "app.txt"), oldApp)
	assertFileContent(t, filepath.Join(root, "new.txt"), newFile)
	assertFileContent(t, filepath.Join(root, deletePath), oldDelete)
	var version VersionState
	if err := ReadJSONFile(statePath(root, "version.json"), &version); err != nil {
		t.Fatal(err)
	}
	if version.Version != "1" {
		t.Fatalf("version after partial = %q, want 1", version.Version)
	}
	var partial Manifest
	if err := ReadJSONFile(statePath(root, "installed_manifest.json"), &partial); err != nil {
		t.Fatal(err)
	}
	if partial.Version != "1" || !skipFailedManifestHasPath(partial, "app.txt") || !skipFailedManifestHasPath(partial, "new.txt") || !skipFailedManifestHasPath(partial, deletePath) {
		t.Fatalf("partial installed manifest = %#v", partial)
	}
	assertNotExists(t, statePath(root, "session.json"))
	assertNotExists(t, statePath(root, "staging"))
	assertNotExists(t, statePath(root, "rollback"))

	files["app.txt"] = newApp
	if err := Run(context.Background(), Options{
		RootDir: root, AutoConfirm: true, Silent: true, UI: testUI{},
		ExePath: filepath.Join(root, "Updater.exe"), Workers: 2,
	}); err != nil {
		t.Fatalf("second run error = %v", err)
	}
	assertFileContent(t, filepath.Join(root, "app.txt"), newApp)
	assertFileContent(t, filepath.Join(root, "new.txt"), newFile)
	assertNotExists(t, filepath.Join(root, deletePath))
	if err := ReadJSONFile(statePath(root, "version.json"), &version); err != nil {
		t.Fatal(err)
	}
	if version.Version != "2" {
		t.Fatalf("version after second run = %q, want 2", version.Version)
	}
	var complete Manifest
	if err := ReadJSONFile(statePath(root, "installed_manifest.json"), &complete); err != nil {
		t.Fatal(err)
	}
	if complete.Version != remote.Version || !skipFailedManifestHasPath(complete, "app.txt") || !skipFailedManifestHasPath(complete, "new.txt") || skipFailedManifestHasPath(complete, deletePath) {
		t.Fatalf("complete installed manifest = %#v", complete)
	}
}

func TestRunPartialFirstInstallRecordsAlreadyValidFiles(t *testing.T) {
	root := t.TempDir()
	already := []byte("already present")
	badExpected := []byte("download later")
	writeTestFile(t, filepath.Join(root, "already.txt"), already)

	remote := Manifest{Version: "2", Files: []FileEntry{
		skipFailedEntry(t, "already.txt", already),
		skipFailedEntry(t, "missing.wav", badExpected),
	}}
	files := map[string][]byte{
		"already.txt": already,
		"missing.wav": []byte("corrupt"),
	}
	server := updateServer(t, remote, files)
	defer server.Close()
	writeInitialState(t, root, "1", Manifest{Version: "1"}, server.URL+"/latest.json")
	if err := os.Remove(statePath(root, "installed_manifest.json")); err != nil {
		t.Fatal(err)
	}

	ui := &skipFailureTestUI{decisions: []FileFailureDecision{FileFailureDecisionSkip}}
	err := Run(context.Background(), Options{
		RootDir: root, AutoConfirm: true, Silent: true, UI: ui,
		ExePath: filepath.Join(root, "Updater.exe"), Workers: 2,
	})
	if !errors.Is(err, ErrUpdatePartial) {
		t.Fatalf("first-install error = %v, want partial result", err)
	}
	var partial Manifest
	if err := ReadJSONFile(statePath(root, "installed_manifest.json"), &partial); err != nil {
		t.Fatal(err)
	}
	if !skipFailedManifestHasPath(partial, "already.txt") || skipFailedManifestHasPath(partial, "missing.wav") {
		t.Fatalf("first-install partial manifest = %#v", partial)
	}
	assertFileContent(t, filepath.Join(root, "already.txt"), already)
	if _, err := os.Stat(filepath.Join(root, "missing.wav")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing.wav stat = %v, want absent", err)
	}
}

func skipFailedEntry(t *testing.T, path string, data []byte) FileEntry {
	t.Helper()
	file := filepath.Join(t.TempDir(), "payload")
	if err := os.WriteFile(file, data, 0o644); err != nil {
		t.Fatal(err)
	}
	size, head, tail, err := HashFile(file)
	if err != nil {
		t.Fatal(err)
	}
	return FileEntry{Path: path, Size: size, HeadHash: head, TailHash: tail}
}

func skipFailedPayloadServer(t *testing.T, payload func(string) []byte) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/files/", func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/files/")
		data := payload(path)
		if data == nil {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(data)
	})
	return httptest.NewServer(mux)
}

func skipFailedManifestHasPath(manifest Manifest, path string) bool {
	for _, entry := range manifest.Files {
		if strings.EqualFold(entry.Path, path) {
			return true
		}
	}
	return false
}
