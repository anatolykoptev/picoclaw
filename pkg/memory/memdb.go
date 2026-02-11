package memory

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/sipeed/picoclaw/pkg/logger"
)

// MemDBClient is an HTTP client for the MemDB memory API.
type MemDBClient struct {
	apiURL     string
	userID     string
	cubeID     string
	secret     string
	httpClient *http.Client
}

// MemDBConfig holds configuration for the MemDB client.
type MemDBConfig struct {
	Enabled bool   `json:"enabled" env:"PICOCLAW_MEMORY_MEMDB_ENABLED"`
	URL     string `json:"url" env:"PICOCLAW_MEMORY_MEMDB_URL"`
	UserID  string `json:"user_id" env:"PICOCLAW_MEMORY_MEMDB_USER_ID"`
	CubeID  string `json:"cube_id" env:"PICOCLAW_MEMORY_MEMDB_CUBE_ID"`
	Secret  string `json:"secret" env:"PICOCLAW_MEMORY_MEMDB_SECRET"`
}

// SearchResult holds formatted search results from MemDB.
type SearchResult struct {
	TextMemories  []MemoryItem
	SkillMemories []MemoryItem
	PrefMemories  []MemoryItem
}

// MemoryItem is a single memory entry.
type MemoryItem struct {
	ID      string
	Content string
	Score   float64
}

// NewMemDBClient creates a new MemDB HTTP client.
func NewMemDBClient(cfg MemDBConfig) *MemDBClient {
	return &MemDBClient{
		apiURL: strings.TrimRight(cfg.URL, "/"),
		userID: cfg.UserID,
		cubeID: cfg.CubeID,
		secret: cfg.Secret,
		httpClient: &http.Client{
			Timeout: 10 * time.Second,
		},
	}
}

// Search queries MemDB for memories relevant to the given query.
func (c *MemDBClient) Search(ctx context.Context, query string) (*SearchResult, error) {
	body := map[string]interface{}{
		"query":                query,
		"user_id":              c.userID,
		"readable_cube_ids":    []string{c.cubeID},
		"top_k":                8,
		"include_skill_memory": true,
		"dedup":                "mmr",
		"relativity":           0.85,
	}

	jsonData, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal search request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", c.apiURL+"/product/search", bytes.NewReader(jsonData))
	if err != nil {
		return nil, fmt.Errorf("create search request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.secret != "" {
		req.Header.Set("X-Internal-Service", c.secret)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("search request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read search response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("search API error %d: %s", resp.StatusCode, string(respBody))
	}

	return parseSearchResponse(respBody)
}

// Store sends conversation messages to MemDB for extraction and storage.
// This is fire-and-forget — errors are logged but not returned.
func (c *MemDBClient) Store(ctx context.Context, messages []map[string]string) {
	body := map[string]interface{}{
		"user_id":            c.userID,
		"writable_cube_ids":  []string{c.cubeID},
		"messages":           messages,
		"mode":               "fast",
	}

	jsonData, err := json.Marshal(body)
	if err != nil {
		logger.ErrorCF("memdb", "marshal store request", map[string]interface{}{"error": err.Error()})
		return
	}

	req, err := http.NewRequestWithContext(ctx, "POST", c.apiURL+"/product/add", bytes.NewReader(jsonData))
	if err != nil {
		logger.ErrorCF("memdb", "create store request", map[string]interface{}{"error": err.Error()})
		return
	}
	req.Header.Set("Content-Type", "application/json")
	if c.secret != "" {
		req.Header.Set("X-Internal-Service", c.secret)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		logger.ErrorCF("memdb", "store request failed", map[string]interface{}{"error": err.Error()})
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		logger.ErrorCF("memdb", "store API error", map[string]interface{}{
			"status": resp.StatusCode,
			"body":   string(body),
		})
		return
	}

	logger.DebugCF("memdb", "stored conversation", map[string]interface{}{
		"messages": len(messages),
	})
}

// Health checks if MemDB is reachable.
func (c *MemDBClient) Health(ctx context.Context) bool {
	req, err := http.NewRequestWithContext(ctx, "GET", c.apiURL+"/health", nil)
	if err != nil {
		return false
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()

	return resp.StatusCode == http.StatusOK
}

// FormatForPrompt formats search results as a text block for the system prompt.
func (r *SearchResult) FormatForPrompt() string {
	if r == nil {
		return ""
	}

	var parts []string

	if len(r.TextMemories) > 0 {
		var items []string
		for _, m := range r.TextMemories {
			items = append(items, fmt.Sprintf("- %s", m.Content))
		}
		parts = append(parts, "### Facts & Knowledge\n"+strings.Join(items, "\n"))
	}

	if len(r.SkillMemories) > 0 {
		var items []string
		for _, m := range r.SkillMemories {
			items = append(items, fmt.Sprintf("- %s", m.Content))
		}
		parts = append(parts, "### Skills & Procedures\n"+strings.Join(items, "\n"))
	}

	if len(r.PrefMemories) > 0 {
		var items []string
		for _, m := range r.PrefMemories {
			items = append(items, fmt.Sprintf("- %s", m.Content))
		}
		parts = append(parts, "### User Preferences\n"+strings.Join(items, "\n"))
	}

	if len(parts) == 0 {
		return ""
	}

	return "## Relevant Memories (from MemDB)\n\n" + strings.Join(parts, "\n\n")
}

// parseSearchResponse parses the MemDB search API response.
// Response shape: data.text_mem[0].memories[], data.skill_mem[0].memories[], data.pref_mem[0].memories[]
func parseSearchResponse(body []byte) (*SearchResult, error) {
	var resp struct {
		Data struct {
			TextMem []struct {
				Memories []struct {
					ID       string                 `json:"id"`
					Memory   string                 `json:"memory"`
					Score    float64                `json:"score"`
					Metadata map[string]interface{} `json:"metadata"`
				} `json:"memories"`
			} `json:"text_mem"`
			SkillMem []struct {
				Memories []struct {
					ID       string                 `json:"id"`
					Memory   string                 `json:"memory"`
					Score    float64                `json:"score"`
					Metadata map[string]interface{} `json:"metadata"`
				} `json:"memories"`
			} `json:"skill_mem"`
			PrefMem []struct {
				Memories []struct {
					ID       string                 `json:"id"`
					Memory   string                 `json:"memory"`
					Score    float64                `json:"score"`
					Metadata map[string]interface{} `json:"metadata"`
				} `json:"memories"`
			} `json:"pref_mem"`
		} `json:"data"`
	}

	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("unmarshal search response: %w", err)
	}

	result := &SearchResult{}

	if len(resp.Data.TextMem) > 0 {
		for _, m := range resp.Data.TextMem[0].Memories {
			content := m.Memory
			if content == "" {
				continue
			}
			result.TextMemories = append(result.TextMemories, MemoryItem{
				ID:      m.ID,
				Content: content,
				Score:   m.Score,
			})
		}
	}

	if len(resp.Data.SkillMem) > 0 {
		for _, m := range resp.Data.SkillMem[0].Memories {
			content := formatSkillMemory(m.Metadata)
			if content == "" {
				content = m.Memory
			}
			if content == "" {
				continue
			}
			result.SkillMemories = append(result.SkillMemories, MemoryItem{
				ID:      m.ID,
				Content: content,
				Score:   m.Score,
			})
		}
	}

	if len(resp.Data.PrefMem) > 0 {
		for _, m := range resp.Data.PrefMem[0].Memories {
			content := m.Memory
			if content == "" {
				continue
			}
			result.PrefMemories = append(result.PrefMemories, MemoryItem{
				ID:      m.ID,
				Content: content,
				Score:   m.Score,
			})
		}
	}

	return result, nil
}

// formatSkillMemory formats a skill memory entry from its metadata.
func formatSkillMemory(metadata map[string]interface{}) string {
	if metadata == nil {
		return ""
	}

	name, _ := metadata["name"].(string)
	description, _ := metadata["description"].(string)
	procedure, _ := metadata["procedure"].(string)

	if name == "" && description == "" {
		return ""
	}

	var parts []string
	if name != "" {
		parts = append(parts, fmt.Sprintf("**%s**", name))
	}
	if description != "" {
		parts = append(parts, description)
	}
	if procedure != "" {
		parts = append(parts, fmt.Sprintf("Procedure: %s", procedure))
	}

	return strings.Join(parts, " — ")
}
