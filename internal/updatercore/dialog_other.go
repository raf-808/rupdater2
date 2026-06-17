//go:build !windows

package updatercore

// DefaultUI returns the console UI on non-Windows platforms.
func DefaultUI(autoConfirm, silent bool) UI {
	return NewConsoleUI(autoConfirm, silent)
}
