package updatercore

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestFetchJSONAcceptsPayloadLargerThan32MiB(t *testing.T) {
	const payloadSize = 33 * 1024 * 1024
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":"`))
		_, _ = w.Write([]byte(strings.Repeat("x", payloadSize)))
		_, _ = w.Write([]byte(`"}`))
	}))
	defer server.Close()

	var result struct {
		Data string `json:"data"`
	}
	if err := fetchJSON(context.Background(), nil, server.URL, &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Data) != payloadSize {
		t.Fatalf("data length = %d, want %d", len(result.Data), payloadSize)
	}
}
