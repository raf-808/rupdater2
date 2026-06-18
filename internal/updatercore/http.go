package updatercore

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func defaultHTTPClient() *http.Client {
	return &http.Client{Timeout: 15 * time.Minute}
}

// http11Client 强制使用 HTTP/1.1，用于降级重试（部分服务器/CDN 在 HTTP/2 + SSL 重协商下返回 INTERNAL_ERROR）
var http11Client = &http.Client{
	Timeout: 15 * time.Minute,
	Transport: &http.Transport{
		ForceAttemptHTTP2: false,
		TLSNextProto:      make(map[string]func(string, *tls.Conn) http.RoundTripper),
	},
}

// isStreamError 判断是否为 HTTP/2 流协议层错误（INTERNAL_ERROR、stream error 等）
func isStreamError(err error) bool {
	s := err.Error()
	return strings.Contains(s, "stream error") ||
		strings.Contains(s, "INTERNAL_ERROR") ||
		strings.Contains(s, "stream ID")
}

// fetchWithFallback 先用默认客户端(HTTP/2)，遇到流错误则用 HTTP/1.1 客户端重试
func fetchWithFallback(ctx context.Context, doFunc func(client *http.Client) error) error {
	if err := doFunc(defaultHTTPClient()); err != nil && isStreamError(err) {
		return doFunc(http11Client)
	}
	return nil
}

func fetchJSON(ctx context.Context, client *http.Client, rawURL string, target any) error {
	return fetchWithFallback(ctx, func(c *http.Client) error {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
		if err != nil {
			return err
		}
		resp, err := c.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("HTTP 状态异常：%s", resp.Status)
		}
		decoder := json.NewDecoder(io.LimitReader(resp.Body, 32*1024*1024))
		decoder.DisallowUnknownFields()
		return decoder.Decode(target)
	})
}

func fileDownloadURL(baseURL, rel string) (string, error) {
	normalized, err := NormalizeManifestPath(rel)
	if err != nil {
		return "", err
	}
	segments := strings.Split(normalized, "/")
	joined, err := url.JoinPath(baseURL, segments...)
	if err != nil {
		return "", err
	}
	return joined, nil
}

func downloadAndVerify(ctx context.Context, client *http.Client, rawURL, dest string, entry FileEntry, progress func(int64)) error {
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		if err := downloadFile(ctx, client, rawURL, dest, progress); err != nil {
			lastErr = err
			continue
		}
		ok, err := VerifyFile(dest, entry)
		if err != nil {
			lastErr = err
			continue
		}
		if ok {
			return nil
		}
		lastErr = fmt.Errorf("大小或 SHA-256 采样哈希不匹配：%s", entry.Path)
		_ = os.Remove(dest)
	}
	return lastErr
}

type progressReader struct {
	r  io.Reader
	cb func(int64)
}

func (pr *progressReader) Read(p []byte) (int, error) {
	n, err := pr.r.Read(p)
	if n > 0 && pr.cb != nil {
		pr.cb(int64(n))
	}
	return n, err
}

func downloadFile(ctx context.Context, client *http.Client, rawURL, dest string, progress func(int64)) error {
	return fetchWithFallback(ctx, func(c *http.Client) error {
		return doDownload(ctx, c, rawURL, dest, progress)
	})
}

// doDownload 执行实际的文件下载（不包含降级逻辑）
func doDownload(ctx context.Context, client *http.Client, rawURL, dest string, progress func(int64)) error {
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	temp := dest + ".download"
	_ = os.Remove(temp)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP 状态异常：%s", resp.Status)
	}

	out, err := os.Create(temp)
	if err != nil {
		return err
	}
	body := &progressReader{r: resp.Body, cb: progress}
	_, copyErr := io.Copy(out, body)
	closeErr := out.Close()
	if copyErr != nil {
		_ = os.Remove(temp)
		return copyErr
	}
	if closeErr != nil {
		_ = os.Remove(temp)
		return closeErr
	}
	if err := os.Rename(temp, dest); err != nil {
		_ = os.Remove(temp)
		return err
	}
	return nil
}
