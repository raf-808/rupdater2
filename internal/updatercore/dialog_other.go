//go:build !windows

package updatercore

type DialogUI struct {
	*ConsoleUI
}

func DefaultUI(autoConfirm, silent bool) UI {
	return NewConsoleUI(autoConfirm, silent)
}
