package updatercore

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"time"
)

func Run(ctx context.Context, opts Options) error {
	rootDir, exePath, err := resolveRuntimePaths(opts)
	if err != nil {
		return err
	}
	ui := opts.UI
	if ui == nil {
		ui = DefaultUI(opts.AutoConfirm, opts.Silent)
	}
	if opts.Workers <= 0 {
		opts.Workers = 4
	}
	if opts.Client == nil {
		opts.Client = defaultHTTPClient()
	}

	if opts.CompleteSelfUpdate {
		return completeSelfUpdate(rootDir, opts.SelfUpdateTarget, opts.SelfUpdatePending, ui)
	}
	if err := maybeHandoffSelfUpdate(rootDir, opts, ui); err != nil {
		return err
	}
	if err := recoverIfNeeded(rootDir, ui); err != nil {
		return err
	}

	version := VersionState{}
	_, err = readOptionalJSON(statePath(rootDir, "version.json"), &version)
	if err != nil {
		return fmt.Errorf("读取本地版本失败：%w", err)
	}

	config := Config{}
	if ok, err := readOptionalJSON(statePath(rootDir, "config.json"), &config); err != nil {
		return fmt.Errorf("读取配置失败：%w", err)
	} else if !ok || !validURL(config.LatestURL) {
		return fmt.Errorf("配置缺失或非法：%s", statePath(rootDir, "config.json"))
	}

	latest := LatestInfo{}
	if err := fetchJSON(ctx, opts.Client, config.LatestURL, &latest); err != nil {
		return fmt.Errorf("下载 latest.json 失败：%w", err)
	}
	if latest.Version == "" || !validURL(latest.ManifestURL) || !validURL(latest.FilesBaseURL) {
		return fmt.Errorf("latest.json 缺少必要字段")
	}
	if latest.Version == version.Version {
		ui.Info("当前已经是最新版本。")
		return ErrNoUpdate
	}

	remoteManifest := Manifest{}
	if err := fetchJSON(ctx, opts.Client, latest.ManifestURL, &remoteManifest); err != nil {
		return fmt.Errorf("下载 manifest.json 失败：%w", err)
	}
	if remoteManifest.Version != latest.Version {
		return fmt.Errorf("latest.json 版本与 manifest.json 版本不一致：%s != %s", latest.Version, remoteManifest.Version)
	}

	var installed *Manifest
	installedManifest := Manifest{}
	if ok, err := readOptionalJSON(statePath(rootDir, "installed_manifest.json"), &installedManifest); err != nil {
		return fmt.Errorf("读取已安装清单失败：%w", err)
	} else if ok {
		installed = &installedManifest
	}

	plan, err := CompareManifests(rootDir, installed, remoteManifest)
	if err != nil {
		return err
	}
	plan.CurrentVersion = version.Version
	plan.LatestVersion = latest.Version
	for _, entry := range plan.Delete {
		if isSelfUpdaterEntry(rootDir, entry, exePath) {
			return fmt.Errorf("远端清单不能删除当前 Updater.exe")
		}
	}
	if len(plan.Add) == 0 && len(plan.Modify) == 0 && len(plan.Delete) == 0 {
		ui.Info("文件已与最新清单一致，正在提交版本状态。")
		return commit(rootDir, remoteManifest, latest.Version)
	}
	if !ui.ConfirmPlan(plan) {
		return ErrUserCancelled
	}

	session := newSession(plan)
	if err := writeSession(rootDir, session, "Plan"); err != nil {
		return err
	}
	if err := stageFiles(ctx, rootDir, latest.FilesBaseURL, opts.Client, opts.Workers, plan, ui); err != nil {
		cleanupBeforeRootChange(rootDir)
		return err
	}
	if err := writeSession(rootDir, session, "OccupancyCheck"); err != nil {
		return err
	}
	if err := occupancyCheck(rootDir, plan, exePath, ui); err != nil {
		cleanupBeforeRootChange(rootDir)
		return err
	}
	if err := backupFiles(rootDir, plan, exePath, &session, ui); err != nil {
		_ = recoverFromSession(rootDir, session, ui)
		return err
	}
	if err := switchFiles(rootDir, plan, exePath, &session, ui); err != nil {
		_ = recoverFromSession(rootDir, session, ui)
		return err
	}
	if err := commit(rootDir, remoteManifest, latest.Version); err != nil {
		_ = recoverFromSession(rootDir, session, ui)
		return err
	}
	cleanupAfterCommit(rootDir)
	ui.Info("更新完成。")
	return nil
}

func resolveRuntimePaths(opts Options) (string, string, error) {
	exePath := opts.ExePath
	if exePath == "" {
		var err error
		exePath, err = os.Executable()
		if err != nil {
			return "", "", err
		}
	}
	exePath, err := filepath.Abs(exePath)
	if err != nil {
		return "", "", err
	}
	rootDir := opts.RootDir
	if rootDir == "" {
		rootDir = filepath.Dir(exePath)
	}
	rootDir, err = filepath.Abs(rootDir)
	if err != nil {
		return "", "", err
	}
	return rootDir, exePath, nil
}

func validURL(raw string) bool {
	parsed, err := url.Parse(raw)
	return err == nil && parsed.Scheme != "" && parsed.Host != ""
}

func newSession(plan Plan) Session {
	session := Session{
		TargetVersion: plan.LatestVersion,
		Add:           pathsOf(plan.Add),
		Modify:        pathsOf(plan.Modify),
		Delete:        pathsOf(plan.Delete),
		StartedAt:     time.Now().Format(time.RFC3339),
	}
	return session
}

func pathsOf(entries []FileEntry) []string {
	paths := make([]string, 0, len(entries))
	for _, entry := range entries {
		paths = append(paths, entry.Path)
	}
	return paths
}

func writeSession(rootDir string, session Session, phase string) error {
	session.Phase = phase
	return WriteJSONAtomic(statePath(rootDir, "session.json"), session)
}

func stageFiles(ctx context.Context, rootDir, filesBaseURL string, client *http.Client, workers int, plan Plan, ui UI) error {
	entries := append([]FileEntry{}, plan.Add...)
	entries = append(entries, plan.Modify...)
	if len(entries) == 0 {
		return nil
	}
	if err := os.RemoveAll(statePath(rootDir, "staging")); err != nil {
		return err
	}
	if err := os.MkdirAll(statePath(rootDir, "staging"), 0o755); err != nil {
		return err
	}

	type job struct {
		entry FileEntry
	}
	jobs := make(chan job)
	errs := make(chan error, len(entries))
	var wg sync.WaitGroup
	var progressMu sync.Mutex
	completed := 0

	worker := func() {
		defer wg.Done()
		for item := range jobs {
			dest, err := stagingPath(rootDir, item.entry.Path)
			if err != nil {
				errs <- err
				continue
			}
			rawURL, err := fileDownloadURL(filesBaseURL, item.entry.Path)
			if err != nil {
				errs <- err
				continue
			}
			if err := downloadAndVerify(ctx, client, rawURL, dest, item.entry); err != nil {
				errs <- fmt.Errorf("下载失败：%s：%w", item.entry.Path, err)
				continue
			}
			progressMu.Lock()
			completed++
			ui.Progress(ProgressEvent{Phase: "Stage", CurrentFile: item.entry.Path, CompletedFiles: completed, TotalFiles: len(entries)})
			progressMu.Unlock()
		}
	}

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go worker()
	}
	for _, entry := range entries {
		jobs <- job{entry: entry}
	}
	close(jobs)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

func occupancyCheck(rootDir string, plan Plan, exePath string, ui UI) error {
	var targets []string
	for _, entry := range append(append([]FileEntry{}, plan.Modify...), plan.Delete...) {
		if isSelfUpdaterEntry(rootDir, entry, exePath) {
			continue
		}
		target, err := installPath(rootDir, entry.Path)
		if err != nil {
			return err
		}
		if _, err := os.Stat(target); errors.Is(err, os.ErrNotExist) {
			continue
		}
		targets = append(targets, target)
	}
	locked, err := FindLockedFiles(targets)
	if err != nil {
		return err
	}
	if len(locked) == 0 {
		return nil
	}
	if !ui.ConfirmProcessTermination(locked) {
		return ErrUserCancelled
	}
	if err := TerminateLockedProcesses(locked); err != nil {
		return err
	}
	locked, err = FindLockedFiles(targets)
	if err != nil {
		return err
	}
	if len(locked) > 0 {
		return fmt.Errorf("文件仍然被占用，更新已中止")
	}
	return nil
}

func backupFiles(rootDir string, plan Plan, exePath string, session *Session, ui UI) error {
	if err := os.RemoveAll(statePath(rootDir, "rollback")); err != nil {
		return err
	}
	if err := os.MkdirAll(statePath(rootDir, "rollback"), 0o755); err != nil {
		return err
	}
	if err := writeSession(rootDir, *session, "Backup"); err != nil {
		return err
	}
	entries := append([]FileEntry{}, plan.Modify...)
	entries = append(entries, plan.Delete...)
	for _, entry := range entries {
		if isSelfUpdaterEntry(rootDir, entry, exePath) {
			continue
		}
		source, err := installPath(rootDir, entry.Path)
		if err != nil {
			return err
		}
		if _, err := os.Stat(source); errors.Is(err, os.ErrNotExist) {
			continue
		}
		backup, err := rollbackPath(rootDir, entry.Path)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(backup), 0o755); err != nil {
			return err
		}
		ui.Progress(ProgressEvent{Phase: "Backup", CurrentFile: entry.Path})
		if err := os.Rename(source, backup); err != nil {
			return fmt.Errorf("备份失败：%s：%w", entry.Path, err)
		}
		session.BackedUp = append(session.BackedUp, MovedFile{Path: entry.Path, BackupPath: backup})
		if err := writeSession(rootDir, *session, "Backup"); err != nil {
			return err
		}
	}
	return nil
}

func switchFiles(rootDir string, plan Plan, exePath string, session *Session, ui UI) error {
	if err := writeSession(rootDir, *session, "Switch"); err != nil {
		return err
	}
	entries := append([]FileEntry{}, plan.Add...)
	entries = append(entries, plan.Modify...)
	for _, entry := range entries {
		staged, err := stagingPath(rootDir, entry.Path)
		if err != nil {
			return err
		}
		if isSelfUpdaterEntry(rootDir, entry, exePath) {
			pending := pendingSelfUpdatePath(rootDir)
			if err := os.MkdirAll(filepath.Dir(pending), 0o755); err != nil {
				return err
			}
			_ = os.Remove(pending)
			if err := os.Rename(staged, pending); err != nil {
				return fmt.Errorf("保存 Updater.exe 自更新文件失败：%w", err)
			}
			target, err := installPath(rootDir, entry.Path)
			if err != nil {
				return err
			}
			if err := markSelfUpdate(rootDir, target, pending); err != nil {
				return err
			}
			session.PendingSelfUpdate = pending
			if err := writeSession(rootDir, *session, "Switch"); err != nil {
				return err
			}
			continue
		}
		target, err := installPath(rootDir, entry.Path)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		ui.Progress(ProgressEvent{Phase: "Switch", CurrentFile: entry.Path})
		_ = os.Remove(target)
		if err := os.Rename(staged, target); err != nil {
			return fmt.Errorf("切换失败：%s：%w", entry.Path, err)
		}
		session.Switched = append(session.Switched, MovedFile{Path: entry.Path})
		if err := writeSession(rootDir, *session, "Switch"); err != nil {
			return err
		}
	}
	return nil
}

func commit(rootDir string, manifest Manifest, version string) error {
	normalized, _, err := NormalizeManifest(manifest)
	if err != nil {
		return err
	}
	manifestPath := statePath(rootDir, "installed_manifest.json")
	versionPath := statePath(rootDir, "version.json")
	oldManifest, hadManifest, err := readStateFile(manifestPath)
	if err != nil {
		return err
	}
	if err := WriteJSONAtomic(manifestPath, normalized); err != nil {
		return err
	}
	if err := WriteJSONAtomic(versionPath, VersionState{Version: version}); err != nil {
		_ = restoreStateFile(manifestPath, oldManifest, hadManifest)
		return err
	}
	_ = os.Remove(statePath(rootDir, "session.json"))
	return nil
}

func readStateFile(fileName string) ([]byte, bool, error) {
	data, err := os.ReadFile(fileName)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	return data, err == nil, err
}

func restoreStateFile(fileName string, data []byte, existed bool) error {
	if !existed {
		return os.Remove(fileName)
	}
	if err := os.MkdirAll(filepath.Dir(fileName), 0o755); err != nil {
		return err
	}
	return os.WriteFile(fileName, data, 0o644)
}

func cleanupBeforeRootChange(rootDir string) {
	_ = os.RemoveAll(statePath(rootDir, "staging"))
	_ = os.Remove(statePath(rootDir, "session.json"))
}

func cleanupAfterCommit(rootDir string) {
	_ = os.Remove(statePath(rootDir, "session.json"))
	_ = os.RemoveAll(statePath(rootDir, "staging"))
	_ = os.RemoveAll(statePath(rootDir, "rollback"))
}

func recoverIfNeeded(rootDir string, ui UI) error {
	var session Session
	ok, err := readOptionalJSON(statePath(rootDir, "session.json"), &session)
	if err != nil {
		return fmt.Errorf("读取恢复状态失败：%w", err)
	}
	if !ok {
		return nil
	}
	ui.Info("检测到上次更新未完成，正在恢复到稳定状态。")
	return recoverFromSession(rootDir, session, ui)
}

func recoverFromSession(rootDir string, session Session, ui UI) error {
	for i := len(session.Switched) - 1; i >= 0; i-- {
		item := session.Switched[i]
		target, err := installPath(rootDir, item.Path)
		if err != nil {
			return err
		}
		ui.Progress(ProgressEvent{Phase: "Recover", CurrentFile: item.Path})
		_ = os.Remove(target)
	}
	for i := len(session.BackedUp) - 1; i >= 0; i-- {
		item := session.BackedUp[i]
		target, err := installPath(rootDir, item.Path)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		ui.Progress(ProgressEvent{Phase: "Recover", CurrentFile: item.Path})
		if _, err := os.Stat(item.BackupPath); err == nil {
			_ = os.Remove(target)
			if err := os.Rename(item.BackupPath, target); err != nil {
				return fmt.Errorf("恢复失败：%s：%w", item.Path, err)
			}
		}
	}
	if session.PendingSelfUpdate != "" {
		_ = os.Remove(session.PendingSelfUpdate)
		_ = os.Remove(selfUpdateMarkerPath(rootDir))
	}
	_ = os.RemoveAll(statePath(rootDir, "staging"))
	_ = os.RemoveAll(statePath(rootDir, "rollback"))
	_ = os.Remove(statePath(rootDir, "session.json"))
	return nil
}
