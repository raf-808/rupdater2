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
	"sync/atomic"
	"time"
)

func debugLog(enabled bool, format string, args ...any) {
	if !enabled {
		return
	}
	fmt.Fprintf(os.Stdout, "[debug] "+format+"\n", args...)
}

func Run(ctx context.Context, opts Options) error {
	rootDir, exePath, err := resolveRuntimePaths(opts)
	if err != nil {
		return err
	}
	stateDir, err := resolveStateDir(rootDir, opts.StateDir)
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

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	if setter, ok := ui.(CancelSetter); ok {
		setter.SetCancel(cancel)
	}
	if dialogUI, ok := ui.(*DialogUI); ok {
		dialogUI.Debug = opts.Debug
	}

	work := func(ctx context.Context) error {
		if opts.CompleteSelfUpdate {
			return completeSelfUpdate(rootDir, opts.SelfUpdateTarget, opts.SelfUpdatePending, ui)
		}
		if err := maybeHandoffSelfUpdate(rootDir, opts, ui); err != nil {
			return err
		}
		if err := recoverIfNeeded(stateDir, rootDir, ui); err != nil {
			return err
		}
		return runUpdate(ctx, rootDir, stateDir, exePath, opts, ui)
	}

	if runner, ok := ui.(GUIRunner); ok {
		return runner.RunMessageLoop(work, ctx)
	}
	return work(ctx)
}

func runUpdate(ctx context.Context, rootDir, stateDir, exePath string, opts Options, ui UI) error {
	version := VersionState{}
	_, err := readOptionalJSON(filepath.Join(stateDir, "version.json"), &version)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("读取本地版本失败：%w", err)
	}

	config := Config{}
	if ok, err := readOptionalJSON(filepath.Join(stateDir, "config.json"), &config); err != nil {
		return fmt.Errorf("读取配置失败：%w", err)
	} else if !ok || !validURL(config.LatestURL) {
		ui.ShowVersionInfo(version.Version, "未知")
		ui.Info("未找到可用配置，界面已启动。")
		return ErrMissingConfig
	}

	if err := ctx.Err(); err != nil {
		cleanupBeforeRootChange(stateDir)
		return ErrUserCancelled
	}

	ui.Progress(ProgressEvent{Phase: "Check"})
	latest := LatestInfo{}
	if err := fetchJSON(ctx, opts.Client, config.LatestURL, &latest); err != nil {
		return fmt.Errorf("下载 latest.json 失败：%w", err)
	}
	if latest.Version == "" || !validURL(latest.ManifestURL) || !validURL(latest.FilesBaseURL) {
		return fmt.Errorf("latest.json 缺少必要字段")
	}
	if latest.Version == version.Version {
		ui.ShowVersionInfo(version.Version, latest.Version)
		ui.Info("当前已经是最新版本。")
		return ErrNoUpdate
	}

	if err := ctx.Err(); err != nil {
		cleanupBeforeRootChange(stateDir)
		return ErrUserCancelled
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
	if ok, err := readOptionalJSON(filepath.Join(stateDir, "installed_manifest.json"), &installedManifest); err != nil {
		return fmt.Errorf("读取已安装清单失败：%w", err)
	} else if ok {
		installed = &installedManifest
	}

	if err := ctx.Err(); err != nil {
		cleanupBeforeRootChange(stateDir)
		return ErrUserCancelled
	}

	plan, err := CompareManifestsWithProgress(rootDir, installed, remoteManifest, opts.Workers, ui.Progress)
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
		return commitWithProgress(stateDir, remoteManifest, latest.Version, ui, opts.Debug)
	}
	if !ui.ConfirmPlan(plan) {
		return ErrUserCancelled
	}

	session := newSession(plan)
	if err := writeSession(stateDir, session, "Plan"); err != nil {
		return err
	}

	if err := ctx.Err(); err != nil {
		cleanupBeforeRootChange(stateDir)
		return ErrUserCancelled
	}

	if err := stageFiles(ctx, rootDir, stateDir, latest.FilesBaseURL, opts.Client, opts.Workers, plan, ui); err != nil {
		cleanupBeforeRootChange(stateDir)
		return err
	}
	if err := writeSession(stateDir, session, "OccupancyCheck"); err != nil {
		return err
	}

	if err := ctx.Err(); err != nil {
		cleanupBeforeRootChange(stateDir)
		return ErrUserCancelled
	}

	ui.Progress(ProgressEvent{Phase: "OccupancyCheck"})
	if err := occupancyCheck(rootDir, plan, exePath, ui); err != nil {
		cleanupBeforeRootChange(stateDir)
		return err
	}

	if err := ctx.Err(); err != nil {
		cleanupBeforeRootChange(stateDir)
		return ErrUserCancelled
	}

	if err := backupFiles(rootDir, stateDir, plan, exePath, &session, ui); err != nil {
		_ = recoverFromSession(stateDir, rootDir, session, ui)
		return err
	}

	if err := ctx.Err(); err != nil {
		_ = recoverFromSession(stateDir, rootDir, session, ui)
		return ErrUserCancelled
	}

	if err := switchFiles(rootDir, stateDir, plan, exePath, &session, ui); err != nil {
		_ = recoverFromSession(stateDir, rootDir, session, ui)
		return err
	}

	if err := ctx.Err(); err != nil {
		_ = recoverFromSession(stateDir, rootDir, session, ui)
		return ErrUserCancelled
	}

	ui.Progress(ProgressEvent{Phase: "Commit"})
	if err := commitWithProgress(stateDir, remoteManifest, latest.Version, ui, opts.Debug); err != nil {
		_ = recoverFromSession(stateDir, rootDir, session, ui)
		return err
	}
	cleanupAfterCommit(stateDir)
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

func resolveStateDir(rootDir, configured string) (string, error) {
	if configured != "" {
		return filepath.Abs(configured)
	}
	candidates := []string{
		filepath.Join(rootDir, StateDirName),
		filepath.Join(filepath.Dir(rootDir), StateDirName),
		filepath.Join(rootDir, "build", StateDirName),
	}
	for _, candidate := range candidates {
		if _, err := os.Stat(candidate); err == nil {
			return candidate, nil
		}
	}
	return filepath.Abs(filepath.Join(rootDir, StateDirName))
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

func writeSession(stateDir string, session Session, phase string) error {
	session.Phase = phase
	return WriteJSONAtomic(filepath.Join(stateDir, "session.json"), session)
}

func stageFiles(ctx context.Context, rootDir, stateDir, filesBaseURL string, client *http.Client, workers int, plan Plan, ui UI) error {
	entries := append([]FileEntry{}, plan.Add...)
	entries = append(entries, plan.Modify...)
	if len(entries) == 0 {
		return nil
	}
	if err := os.RemoveAll(filepath.Join(stateDir, "staging")); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(stateDir, "staging"), 0o755); err != nil {
		return err
	}

	var bytesTotal int64
	for _, entry := range entries {
		bytesTotal += entry.Size
	}

	type job struct {
		entry FileEntry
	}
	jobs := make(chan job)
	errs := make(chan error, len(entries))
	var wg sync.WaitGroup
	var progressMu sync.Mutex
	var completed int32
	var bytesDone int64
	lastProgress := time.Now()
	lastSpeedTime := time.Now()
	lastSpeedBytes := int64(0)
	speed := float64(0)

	worker := func() {
		defer wg.Done()
		for item := range jobs {
			if ctx.Err() != nil {
				return
			}
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
			progressCb := func(n int64) {
				done := atomic.AddInt64(&bytesDone, n)
				now := time.Now()
				progressMu.Lock()
				if now.Sub(lastProgress) >= 200*time.Millisecond {
					lastProgress = now
					elapsed := now.Sub(lastSpeedTime).Seconds()
					if elapsed > 0 {
						speed = float64(done-lastSpeedBytes) / elapsed
					}
					lastSpeedTime = now
					lastSpeedBytes = done
					ui.Progress(ProgressEvent{
						Phase:          "Stage",
						CurrentFile:    item.entry.Path,
						CompletedFiles: int(completed),
						TotalFiles:     len(entries),
						BytesDone:      done,
						BytesTotal:     bytesTotal,
						SpeedBytes:     speed,
					})
				}
				progressMu.Unlock()
			}
			if err := downloadAndVerify(ctx, client, rawURL, dest, item.entry, progressCb); err != nil {
				errs <- fmt.Errorf("下载失败：%s：%w", item.entry.Path, err)
				continue
			}
			progressMu.Lock()
			completed++
			ui.Progress(ProgressEvent{
				Phase:          "Stage",
				CurrentFile:    item.entry.Path,
				CompletedFiles: int(completed),
				TotalFiles:     len(entries),
				BytesDone:      atomic.LoadInt64(&bytesDone),
				BytesTotal:     bytesTotal,
				SpeedBytes:     speed,
			})
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

func backupFiles(rootDir, stateDir string, plan Plan, exePath string, session *Session, ui UI) error {
	if err := os.RemoveAll(filepath.Join(stateDir, "rollback")); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(stateDir, "rollback"), 0o755); err != nil {
		return err
	}
	if err := writeSession(stateDir, *session, "Backup"); err != nil {
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
		if err := writeSession(stateDir, *session, "Backup"); err != nil {
			return err
		}
	}
	return nil
}

func switchFiles(rootDir, stateDir string, plan Plan, exePath string, session *Session, ui UI) error {
	if err := writeSession(stateDir, *session, "Switch"); err != nil {
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
			if err := writeSession(stateDir, *session, "Switch"); err != nil {
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
		if err := writeSession(stateDir, *session, "Switch"); err != nil {
			return err
		}
	}
	return nil
}

func commit(stateDir string, manifest Manifest, version string) error {
	return commitWithProgress(stateDir, manifest, version, nil, false)
}

func commitWithProgress(stateDir string, manifest Manifest, version string, ui UI, debug bool) error {
	if ui != nil {
		ui.Progress(ProgressEvent{Phase: "Commit", CurrentFile: "正在规范化清单"})
	}
	debugLog(debug, "commit: normalize start")
	normalized, _, err := NormalizeManifest(manifest)
	if err != nil {
		return err
	}
	debugLog(debug, "commit: normalize done")
	manifestPath := filepath.Join(stateDir, "installed_manifest.json")
	versionPath := filepath.Join(stateDir, "version.json")
	oldManifest, hadManifest, err := readStateFile(manifestPath)
	if err != nil {
		return err
	}
	if ui != nil {
		ui.Progress(ProgressEvent{Phase: "Commit", CurrentFile: "正在写入 installed_manifest.json"})
	}
	debugLog(debug, "commit: write installed_manifest start")
	if err := WriteJSONAtomic(manifestPath, normalized); err != nil {
		return err
	}
	debugLog(debug, "commit: write installed_manifest done")
	if ui != nil {
		ui.Progress(ProgressEvent{Phase: "Commit", CurrentFile: "正在写入 version.json"})
	}
	debugLog(debug, "commit: write version start")
	if err := WriteJSONAtomic(versionPath, VersionState{Version: version}); err != nil {
		_ = restoreStateFile(manifestPath, oldManifest, hadManifest)
		return err
	}
	debugLog(debug, "commit: write version done")
	_ = os.Remove(filepath.Join(stateDir, "session.json"))
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

func cleanupBeforeRootChange(stateDir string) {
	_ = os.RemoveAll(filepath.Join(stateDir, "staging"))
	_ = os.Remove(filepath.Join(stateDir, "session.json"))
}

func cleanupAfterCommit(stateDir string) {
	_ = os.Remove(filepath.Join(stateDir, "session.json"))
	_ = os.RemoveAll(filepath.Join(stateDir, "staging"))
	_ = os.RemoveAll(filepath.Join(stateDir, "rollback"))
}

func recoverIfNeeded(stateDir, rootDir string, ui UI) error {
	var session Session
	ok, err := readOptionalJSON(filepath.Join(stateDir, "session.json"), &session)
	if err != nil {
		return fmt.Errorf("读取恢复状态失败：%w", err)
	}
	if !ok {
		return nil
	}
	ui.Info("检测到上次更新未完成，正在恢复到稳定状态。")
	return recoverFromSession(stateDir, rootDir, session, ui)
}

func recoverFromSession(stateDir, rootDir string, session Session, ui UI) error {
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
	_ = os.RemoveAll(filepath.Join(stateDir, "staging"))
	_ = os.RemoveAll(filepath.Join(stateDir, "rollback"))
	_ = os.Remove(filepath.Join(stateDir, "session.json"))
	return nil
}
