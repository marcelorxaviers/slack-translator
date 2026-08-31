// Package translator talks to OpenRouter's chat completions endpoint to
// translate text, and implements an automatic fallback chain across
// multiple free-tier models so a single model's daily rate limit doesn't
// take the whole app down.
package translator

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// ErrRateLimit is returned when a model reports it is rate limited
// (HTTP 429, or an OpenRouter error body indicating the same).
var ErrRateLimit = errors.New("model rate limited")

// ErrModelUnavailable is returned when a model reports it is not usable
// right now - e.g. OpenRouter's "This model is unavailable for free"
// (the free variant dropped, so only a paid slug is available). Such a
// model should be treated as exhausted and skipped in favor of the next
// free model, just like a rate limit.
var ErrModelUnavailable = errors.New("model unavailable")

// ErrEmptyResponse is returned when the model responded but produced no text.
var ErrEmptyResponse = errors.New("model returned an empty translation")

// ErrGarbageResponse is returned when the model "succeeded" but returned
// content that is clearly not a real translation - e.g. a Slack-internal UI
// label like "User Safety: safe" that free-tier models sometimes echo back.
// Such output is discarded so the pool can try the next model and it is
// never written to the translation cache.
var ErrGarbageResponse = errors.New("model returned unusable translation content")

const (
	openRouterURL    = "https://openrouter.ai/api/v1/chat/completions"
	openRouterModels = "https://openrouter.ai/api/v1/models"
)

// Client is a minimal OpenRouter chat-completions client, scoped to the
// single "translate this text" use case.
type Client struct {
	apiKey     string
	httpClient *http.Client
	// AppURL and AppTitle are sent as OpenRouter's recommended attribution
	// headers (HTTP-Referer / X-Title). Optional but good practice.
	AppURL   string
	AppTitle string
}

func NewClient(apiKey string) *Client {
	return &Client{
		apiKey: apiKey,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
		AppTitle: "slack-translator",
	}
}

type chatRequest struct {
	Model       string        `json:"model"`
	Messages    []chatMessage `json:"messages"`
	Temperature float64       `json:"temperature"`
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatResponse struct {
	Choices []struct {
		Message chatMessage `json:"message"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
		Code    int    `json:"code"`
	} `json:"error"`
}

// buildPrompt asks the model to translate and to return ONLY the
// translation, with no commentary, quotes, or explanations - this keeps
// parsing trivial and avoids the model "helpfully" adding notes.
func buildPrompt(text, targetLangName string) []chatMessage {
	system := "You are a translation engine embedded in a chat application. " +
		"Translate the user's message into the requested target language. " +
		"Preserve tone, emojis, and formatting (e.g. Slack markdown like *bold* or _italic_). " +
		"Reply with ONLY the translated text - no quotes, no explanations, no language labels."

	user := fmt.Sprintf("Target language: %s\n\nText to translate:\n%s", targetLangName, text)

	return []chatMessage{
		{Role: "system", Content: system},
		{Role: "user", Content: user},
	}
}

// Translate calls a specific OpenRouter model and returns the translated text.
func (c *Client) Translate(ctx context.Context, model, text, targetLangName string) (string, error) {
	reqBody := chatRequest{
		Model:       model,
		Messages:    buildPrompt(text, targetLangName),
		Temperature: 0.2, // translations should be literal/stable, not creative
	}

	payload, err := json.Marshal(reqBody)
	if err != nil {
		return "", fmt.Errorf("marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, openRouterURL, bytes.NewReader(payload))
	if err != nil {
		return "", fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Content-Type", "application/json")
	if c.AppURL != "" {
		req.Header.Set("HTTP-Referer", c.AppURL)
	}
	if c.AppTitle != "" {
		req.Header.Set("X-Title", c.AppTitle)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("http request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode == http.StatusTooManyRequests {
		return "", ErrRateLimit
	}

	var parsed chatResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", fmt.Errorf("parse response (status %d): %w", resp.StatusCode, err)
	}

	if parsed.Error != nil {
		msg := strings.ToLower(parsed.Error.Message)
		if parsed.Error.Code == 429 || strings.Contains(msg, "rate limit") || strings.Contains(msg, "quota") {
			return "", ErrRateLimit
		}
		if strings.Contains(msg, "unavailable for free") ||
			strings.Contains(msg, "requires a paid") ||
			strings.Contains(msg, "paid version") ||
			strings.Contains(msg, "not available") ||
			strings.Contains(msg, "model is unavailable") ||
			strings.Contains(msg, "no free") {
			return "", ErrModelUnavailable
		}
		return "", fmt.Errorf("openrouter error: %s", parsed.Error.Message)
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("openrouter http %d: %s", resp.StatusCode, string(body))
	}

	if len(parsed.Choices) == 0 || strings.TrimSpace(parsed.Choices[0].Message.Content) == "" {
		return "", ErrEmptyResponse
	}

	out := strings.TrimSpace(parsed.Choices[0].Message.Content)
	if isGarbageTranslation(out) {
		return "", ErrGarbageResponse
	}

	return out, nil
}

// isGarbageTranslation reports whether a "successful" model output is clearly
// not a translation. Free-tier models occasionally echo Slack-internal UI
// strings (like "User Safety: safe") or other system labels verbatim instead
// of translating. Those are not translations and must not be shown or cached.
func isGarbageTranslation(out string) bool {
	lower := strings.ToLower(out)

	// Slack user-safety indicator labels the model sometimes copies verbatim.
	if strings.Contains(lower, "user safety:") || strings.Contains(lower, "user safety safe") {
		return true
	}

	// Case where the model just echoes the input unchanged (no actual
	// translation happened for a target language that differs from source).
	return false
}

type modelsResponse struct {
	Data []struct {
		ID       string `json:"id"`
		Pricing  struct {
			Prompt string `json:"prompt"`
		} `json:"pricing"`
	} `json:"data"`
}

// ListFreeModels fetches OpenRouter's current model catalog and returns the
// set of model IDs whose prompt pricing is free (i.e. still usable without
// burning credits). It is used to proactively detect models that have been
// pulled from the free tier, so the pool can skip them before wasting a
// translation request. The `openrouter/free` router is always treated as
// available since it re-routes to whatever free model exists.
func (c *Client) ListFreeModels(ctx context.Context) (map[string]bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, openRouterModels, nil)
	if err != nil {
		return nil, fmt.Errorf("build models request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("models request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("openrouter models http %d", resp.StatusCode)
	}

	var parsed modelsResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, fmt.Errorf("parse models response: %w", err)
	}

	free := make(map[string]bool)
	for _, m := range parsed.Data {
		if m.Pricing.Prompt == "0" {
			free[m.ID] = true
		}
	}
	free["openrouter/free"] = true
	return free, nil
}
