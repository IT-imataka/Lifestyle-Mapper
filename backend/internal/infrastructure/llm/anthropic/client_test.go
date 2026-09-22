package anthropic

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/taka/lifestyle-mapper/backend/internal/infrastructure/llm"
)

func TestAnthropicComposePlanParsesJSONResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("x-api-key"); got != "test-key" {
			t.Fatalf("api key header = %q, want test-key", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id": "msg_123",
			"type": "message",
			"role": "assistant",
			"content": [{"type": "text", "text": "{\"planTitle\":\"テストプラン\",\"planSummary\":\"要約\",\"vibe\":\"efficient_transit\",\"steps\":[{\"order\":1,\"kind\":\"dining\",\"candidateId\":\"p_1\",\"stayMinutes\":60,\"headline\":\"会場近くで食事\",\"reason\":\"近い\",\"tip\":\"早めに\"}],\"closingNote\":\"締め\",\"unusedCandidateNotes\":[]}"}]
			,"usage": {"input_tokens": 42, "output_tokens": 18}
		}`))
	}))
	defer server.Close()

	c := New("test-key", "claude-opus-5", 5*time.Second, server.Client())
	c.baseURL = server.URL

	resp, err := c.ComposePlan(context.Background(), llm.Request{Prompt: "test prompt", System: "system", Schema: llm.Schema(), MaxTokens: 1000})
	if err != nil {
		t.Fatalf("ComposePlan returned error: %v", err)
	}
	if resp == nil || resp.Draft == nil {
		t.Fatal("resp.Draft is nil")
	}
	if resp.Meta.Provider != "anthropic" {
		t.Fatalf("provider = %s, want anthropic", resp.Meta.Provider)
	}
}
