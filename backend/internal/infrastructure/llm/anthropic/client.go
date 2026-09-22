package anthropic

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/taka/lifestyle-mapper/backend/internal/apperror"
	"github.com/taka/lifestyle-mapper/backend/internal/infrastructure/llm"
	"github.com/taka/lifestyle-mapper/backend/internal/model"
)

const defaultBaseURL = "https://api.anthropic.com/v1"

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

func (c *Client) Provider() model.LLMProvider { return model.LLMAnthropic }

func (c *Client) ComposePlan(ctx context.Context, req llm.Request) (*llm.Response, error) {
	if c.apiKey == "" || c.model == "" {
		return nil, llm.Unavailable(model.LLMAnthropic, fmt.Errorf("anthropic credentials are not configured"))
	}
	start := time.Now()
	payload := map[string]any{
		"model":      c.model,
		"max_tokens": req.MaxTokens,
		"system":     req.System,
		"messages": []map[string]any{{
			"role": "user",
			"content": []map[string]any{{
				"type": "text",
				"text": buildPrompt(req),
			}},
		}},
	}
	if req.MaxTokens <= 0 {
		payload["max_tokens"] = 4000
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, llm.Unavailable(model.LLMAnthropic, fmt.Errorf("anthropic request encode failed: %w", err))
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/messages", bytes.NewReader(body))
	if err != nil {
		return nil, llm.Unavailable(model.LLMAnthropic, fmt.Errorf("create anthropic request failed: %w", err))
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-api-key", c.apiKey)
	httpReq.Header.Set("anthropic-version", "2023-06-01")

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, llm.Unavailable(model.LLMAnthropic, fmt.Errorf("request to anthropic failed: %w", err))
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, llm.Unavailable(model.LLMAnthropic, fmt.Errorf("read anthropic response failed: %w", err))
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, llm.Unavailable(model.LLMAnthropic, fmt.Errorf("anthropic responded %d: %s", resp.StatusCode, strings.TrimSpace(string(data))))
	}

	var decoded struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		Usage struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(data, &decoded); err != nil {
		return nil, llm.InvalidOutput(model.LLMAnthropic, fmt.Errorf("anthropic response is not valid JSON: %w", err))
	}
	text := extractText(decoded.Content)
	if text == "" {
		return nil, llm.InvalidOutput(model.LLMAnthropic, fmt.Errorf("anthropic response did not contain a text block"))
	}

	draft, err := llm.ParseDraft([]byte(text))
	if err != nil {
		return nil, llm.InvalidOutput(model.LLMAnthropic, fmt.Errorf("anthropic draft parse failed: %w", err))
	}
	meta := llm.NewMeta(model.LLMAnthropic, c.model, time.Since(start), decoded.Usage.InputTokens, decoded.Usage.OutputTokens)
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

func extractText(content []struct {
	Type string `json:"type"`
	Text string `json:"text"`
}) string {
	for _, item := range content {
		if item.Type == "text" && strings.TrimSpace(item.Text) != "" {
			return item.Text
		}
	}
	return ""
}

var _ llm.Composer = (*Client)(nil)
var _ = apperror.Wrap
