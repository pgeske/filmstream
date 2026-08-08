package resolver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

const maxModelResponseBytes = 2 << 20

type OpenAICompatible struct {
	endpoint string
	model    string
	apiKey   string
	client   *http.Client
}

func NewOpenAICompatible(baseURL, model, apiKey string, client *http.Client) (*OpenAICompatible, error) {
	baseURL = strings.TrimRight(baseURL, "/")
	parsed, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("parse model base URL: %w", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, errors.New("model base URL must use http or https")
	}
	if parsed.Host == "" {
		return nil, errors.New("model base URL must include a host")
	}
	if model == "" {
		return nil, errors.New("model name cannot be empty")
	}
	endpoint, err := url.JoinPath(baseURL, "chat/completions")
	if err != nil {
		return nil, err
	}
	return &OpenAICompatible{endpoint: endpoint, model: model, apiKey: apiKey, client: client}, nil
}

func (o *OpenAICompatible) Complete(ctx context.Context, systemPrompt, userPrompt string) (string, error) {
	content, status, err := o.complete(ctx, systemPrompt, userPrompt, true)
	if err == nil {
		return content, nil
	}
	// Some compatible servers implement chat completions but not JSON response mode.
	if status == http.StatusBadRequest {
		content, _, fallbackErr := o.complete(ctx, systemPrompt, userPrompt, false)
		if fallbackErr == nil {
			return content, nil
		}
	}
	return "", err
}

func (o *OpenAICompatible) complete(ctx context.Context, systemPrompt, userPrompt string, jsonMode bool) (string, int, error) {
	requestBody := struct {
		Model          string         `json:"model"`
		Messages       []chatMessage  `json:"messages"`
		ResponseFormat map[string]any `json:"response_format,omitempty"`
	}{
		Model: o.model,
		Messages: []chatMessage{
			{Role: "system", Content: systemPrompt},
			{Role: "user", Content: userPrompt},
		},
	}
	if jsonMode {
		requestBody.ResponseFormat = map[string]any{"type": "json_object"}
	}
	encoded, err := json.Marshal(requestBody)
	if err != nil {
		return "", 0, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, o.endpoint, bytes.NewReader(encoded))
	if err != nil {
		return "", 0, err
	}
	request.Header.Set("Content-Type", "application/json")
	if o.apiKey != "" {
		request.Header.Set("Authorization", "Bearer "+o.apiKey)
	}
	response, err := o.client.Do(request)
	if err != nil {
		return "", 0, fmt.Errorf("call model: %w", requestError(err))
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, maxModelResponseBytes))
	if err != nil {
		return "", response.StatusCode, fmt.Errorf("read model response: %w", err)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		var payload struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(body, &payload) == nil && payload.Error.Message != "" {
			return "", response.StatusCode, fmt.Errorf("model returned %s: %s", response.Status, payload.Error.Message)
		}
		return "", response.StatusCode, fmt.Errorf("model returned %s", response.Status)
	}
	var payload struct {
		Choices []struct {
			Message chatMessage `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return "", response.StatusCode, fmt.Errorf("decode model response: %w", err)
	}
	if len(payload.Choices) == 0 || strings.TrimSpace(payload.Choices[0].Message.Content) == "" {
		return "", response.StatusCode, errors.New("model returned no content")
	}
	return payload.Choices[0].Message.Content, response.StatusCode, nil
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

func requestError(err error) error {
	var urlError *url.Error
	if errors.As(err, &urlError) {
		return urlError.Err
	}
	return err
}
