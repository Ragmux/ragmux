package e2e

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ragmux/ragmux/internal/testdb"
)

// TestAzureStreamThroughGatewayHasNoEmptyChoicesChunk sends a stream whose
// first event is Azure's prompt-filter chunk (no choices, empty id) through
// the whole stack. A client that never asked for usage must see a first event
// with a non-empty choices list, and no event without choices at all.
func TestAzureStreamThroughGatewayHasNoEmptyChoicesChunk(t *testing.T) {
	sse, err := os.ReadFile(filepath.Join("..", "provider", "testdata", "azure", "chat.sse"))
	if err != nil {
		t.Fatal(err)
	}
	var seenKey, seenAuth string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenKey, seenAuth = r.Header.Get("api-key"), r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write(sse)
	}))
	defer up.Close()

	e := newEnv(t, testdb.Config(t))
	conn := e.call("POST", "/admin/api/models", map[string]any{"name": "azure", "provider_type": "azure_openai",
		"base_url": up.URL, "api_key": "azure-e2e-key", "model_name": "dep"}, "")
	if conn["_status"] != float64(201) {
		t.Fatalf("create azure connection: %v", conn)
	}
	apiKey, _ := newProject(t, e, int64(conn["id"].(float64)), "azure-app", 0)

	resp, r := openStream(t, e, apiKey)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var events, withChoices int
	for line := readEvent(r); line != ""; line = readEvent(r) {
		data, ok := strings.CutPrefix(line, "data: ")
		if !ok || data == "[DONE]" {
			continue
		}
		var chunk struct {
			Choices []json.RawMessage `json:"choices"`
		}
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			t.Fatalf("bad event %q: %v", data, err)
		}
		events++
		if len(chunk.Choices) > 0 {
			withChoices++
		} else {
			t.Errorf("event %d reached the client without choices: %s", events, data)
		}
		if events == 1 && len(chunk.Choices) == 0 {
			t.Error("the first event has an empty choices list")
		}
	}
	if events == 0 || events != withChoices {
		t.Errorf("events = %d, with choices = %d", events, withChoices)
	}
	if seenKey != "azure-e2e-key" || seenAuth != "" {
		t.Errorf("upstream auth: api-key=%q authorization=%q", seenKey, seenAuth)
	}
}
