package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
)

const openRouterAPIURL = "https://openrouter.ai/api/v1/chat/completions"

// Task execution supplies the deadline and cancellation context. Keep this
// shared client free of a second timeout so TASK_TIMEOUT is authoritative.
var openRouterHTTPClient = &http.Client{}

// ── request / response shapes ─────────────────────────────────────────────────

type orMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type orRequest struct {
	Model          string          `json:"model"`
	Messages       []orMessage     `json:"messages"`
	Temperature    *float64        `json:"temperature,omitempty"`
	MaxTokens      *int            `json:"max_tokens,omitempty"`
	ResponseFormat json.RawMessage `json:"response_format,omitempty"`
}

type orResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`

	// OpenRouter surfaces provider-level errors inside the JSON body.
	Error *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

type openRouterError struct {
	message   string
	retryable bool
}

func (e *openRouterError) Error() string   { return e.message }
func (e *openRouterError) Retryable() bool { return e.retryable }

// ── provider ──────────────────────────────────────────────────────────────────

// OpenRouterProvider calls the OpenRouter Chat Completions API.
// Credentials are held server-side (OPENROUTER_API_KEY in .env).
type OpenRouterProvider struct {
	apiKey   string
	model    string
	endpoint string
	client   *http.Client
}

// NewOpenRouterProvider constructs the provider.
// model should be a full OpenRouter model ID, such as "provider/model-name".
func NewOpenRouterProvider(apiKey, model string) *OpenRouterProvider {
	return &OpenRouterProvider{apiKey: apiKey, model: model, endpoint: openRouterAPIURL, client: openRouterHTTPClient}
}

// Name satisfies the Provider interface.
func (p *OpenRouterProvider) Name() string { return "openrouter" }

// Process sends the task + input to OpenRouter and returns the model's reply.
//
// Network failures, timeouts, rate limits, server errors, and malformed
// responses are marked retryable. Other HTTP errors are treated as permanent.
func (p *OpenRouterProvider) Process(parent context.Context, task TaskRecord) (string, error) {
	endpoint := p.endpoint
	if endpoint == "" {
		endpoint = openRouterAPIURL
	}
	client := p.client
	if client == nil {
		client = openRouterHTTPClient
	}
	model := strings.TrimSpace(task.Model)
	if model == "" {
		model = p.model
	}
	body, err := json.Marshal(orRequest{
		Model:          model,
		Temperature:    task.Temperature,
		MaxTokens:      task.MaxOutputTokens,
		ResponseFormat: task.ResponseFormat,
		Messages: []orMessage{
			{Role: "system", Content: buildORSystemPrompt(task.Task)},
			{Role: "user", Content: task.Input},
		},
	})
	if err != nil {
		return "", fmt.Errorf("openrouter: could not encode request: %w", err)
	}

	req, err := http.NewRequestWithContext(parent, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return "", &openRouterError{message: fmt.Sprintf("openrouter: could not build request: %v", err)}
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+p.apiKey)
	// OpenRouter asks for these headers to track app usage in the dashboard.
	req.Header.Set("HTTP-Referer", "https://github.com/modelrail")
	req.Header.Set("X-Title", "ModelRail")

	resp, err := client.Do(req)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(parent.Err(), context.DeadlineExceeded) {
			return "", &openRouterError{
				message:   "openrouter: request timed out",
				retryable: true,
			}
		}
		return "", &openRouterError{
			message:   "openrouter: request failed",
			retryable: true,
		}
	}
	defer resp.Body.Close()

	rawBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20)) // 1 MB cap
	if err != nil {
		return "", &openRouterError{
			message:   fmt.Sprintf("openrouter: failed to read response body: %v", err),
			retryable: true,
		}
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Log the raw body to the server console so operators can diagnose
		// failures without storing potentially sensitive content in the DB.
		slog.Warn("openrouter non-2xx response",
			"status", resp.StatusCode,
			"model", model,
			"endpoint", p.endpoint,
			"body", truncate(string(rawBody), 500),
		)
		return "", &openRouterError{
			message:   fmt.Sprintf("openrouter: HTTP %d", resp.StatusCode),
			retryable: isRetryableOpenRouterStatus(resp.StatusCode),
		}
	}

	var orResp orResponse
	if err := json.Unmarshal(rawBody, &orResp); err != nil {
		return "", &openRouterError{
			message:   fmt.Sprintf("openrouter: could not parse response: %v", err),
			retryable: true,
		}
	}

	// Some errors are returned as HTTP 200 with an error field in the body.
	if orResp.Error != nil {
		return "", &openRouterError{
			message:   fmt.Sprintf("openrouter: provider error (code %d): %s", orResp.Error.Code, orResp.Error.Message),
			retryable: isRetryableOpenRouterStatus(orResp.Error.Code),
		}
	}

	if len(orResp.Choices) == 0 || orResp.Choices[0].Message.Content == "" {
		return "", &openRouterError{
			message:   "openrouter: response contained no content",
			retryable: true,
		}
	}

	return orResp.Choices[0].Message.Content, nil
}

func isRetryableOpenRouterStatus(status int) bool {
	return status == http.StatusRequestTimeout ||
		status == http.StatusTooManyRequests ||
		status >= http.StatusInternalServerError
}

// truncate caps a string to n runes for safe inclusion in error messages.
func truncate(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n]) + "…"
}

// buildORSystemPrompt turns a task name into a concise system instruction.
func buildORSystemPrompt(task string) string {
	return fmt.Sprintf(
		"You are a helpful assistant. Your job: %s. "+
			"Reply with only the result — no meta-commentary.",
		task,
	)
}
