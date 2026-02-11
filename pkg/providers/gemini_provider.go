package providers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// GeminiProvider implements LLMProvider using the native Gemini REST API.
type GeminiProvider struct {
	apiKey     string
	apiBase    string
	httpClient *http.Client
}

func NewGeminiProvider(apiKey, apiBase string) *GeminiProvider {
	return &GeminiProvider{
		apiKey:  apiKey,
		apiBase: apiBase,
		httpClient: &http.Client{
			Timeout: 120 * time.Second,
		},
	}
}

// geminiContent represents a single turn in the Gemini conversation.
type geminiContent struct {
	Role  string       `json:"role"`
	Parts []geminiPart `json:"parts"`
}

// geminiPart is a union type for text, function call, or function response.
type geminiPart struct {
	Text             string                `json:"text,omitempty"`
	FunctionCall     *geminiFunctionCall   `json:"functionCall,omitempty"`
	FunctionResponse *geminiFuncResponse   `json:"functionResponse,omitempty"`
}

type geminiFunctionCall struct {
	Name string                 `json:"name"`
	Args map[string]interface{} `json:"args"`
}

type geminiFuncResponse struct {
	Name     string                 `json:"name"`
	Response map[string]interface{} `json:"response"`
}

type geminiFunctionDeclaration struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description"`
	Parameters  map[string]interface{} `json:"parameters,omitempty"`
}

func (g *GeminiProvider) Chat(ctx context.Context, messages []Message, tools []ToolDefinition, model string, options map[string]interface{}) (*LLMResponse, error) {
	if g.apiBase == "" {
		return nil, fmt.Errorf("Gemini API base not configured")
	}

	// Build request body
	body := map[string]interface{}{}

	// Convert messages to Gemini format
	contents, systemInstruction := convertMessagesToGemini(messages)
	body["contents"] = contents

	if systemInstruction != "" {
		body["systemInstruction"] = map[string]interface{}{
			"parts": []map[string]interface{}{
				{"text": systemInstruction},
			},
		}
	}

	// Convert tools
	if len(tools) > 0 {
		declarations := make([]geminiFunctionDeclaration, 0, len(tools))
		for _, t := range tools {
			declarations = append(declarations, geminiFunctionDeclaration{
				Name:        t.Function.Name,
				Description: t.Function.Description,
				Parameters:  t.Function.Parameters,
			})
		}
		body["tools"] = []map[string]interface{}{
			{"functionDeclarations": declarations},
		}
		body["toolConfig"] = map[string]interface{}{
			"functionCallingConfig": map[string]interface{}{
				"mode": "AUTO",
			},
		}
	}

	// Generation config
	genConfig := map[string]interface{}{}
	if maxTokens, ok := options["max_tokens"].(int); ok {
		genConfig["maxOutputTokens"] = maxTokens
	}
	if temperature, ok := options["temperature"].(float64); ok {
		genConfig["temperature"] = temperature
	}
	if len(genConfig) > 0 {
		body["generationConfig"] = genConfig
	}

	jsonData, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal Gemini request: %w", err)
	}

	// Gemini endpoint: /v1beta/models/{model}:generateContent?key={apiKey}
	url := fmt.Sprintf("%s/models/%s:generateContent?key=%s", g.apiBase, model, g.apiKey)

	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(jsonData))
	if err != nil {
		return nil, fmt.Errorf("failed to create Gemini request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := g.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("Gemini API request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read Gemini response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Gemini API error (%d): %s", resp.StatusCode, string(respBody))
	}

	return parseGeminiResponse(respBody)
}

func (g *GeminiProvider) GetDefaultModel() string {
	return "gemini-2.5-flash"
}

// convertMessagesToGemini converts OpenAI-style messages to Gemini format.
// Returns (contents, systemInstruction).
func convertMessagesToGemini(messages []Message) ([]geminiContent, string) {
	var contents []geminiContent
	var systemInstruction string

	for _, msg := range messages {
		switch msg.Role {
		case "system":
			// Gemini uses systemInstruction, not a system role in contents
			if systemInstruction == "" {
				systemInstruction = msg.Content
			} else {
				systemInstruction += "\n" + msg.Content
			}

		case "assistant":
			parts := []geminiPart{}
			if msg.Content != "" {
				parts = append(parts, geminiPart{Text: msg.Content})
			}
			// Convert tool calls from assistant
			for _, tc := range msg.ToolCalls {
				args := tc.Arguments
				if args == nil {
					args = map[string]interface{}{}
				}
				name := tc.Name
				if name == "" && tc.Function != nil {
					name = tc.Function.Name
					if tc.Function.Arguments != "" {
						_ = json.Unmarshal([]byte(tc.Function.Arguments), &args)
					}
				}
				parts = append(parts, geminiPart{
					FunctionCall: &geminiFunctionCall{
						Name: name,
						Args: args,
					},
				})
			}
			if len(parts) > 0 {
				contents = append(contents, geminiContent{Role: "model", Parts: parts})
			}

		case "tool":
			// Tool result → functionResponse
			var result map[string]interface{}
			if err := json.Unmarshal([]byte(msg.Content), &result); err != nil {
				result = map[string]interface{}{"result": msg.Content}
			}
			// Find the tool name from ToolCallID by looking back at previous assistant message
			toolName := "unknown"
			if msg.ToolCallID != "" {
				for i := len(contents) - 1; i >= 0; i-- {
					for _, p := range contents[i].Parts {
						if p.FunctionCall != nil {
							// Match by position (Gemini doesn't use IDs)
							toolName = p.FunctionCall.Name
							break
						}
					}
					if toolName != "unknown" {
						break
					}
				}
			}
			contents = append(contents, geminiContent{
				Role: "user",
				Parts: []geminiPart{{
					FunctionResponse: &geminiFuncResponse{
						Name:     toolName,
						Response: result,
					},
				}},
			})

		default: // "user"
			contents = append(contents, geminiContent{
				Role: "user",
				Parts: []geminiPart{{Text: msg.Content}},
			})
		}
	}

	return contents, systemInstruction
}

func parseGeminiResponse(body []byte) (*LLMResponse, error) {
	var resp struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					Text         string `json:"text"`
					FunctionCall *struct {
						Name string                 `json:"name"`
						Args map[string]interface{} `json:"args"`
					} `json:"functionCall"`
				} `json:"parts"`
				Role string `json:"role"`
			} `json:"content"`
			FinishReason string `json:"finishReason"`
		} `json:"candidates"`
		UsageMetadata *struct {
			PromptTokenCount     int `json:"promptTokenCount"`
			CandidatesTokenCount int `json:"candidatesTokenCount"`
			TotalTokenCount      int `json:"totalTokenCount"`
		} `json:"usageMetadata"`
	}

	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("failed to parse Gemini response: %w", err)
	}

	if len(resp.Candidates) == 0 {
		return &LLMResponse{Content: "", FinishReason: "stop"}, nil
	}

	candidate := resp.Candidates[0]

	var content string
	var toolCalls []ToolCall

	for i, part := range candidate.Content.Parts {
		if part.Text != "" {
			content += part.Text
		}
		if part.FunctionCall != nil {
			toolCalls = append(toolCalls, ToolCall{
				ID:        fmt.Sprintf("call_%d", i),
				Name:      part.FunctionCall.Name,
				Arguments: part.FunctionCall.Args,
			})
		}
	}

	finishReason := "stop"
	switch candidate.FinishReason {
	case "STOP":
		finishReason = "stop"
	case "MAX_TOKENS":
		finishReason = "length"
	case "SAFETY", "RECITATION", "OTHER":
		finishReason = "stop"
	}

	result := &LLMResponse{
		Content:      content,
		ToolCalls:    toolCalls,
		FinishReason: finishReason,
	}

	if resp.UsageMetadata != nil {
		result.Usage = &UsageInfo{
			PromptTokens:     resp.UsageMetadata.PromptTokenCount,
			CompletionTokens: resp.UsageMetadata.CandidatesTokenCount,
			TotalTokens:      resp.UsageMetadata.TotalTokenCount,
		}
	}

	return result, nil
}
