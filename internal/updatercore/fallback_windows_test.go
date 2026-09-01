//go:build windows

package updatercore

import (
	"strings"
	"testing"
)

func TestFallbackCopyPayloadUsesFixedPackageInfo(t *testing.T) {
	ui := NewDialogUI(false)
	var copied string
	ui.fallbackVisible = true
	ui.copyTextFunc = func(text string) error {
		copied = text
		return nil
	}
	ui.copyFallbackPackage()

	for _, want := range []string{FullPackageName, FullPackageURL, FullPackageExtractCode} {
		if !strings.Contains(copied, want) {
			t.Fatalf("copied payload %q does not contain %q", copied, want)
		}
	}
}

func TestFallbackOpenPassesOnlyFixedURL(t *testing.T) {
	ui := NewDialogUI(false)
	var opened string
	ui.fallbackVisible = true
	ui.openURLFunc = func(url string) error {
		opened = url
		return nil
	}
	ui.openFallbackPackage()
	if opened != FullPackageURL {
		t.Fatalf("opened URL = %q, want %q", opened, FullPackageURL)
	}
}
