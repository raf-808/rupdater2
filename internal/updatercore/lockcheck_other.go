//go:build !windows

package updatercore

func FindLockedFiles(paths []string) ([]LockedFile, error) {
	return nil, nil
}

func TerminateLockedProcesses(files []LockedFile) error {
	return nil
}
