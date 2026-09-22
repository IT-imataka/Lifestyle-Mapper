package gemini

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/taka/lifestyle-mapper/backend/internal/infrastructure/llm"
	"github.com/taka/lifestyle-mapper/backend/internal/model"
)

const defaultBaseURL = "https://generativelanguage.googleapis.com/v1beta"

type Client struct {
	apiKey     string
	model      string
	timeout    time.Duration
	httpClient *http.Client
	baseURL    string
}

func New(apiKey, model string, timeout time.Duration, httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: timeout}
	}
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	return &Client{
		apiKey:     apiKey,
		model:      model,
		timeout:    timeout,
		httpClient: httpClient,
		baseURL:    defaultBaseURL,
	}
}

func (c *Client) Provider() model.LLMProvider { return model.LLMGoogle }

func (c *Client) ComposePlan(ctx context.Context, req llm.Request) (*llm.Response, error) {
	if c.apiKey == "" || c.model == "" {
		return nil, llm.Unavailable(model.LLMGoogle, fmt.Errorf("gemini credentials are not configured"))
	}
	start := time.Now()
	payload := map[string]any{
		"contents": []map[string]any{{
			"parts": []map[string]any{{"text": buildPrompt(req)}},
		}},
		"generationConfig": map[string]any{
			"responseMimeType": "application/json",
		},
	}
	if req.System != "" {
		payload["system_instruction"] = map[string]any{"parts": []map[string]any{{"text": req.System}}}
	}
	if len(req.Schema) > 0 {
		payload["generationConfig"].(map[string]any)["responseSchema"] = json.RawMessage(req.Schema)
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, llm.Unavailable(model.LLMGoogle, fmt.Errorf("gemini request encode failed: %w", err))
	}

	url := fmt.Sprintf("%s/models/%s:generateContent?key=%s", c.baseURL, c.model, c.apiKey)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, llm.Unavailable(model.LLMGoogle, fmt.Errorf("create gemini request failed: %w", err))
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, llm.Unavailable(model.LLMGoogle, fmt.Errorf("request to gemini failed: %w", err))
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, llm.Unavailable(model.LLMGoogle, fmt.Errorf("read gemini response failed: %w", err))
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, llm.Unavailable(model.LLMGoogle, fmt.Errorf("gemini responded %d: %s", resp.StatusCode, strings.TrimSpace(string(data))))
	}

	var decoded struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"content"`
		} `json:"candidates"`
		UsageMetadata struct {
			PromptTokenCount     int `json:"promptTokenCount"`
			CandidatesTokenCount int `json:"candidatesTokenCount"`
		} `json:"usageMetadata"`
	}
	if err := json.Unmarshal(data, &decoded); err != nil {
		return nil, llm.InvalidOutput(model.LLMGoogle, fmt.Errorf("gemini response is not valid JSON: %w", err))
	}
	text := extractText(decoded.Candidates)
	if text == "" {
		return nil, llm.InvalidOutput(model.LLMGoogle, fmt.Errorf("gemini response did not contain text"))
	}

	draft, err := llm.ParseDraft([]byte(text))
	if err != nil {
		return nil, llm.InvalidOutput(model.LLMGoogle, fmt.Errorf("gemini draft parse failed: %w", err))
	}
	meta := llm.NewMeta(model.LLMGoogle, c.model, time.Since(start), decoded.UsageMetadata.PromptTokenCount, decoded.UsageMetadata.CandidatesTokenCount)
	return &llm.Response{Draft: draft, Meta: meta, Raw: []byte(text)}, nil
}

func buildPrompt(req llm.Request) string {
	var b strings.Builder
	if req.System != "" {
		b.WriteString("System instructions:\n")
		b.WriteString(req.System)
		b.WriteString("\n\n")
	}
	if len(req.Schema) > 0 {
		b.WriteString("Return only a JSON object that matches the schema below.\n")
		b.WriteString("JSON Schema:\n")
		b.WriteString(string(req.Schema))
		b.WriteString("\n\n")
	}
	if len(req.Violations) > 0 {
		b.WriteString("Fix the following issues from the previous attempt:\n")
		for _, v := range req.Violations {
			b.WriteString("- ")
			b.WriteString(v)
			b.WriteString("\n")
		}
		b.WriteString("\n")
	}
	b.WriteString(req.Prompt)
	return b.String()
}

func extractText(candidates []struct {
	Content struct {
		Parts []struct {
			Text string `json:"text"`
		} `json:"parts"`
	} `json:"content"`
}) string {
	for _, c := range candidates {
		for _, part := range c.Content.Parts {
			if strings.TrimSpace(part.Text) != "" {
				return part.Text
			}
		}
	}
	return ""
}

var _ llm.Composer = (*Client)(nil)
