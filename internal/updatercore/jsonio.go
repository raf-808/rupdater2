package updatercore

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)

func ReadJSONFile(fileName string, target any) error {
	data, err := os.ReadFile(fileName)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, target)
}

func WriteJSONAtomic(fileName string, value any) error {
	if err := os.MkdirAll(filepath.Dir(fileName), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	temp := fileName + ".tmp"
	if err := os.WriteFile(temp, data, 0o644); err != nil {
		return err
	}
	return replaceWithBackup(temp, fileName)
}

func readOptionalJSON(fileName string, target any) (bool, error) {
	err := ReadJSONFile(fileName, target)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}

func replaceWithBackup(temp, target string) error {
	backup := target + ".bak"
	_ = os.Remove(backup)
	hadTarget := false
	if _, err := os.Stat(target); err == nil {
		hadTarget = true
		if err := os.Rename(target, backup); err != nil {
			_ = os.Remove(temp)
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		_ = os.Remove(temp)
		return err
	}
	if err := os.Rename(temp, target); err != nil {
		if hadTarget {
			_ = os.Rename(backup, target)
		}
		_ = os.Remove(temp)
		return err
	}
	_ = os.Remove(backup)
	return nil
}
