package provider

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"math"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type ModelConfig struct {
	BaseURL             string // Explicit compatible API root, e.g. https://provider.example/v1.
	APIKey              string
	ChatModel           string
	VisionModel         string
	EmbeddingModel      string
	EmbeddingDimensions int
	HTTPClient          *http.Client
	Timeout             time.Duration
	MaxRequestBytes     int64
	MaxResponseBytes    int64
	AllowInsecureHTTP   bool // Explicit local/test endpoints only.
}

type OpenAICompatible struct {
	cfg    ModelConfig
	client *http.Client
}

func NewOpenAICompatible(cfg ModelConfig) (*OpenAICompatible, error) {
	if _, err := configuredURL(cfg.BaseURL, cfg.AllowInsecureHTTP); err != nil {
		return nil, err
	}
	if !safeIdentifier(cfg.ChatModel) || cfg.APIKey == "" || strings.ContainsAny(cfg.APIKey, "\r\n") {
		return nil, &Error{Code: "MISSING_MODEL_CONFIGURATION"}
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 90 * time.Second
	}
	if cfg.Timeout < 0 {
		return nil, &Error{Code: "INVALID_TIMEOUT"}
	}
	if cfg.MaxRequestBytes == 0 {
		cfg.MaxRequestBytes = 24 << 20
	}
	if cfg.MaxResponseBytes == 0 {
		cfg.MaxResponseBytes = 8 << 20
	}
	if cfg.MaxRequestBytes < 1 || cfg.MaxResponseBytes < 1 || cfg.EmbeddingDimensions < 0 {
		return nil, &Error{Code: "INVALID_LIMITS"}
	}
	if cfg.VisionModel == "" {
		cfg.VisionModel = cfg.ChatModel
	}
	cfg.BaseURL = strings.TrimRight(cfg.BaseURL, "/")
	return &OpenAICompatible{cfg: cfg, client: boundedClient(cfg.HTTPClient, cfg.Timeout)}, nil
}

func (p *OpenAICompatible) Chat(ctx context.Context, request ChatRequest) (ChatResponse, error) {
	return p.chat(ctx, p.cfg.ChatModel, request)
}

func (p *OpenAICompatible) Vision(ctx context.Context, request VisionRequest) (ChatResponse, error) {
	if len(request.Images) == 0 || len(request.Images) > 8 {
		return ChatResponse{}, &Error{Code: "INVALID_IMAGE_COUNT"}
	}
	return p.chat(ctx, p.cfg.VisionModel, ChatRequest{Messages: []Message{{Role: "user", Content: request.Prompt, Images: request.Images}}, MaxTokens: request.MaxTokens, JSONMode: request.JSONMode})
}

type wireToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}
type wireMessage struct {
	Role       string         `json:"role"`
	Content    any            `json:"content"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
	ToolCalls  []wireToolCall `json:"tool_calls,omitempty"`
}

func (p *OpenAICompatible) imageURL(image Image) (string, error) {
	if (image.URL == "") == (len(image.Data) == 0) {
		return "", &Error{Code: "INVALID_IMAGE"}
	}
	if len(image.Data) > 0 {
		if len(image.Data) > 10<<20 {
			return "", &Error{Code: "IMAGE_TOO_LARGE"}
		}
		mime := http.DetectContentType(image.Data)
		if mime != "image/png" && mime != "image/jpeg" && mime != "image/webp" && mime != "image/gif" {
			return "", &Error{Code: "UNSUPPORTED_IMAGE_TYPE"}
		}
		if image.MIMEType != "" && image.MIMEType != mime {
			return "", &Error{Code: "IMAGE_TYPE_MISMATCH"}
		}
		return "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(image.Data), nil
	}
	u, err := url.Parse(image.URL)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || len(image.URL) > 16384 {
		return "", &Error{Code: "INVALID_IMAGE_URL"}
	}
	return image.URL, nil
}

func (p *OpenAICompatible) chat(ctx context.Context, model string, request ChatRequest) (ChatResponse, error) {
	if !safeIdentifier(model) || len(request.Messages) == 0 || len(request.Messages) > 256 || len(request.Tools) > 64 || request.MaxTokens < 0 || request.MaxTokens > 65536 {
		return ChatResponse{}, &Error{Code: "INVALID_CHAT_REQUEST"}
	}
	messages := make([]wireMessage, 0, len(request.Messages))
	for _, msg := range request.Messages {
		if msg.Role != "system" && msg.Role != "user" && msg.Role != "assistant" && msg.Role != "tool" {
			return ChatResponse{}, &Error{Code: "INVALID_MESSAGE_ROLE"}
		}
		if msg.Role == "tool" && !safeIdentifier(msg.ToolCallID) {
			return ChatResponse{}, &Error{Code: "MISSING_TOOL_CALL_ID"}
		}
		if len(msg.Images) > 8 || (len(msg.Images) > 0 && msg.Role != "user") {
			return ChatResponse{}, &Error{Code: "INVALID_IMAGE_COUNT"}
		}
		wire := wireMessage{Role: msg.Role, Content: msg.Content, ToolCallID: msg.ToolCallID}
		if len(msg.Images) > 0 {
			content := []any{map[string]any{"type": "text", "text": msg.Content}}
			for _, image := range msg.Images {
				uri, err := p.imageURL(image)
				if err != nil {
					return ChatResponse{}, err
				}
				content = append(content, map[string]any{"type": "image_url", "image_url": map[string]string{"url": uri}})
			}
			wire.Content = content
		}
		for _, call := range msg.ToolCalls {
			if msg.Role != "assistant" || !safeIdentifier(call.ID) || !safeIdentifier(call.Name) || !json.Valid([]byte(call.Arguments)) {
				return ChatResponse{}, &Error{Code: "INVALID_TOOL_CALL"}
			}
			wc := wireToolCall{ID: call.ID, Type: "function"}
			wc.Function.Name = call.Name
			wc.Function.Arguments = call.Arguments
			wire.ToolCalls = append(wire.ToolCalls, wc)
		}
		messages = append(messages, wire)
	}
	body := map[string]any{"model": model, "messages": messages, "stream": false}
	if request.MaxTokens > 0 {
		body["max_tokens"] = request.MaxTokens
	}
	if request.Temperature != nil {
		if math.IsNaN(*request.Temperature) || *request.Temperature < 0 || *request.Temperature > 2 {
			return ChatResponse{}, &Error{Code: "INVALID_TEMPERATURE"}
		}
		body["temperature"] = *request.Temperature
	}
	if request.JSONMode {
		body["response_format"] = map[string]string{"type": "json_object"}
	}
	if len(request.Tools) > 0 {
		tools := make([]any, 0, len(request.Tools))
		for _, tool := range request.Tools {
			if !safeIdentifier(tool.Name) || !json.Valid(tool.Parameters) {
				return ChatResponse{}, &Error{Code: "INVALID_TOOL_SCHEMA"}
			}
			tools = append(tools, map[string]any{"type": "function", "function": tool})
		}
		body["tools"] = tools
		body["tool_choice"] = "auto"
	}
	data, headers, err := p.post(ctx, "/chat/completions", body)
	if err != nil {
		return ChatResponse{}, err
	}
	var result struct {
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Content   *string        `json:"content"`
				ToolCalls []wireToolCall `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage *wireUsage `json:"usage"`
	}
	if json.Unmarshal(data, &result) != nil || len(result.Choices) != 1 {
		return ChatResponse{}, &Error{Code: "INVALID_PROVIDER_RESPONSE"}
	}
	choice := result.Choices[0]
	out := ChatResponse{Model: result.Model, RequestID: headers.Get("X-Request-Id"), FinishReason: choice.FinishReason, Usage: normalizeUsage(result.Usage)}
	if choice.Message.Content != nil {
		out.Content = *choice.Message.Content
	}
	for _, call := range choice.Message.ToolCalls {
		if call.Type != "function" || !safeIdentifier(call.ID) || !safeIdentifier(call.Function.Name) || !json.Valid([]byte(call.Function.Arguments)) {
			return ChatResponse{}, &Error{Code: "INVALID_TOOL_CALL_RESPONSE"}
		}
		out.ToolCalls = append(out.ToolCalls, ToolCall{ID: call.ID, Name: call.Function.Name, Arguments: call.Function.Arguments})
	}
	if out.Content == "" && len(out.ToolCalls) == 0 {
		return ChatResponse{}, &Error{Code: "EMPTY_MODEL_RESPONSE"}
	}
	return out, nil
}

type wireUsage struct {
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	TotalTokens      int64 `json:"total_tokens"`
}

func normalizeUsage(u *wireUsage) Usage {
	if u == nil {
		return Usage{Known: false}
	}
	return Usage{InputTokens: u.PromptTokens, OutputTokens: u.CompletionTokens, TotalTokens: u.TotalTokens, Known: true}
}

func (p *OpenAICompatible) post(ctx context.Context, path string, body any) ([]byte, http.Header, error) {
	data, err := json.Marshal(body)
	if err != nil {
		return nil, nil, &Error{Code: "INVALID_REQUEST", Cause: err}
	}
	if int64(len(data)) > p.cfg.MaxRequestBytes {
		return nil, nil, &Error{Code: "REQUEST_TOO_LARGE"}
	}
	return requestJSON(ctx, p.client, p.cfg.BaseURL+path, http.Header{"Content-Type": []string{"application/json"}, "Authorization": []string{"Bearer " + p.cfg.APIKey}}, data, p.cfg.MaxResponseBytes)
}

func (p *OpenAICompatible) Embed(ctx context.Context, input []string) (EmbeddingResponse, error) {
	if !safeIdentifier(p.cfg.EmbeddingModel) || len(input) == 0 || len(input) > 128 {
		return EmbeddingResponse{}, &Error{Code: "INVALID_EMBEDDING_REQUEST"}
	}
	for _, s := range input {
		if strings.TrimSpace(s) == "" {
			return EmbeddingResponse{}, &Error{Code: "EMPTY_EMBEDDING_INPUT"}
		}
	}
	data, headers, err := p.post(ctx, "/embeddings", map[string]any{"model": p.cfg.EmbeddingModel, "input": input, "encoding_format": "float"})
	if err != nil {
		return EmbeddingResponse{}, err
	}
	var result struct {
		Model string     `json:"model"`
		Usage *wireUsage `json:"usage"`
		Data  []struct {
			Index     int       `json:"index"`
			Embedding []float32 `json:"embedding"`
		} `json:"data"`
	}
	if json.Unmarshal(data, &result) != nil || len(result.Data) != len(input) {
		return EmbeddingResponse{}, &Error{Code: "INVALID_EMBEDDING_RESPONSE"}
	}
	out := EmbeddingResponse{Model: result.Model, RequestID: headers.Get("X-Request-Id"), Usage: normalizeUsage(result.Usage), Vectors: make([][]float32, len(input))}
	dim := p.cfg.EmbeddingDimensions
	for _, item := range result.Data {
		if item.Index < 0 || item.Index >= len(input) || out.Vectors[item.Index] != nil || len(item.Embedding) == 0 {
			return EmbeddingResponse{}, &Error{Code: "INVALID_EMBEDDING_RESPONSE"}
		}
		if dim == 0 {
			dim = len(item.Embedding)
		}
		if len(item.Embedding) != dim {
			return EmbeddingResponse{}, &Error{Code: "EMBEDDING_DIMENSION_MISMATCH"}
		}
		var magnitude float64
		for _, v := range item.Embedding {
			if math.IsNaN(float64(v)) || math.IsInf(float64(v), 0) {
				return EmbeddingResponse{}, &Error{Code: "INVALID_EMBEDDING_VECTOR"}
			}
			magnitude += float64(v) * float64(v)
		}
		if magnitude == 0 {
			return EmbeddingResponse{}, &Error{Code: "ZERO_EMBEDDING_VECTOR"}
		}
		out.Vectors[item.Index] = item.Embedding
	}
	return out, nil
}
