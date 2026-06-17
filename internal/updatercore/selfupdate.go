package updatercore

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"
)

type selfUpdateMarker struct {
	TargetPath  string `json:"target_path"`
	PendingPath string `json:"pending_path"`
}

func maybeHandoffSelfUpdate(rootDir string, opts Options, ui UI) error {
	if opts.SkipSelfUpdate {
		return nil
	}
	marker, ok, err := readSelfUpdateMarker(rootDir)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	if _, err := os.Stat(marker.PendingPath); err != nil {
		_ = os.Remove(selfUpdateMarkerPath(rootDir))
		return nil
	}
	if runtime.GOOS != "windows" {
		return completeSelfUpdate(rootDir, marker.TargetPath, marker.PendingPath, ui)
	}
	args := []string{"--complete-self-update", "--root", rootDir, "--self-target", marker.TargetPath, "--self-pending", marker.PendingPath}
	if opts.Silent {
		args = append(args, "--silent")
	}
	cmd := exec.Command(marker.PendingPath, args...)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("启动自更新辅助进程失败：%w", err)
	}
	ui.Info("检测到 Updater.exe 新版本，正在交接到新更新器完成自更新。")
	return ErrSelfUpdateHandoff
}

func completeSelfUpdate(rootDir, targetPath, pendingPath string, ui UI) error {
	if targetPath == "" || pendingPath == "" {
		marker, ok, err := readSelfUpdateMarker(rootDir)
		if err != nil {
			return err
		}
		if !ok {
			return nil
		}
		targetPath = marker.TargetPath
		pendingPath = marker.PendingPath
	}
	for i := 0; i < 30; i++ {
		if err := replaceFileByCopy(targetPath, pendingPath); err == nil {
			_ = os.Remove(selfUpdateMarkerPath(rootDir))
			ui.Info("Updater.exe 已更新，正在启动新版本。")
			cmd := exec.Command(targetPath, "--root", rootDir, "--skip-self-update")
			_ = cmd.Start()
			return nil
		}
		time.Sleep(300 * time.Millisecond)
	}
	return fmt.Errorf("Updater.exe 自更新失败：目标文件仍不可替换")
}

func markSelfUpdate(rootDir, targetPath, pendingPath string) error {
	return WriteJSONAtomic(selfUpdateMarkerPath(rootDir), selfUpdateMarker{
		TargetPath:  targetPath,
		PendingPath: pendingPath,
	})
}

func readSelfUpdateMarker(rootDir string) (selfUpdateMarker, bool, error) {
	var marker selfUpdateMarker
	ok, err := readOptionalJSON(selfUpdateMarkerPath(rootDir), &marker)
	return marker, ok, err
}

func selfUpdateMarkerPath(rootDir string) string {
	return statePath(rootDir, "self_update.json")
}

func isSelfUpdaterEntry(rootDir string, entry FileEntry, exePath string) bool {
	target, err := installPath(rootDir, entry.Path)
	if err != nil {
		return false
	}
	if exePath != "" && samePath(target, exePath) {
		return true
	}
	return stringsEqualFold(filepath.Base(entry.Path), "Updater.exe")
}

func pendingSelfUpdatePath(rootDir string) string {
	return statePath(rootDir, "pending", "Updater.exe")
}

func replaceFileByCopy(targetPath, pendingPath string) error {
	if err := os.MkdirAll(filepath.Dir(targetPath), 0o755); err != nil {
		return err
	}
	temp := targetPath + ".new"
	if err := copyFile(pendingPath, temp, 0o755); err != nil {
		return err
	}
	old := targetPath + ".old"
	_ = os.Remove(old)
	if err := os.Rename(targetPath, old); err != nil && !errors.Is(err, os.ErrNotExist) {
		_ = os.Remove(temp)
		return err
	}
	if err := os.Rename(temp, targetPath); err != nil {
		_ = os.Rename(old, targetPath)
		_ = os.Remove(temp)
		return err
	}
	_ = os.Remove(old)
	return nil
}

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, in)
	closeErr := out.Close()
	if copyErr != nil {
		return copyErr
	}
	return closeErr
}

func stringsEqualFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		ca, cb := a[i], b[i]
		if ca >= 'A' && ca <= 'Z' {
			ca += 'a' - 'A'
		}
		if cb >= 'A' && cb <= 'Z' {
			cb += 'a' - 'A'
		}
		if ca != cb {
			return false
		}
	}
	return true
}
