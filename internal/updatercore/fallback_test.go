package updatercore

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestPublicFailureContracts(t *testing.T) {
	failure := FileFailure{
		Path:      "bin/example.wav",
		Error:     "大小或 SHA-256 采样哈希不匹配",
		Important: false,
	}
	if failure.Path == "" || failure.Error == "" {
		t.Fatal("FileFailure must retain the path and user-readable error")
	}

	var ui FileFailureUI = fileFailureUIDelegate(func(FileFailure) FileFailureDecision {
		return FileFailureDecisionSkip
	})
	if got := ui.DecideFileFailure(context.Background(), failure); got != FileFailureDecisionSkip {
		t.Fatalf("FileFailureUI decision = %v, want Skip", got)
	}

	result := StageResult{
		EffectivePlan: Plan{LatestVersion: "2026.09.01"},
		Skipped: []SkippedFile{{
			Path:  failure.Path,
			Error: failure.Error,
		}},
	}
	if len(result.Skipped) != 1 || result.EffectivePlan.LatestVersion == "" {
		t.Fatal("StageResult must expose the effective plan and skipped files")
	}
}

func TestResultClassificationUsesErrorsIsForWrappedErrors(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want ResultClass
		code int
	}{
		{name: "success", want: ResultSuccess, code: 0},
		{name: "no update", err: fmt.Errorf("check: %w", ErrNoUpdate), want: ResultNoUpdate, code: 0},
		{name: "handoff", err: fmt.Errorf("handoff: %w", ErrSelfUpdateHandoff), want: ResultSelfUpdateHandoff, code: 0},
		{name: "cancelled", err: fmt.Errorf("cancel: %w", ErrUserCancelled), want: ResultCancelled, code: 2},
		{name: "partial", err: fmt.Errorf("partial: %w", ErrUpdatePartial), want: ResultPartial, code: 3},
		{name: "failure", err: errors.New("network unavailable"), want: ResultFailure, code: 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ClassifyResult(tt.err); got != tt.want {
				t.Fatalf("ClassifyResult(%v) = %v, want %v", tt.err, got, tt.want)
			}
			if got := ExitCode(tt.err); got != tt.code {
				t.Fatalf("ExitCode(%v) = %d, want %d", tt.err, got, tt.code)
			}
		})
	}
}

func TestPartialErrorWrapsSentinelAndCarriesDirectlyOutputtableSummary(t *testing.T) {
	partial := NewUpdatePartialError([]SkippedFile{
		{Path: "data/first.json", Error: "校验失败"},
		{Path: "data/second.json", Error: "下载失败"},
	})
	wrapped := fmt.Errorf("提交部分结果失败：%w", partial)

	if !errors.Is(wrapped, ErrUpdatePartial) {
		t.Fatal("wrapped partial error must satisfy errors.Is(err, ErrUpdatePartial)")
	}
	for _, want := range []string{"data/first.json", "data/second.json", "校验失败", "下载失败"} {
		if !strings.Contains(wrapped.Error(), want) {
			t.Fatalf("wrapped partial error %q does not contain %q", wrapped.Error(), want)
		}
	}
	if got := SkippedSummary(wrapped); got == "" || !strings.Contains(got, "data/first.json") {
		t.Fatalf("SkippedSummary(%q) = %q", wrapped, got)
	}
}

func TestFallbackVisibilityUsesWrappedResultClassification(t *testing.T) {
	partial := fmt.Errorf("outer: %w", ErrUpdatePartial)
	if !ShouldShowFallback(partial, false) {
		t.Fatal("partial updates must show the complete-package fallback")
	}
	if ShouldShowFallback(fmt.Errorf("outer: %w", ErrNoUpdate), true) {
		t.Fatal("no-update results must not show the fallback")
	}
	if ShouldShowFallback(fmt.Errorf("outer: %w", ErrSelfUpdateHandoff), true) {
		t.Fatal("self-update handoff must not show the fallback")
	}
	cancelled := fmt.Errorf("outer: %w", ErrUserCancelled)
	if ShouldShowFallback(cancelled, false) {
		t.Fatal("cancellation before a file failure must not show the fallback")
	}
	if !ShouldShowFallback(cancelled, true) {
		t.Fatal("cancellation during a file failure decision must show the fallback")
	}
	if !ShouldShowFallback(errors.New("network failure"), false) {
		t.Fatal("ordinary failures must show the fallback")
	}
}

func TestFallbackNoticeIsFixedAndComplete(t *testing.T) {
	info := CompletePackageFallback()
	if info.Name != "U-META_install.rar" {
		t.Fatalf("fallback name = %q", info.Name)
	}
	if info.URL != "https://pan.baidu.com/s/13AMbh68A-_z7aCZSoQRRXA?pwd=1234" {
		t.Fatalf("fallback URL = %q", info.URL)
	}
	if info.ExtractCode != "1234" {
		t.Fatalf("fallback extract code = %q", info.ExtractCode)
	}
	if info.Suggestion != "建议下载完整安装包完成安装后，再运行更新器检查更新" {
		t.Fatalf("fallback suggestion = %q", info.Suggestion)
	}

	notice := FallbackNotice()
	for _, want := range []string{info.Name, info.URL, info.ExtractCode, info.Suggestion} {
		if !strings.Contains(notice, want) {
			t.Fatalf("fallback notice %q does not contain %q", notice, want)
		}
	}
}

type fileFailureUIDelegate func(FileFailure) FileFailureDecision

func (f fileFailureUIDelegate) DecideFileFailure(_ context.Context, failure FileFailure) FileFailureDecision {
	return f(failure)
}
