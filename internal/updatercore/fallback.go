package updatercore

import (
	"errors"
	"fmt"
	"io"
	"strings"
)

const (
	// FullPackageName, FullPackageURL, and FullPackageExtractCode are fixed
	// recovery-package details. They intentionally are not configuration.
	FullPackageName        = "U-META_install.rar"
	FullPackageURL         = "https://pan.baidu.com/s/13AMbh68A-_z7aCZSoQRRXA?pwd=1234"
	FullPackageExtractCode = "1234"
	FullPackageSuggestion  = "建议下载完整安装包完成安装后，再运行更新器检查更新"
)

// FallbackInfo is the fixed complete-package information shown after an
// update failure or partial update.
type FallbackInfo struct {
	Name        string
	URL         string
	ExtractCode string
	Suggestion  string
}

// CompletePackageFallback returns the official, fixed complete-package
// information.
func CompletePackageFallback() FallbackInfo {
	return FallbackInfo{
		Name:        FullPackageName,
		URL:         FullPackageURL,
		ExtractCode: FullPackageExtractCode,
		Suggestion:  FullPackageSuggestion,
	}
}

// FallbackNotice renders all fixed complete-package details for a human
// readable stderr or UI message.
func FallbackNotice() string {
	info := CompletePackageFallback()
	return fmt.Sprintf("官方完整安装包：%s\n下载地址：%s\n提取码：%s\n%s",
		info.Name, info.URL, info.ExtractCode, info.Suggestion)
}

// WriteFallbackNotice writes the fixed complete-package notice without
// relying on any UI's transient state.
func WriteFallbackNotice(w io.Writer) {
	fmt.Fprintln(w, FallbackNotice())
}

// UpdatePartialError carries the skipped-file summary while remaining
// identifiable as ErrUpdatePartial through any outer %w wrapper.
type UpdatePartialError struct {
	Skipped []SkippedFile
	Cause   error
}

// PartialUpdateError is an alternate descriptive name for UpdatePartialError.
type PartialUpdateError = UpdatePartialError

// NewUpdatePartialError creates a partial-update error. An optional cause is
// included in the directly printable error text for state-write failures.
func NewUpdatePartialError(skipped []SkippedFile, cause ...error) *UpdatePartialError {
	copied := append([]SkippedFile(nil), skipped...)
	var err error
	if len(cause) > 0 {
		err = cause[0]
	}
	return &UpdatePartialError{Skipped: copied, Cause: err}
}

// NewPartialUpdateError is an alternate constructor name for callers that
// describe the result as a partial update.
func NewPartialUpdateError(skipped []SkippedFile, cause ...error) *UpdatePartialError {
	return NewUpdatePartialError(skipped, cause...)
}

func (e *UpdatePartialError) Error() string {
	if e == nil {
		return ErrUpdatePartial.Error()
	}
	message := FormatSkippedSummary(e.Skipped)
	if message == "" {
		message = "更新部分完成：存在未完成的文件"
	}
	if e.Cause != nil {
		message += fmt.Sprintf("；状态写入错误：%v", e.Cause)
	}
	return message
}

func (e *UpdatePartialError) Unwrap() error {
	return ErrUpdatePartial
}

// FormatSkippedSummary returns a directly printable summary of skipped files.
func FormatSkippedSummary(skipped []SkippedFile) string {
	if len(skipped) == 0 {
		return ""
	}

	var builder strings.Builder
	fmt.Fprintf(&builder, "更新部分完成：有 %d 个文件被跳过，下次检查将继续重试", len(skipped))
	for _, file := range skipped {
		if strings.TrimSpace(file.Error) == "" {
			fmt.Fprintf(&builder, "\n- %s", file.Path)
			continue
		}
		fmt.Fprintf(&builder, "\n- %s：%s", file.Path, file.Error)
	}
	return builder.String()
}

// SkippedSummary extracts a skipped-file summary through arbitrary %w
// wrappers. It returns an empty string when the error carries no summary.
func SkippedSummary(err error) string {
	var partial *UpdatePartialError
	if err == nil || !errors.As(err, &partial) || partial == nil {
		return ""
	}
	return FormatSkippedSummary(partial.Skipped)
}

// ResultClass is the process-visible classification of a Run result.
type ResultClass uint8

const (
	ResultSuccess ResultClass = iota
	ResultNoUpdate
	ResultPartial
	ResultCancelled
	ResultSelfUpdateHandoff
	ResultFailure
)

// ClassifyResult classifies errors through errors.Is so wrapped sentinels keep
// their process semantics.
func ClassifyResult(err error) ResultClass {
	switch {
	case err == nil:
		return ResultSuccess
	case errors.Is(err, ErrUpdatePartial):
		return ResultPartial
	case errors.Is(err, ErrNoUpdate):
		return ResultNoUpdate
	case errors.Is(err, ErrSelfUpdateHandoff):
		return ResultSelfUpdateHandoff
	case errors.Is(err, ErrUserCancelled):
		return ResultCancelled
	default:
		return ResultFailure
	}
}

// ExitCode maps a Run result to the Updater process contract: 0 for success,
// no update, or self-update handoff; 1 for an update failure; 2 for user
// cancellation; and 3 for a partial update.
func ExitCode(err error) int {
	switch ClassifyResult(err) {
	case ResultSuccess, ResultNoUpdate, ResultSelfUpdateHandoff:
		return 0
	case ResultCancelled:
		return 2
	case ResultPartial:
		return 3
	default:
		return 1
	}
}

// ShouldShowFallback reports whether the fixed complete package should be
// shown. A user cancellation shows it only when a file-failure decision had
// already been entered by the current UI run.
func ShouldShowFallback(err error, hadFileFailure bool) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, ErrUpdatePartial):
		return true
	case errors.Is(err, ErrNoUpdate), errors.Is(err, ErrSelfUpdateHandoff):
		return false
	case errors.Is(err, ErrUserCancelled):
		return hadFileFailure
	default:
		return true
	}
}
