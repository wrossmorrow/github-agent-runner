package review

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type piModelsFile struct {
	Providers map[string]piProviderConfig `json:"providers"`
}

type piProviderConfig struct {
	BaseURL    string            `json:"baseUrl"`
	API        string            `json:"api"`
	APIKey     string            `json:"apiKey"`
	AuthHeader bool              `json:"authHeader,omitempty"`
	Headers    map[string]string `json:"headers,omitempty"`
	Models     []piModelConfig   `json:"models"`
}

type piModelConfig struct {
	ID            string        `json:"id"`
	Name          string        `json:"name,omitempty"`
	ContextWindow int           `json:"contextWindow,omitempty"`
	MaxTokens     int           `json:"maxTokens,omitempty"`
	Compat        piModelCompat `json:"compat"`
}

type piModelCompat struct {
	SupportsStore         bool   `json:"supportsStore"`
	SupportsDeveloperRole bool   `json:"supportsDeveloperRole"`
	SupportsStrictMode    bool   `json:"supportsStrictMode"`
	MaxTokensField        string `json:"maxTokensField,omitempty"`
}

func providerName(cfg Config) string {
	if p := strings.TrimSpace(cfg.Provider); p != "" {
		return p
	}
	return "gateway"
}

// writePiConfigDir creates a per-run $PI_CODING_AGENT_DIR containing the
// assembled AGENTS.md and a models.json routing pi through gatewayEndpoint.
func writePiConfigDir(cfg Config, req Request, agentsMD string, gatewayToken string) (string, error) {
	dir, err := os.MkdirTemp("", "agentrunner-pi-*")
	if err != nil {
		return "", fmt.Errorf("creating pi config dir: %w", err)
	}

	if strings.TrimSpace(agentsMD) != "" {
		if err := os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte(agentsMD+"\n"), 0o600); err != nil {
			_ = os.RemoveAll(dir)
			return "", fmt.Errorf("writing AGENTS.md: %w", err)
		}
	}

	if cfg.GatewayEndpoint != "" && cfg.Model != "" {
		gatewayToken = strings.TrimSpace(gatewayToken)
		apiKey := "local"
		authHeader := false
		headers := map[string]string{}
		if gatewayToken != "" {
			apiKey = gatewayToken
			authHeader = true
			headers["Authorization"] = "Bearer " + gatewayToken
		}

		if req.Repo != "" {
			headers["x-github-repo"] = req.Repo
		}
		if req.RunID != "" {
			headers["x-github-run-id"] = req.RunID
		}
		if req.PRNumber > 0 {
			headers["x-github-pr"] = strconv.Itoa(req.PRNumber)
		}

		modelsDoc := piModelsFile{
			Providers: map[string]piProviderConfig{
				providerName(cfg): {
					BaseURL:    cfg.GatewayEndpoint,
					API:        "openai-completions",
					APIKey:     apiKey,
					AuthHeader: authHeader,
					Headers:    headers,
					Models: []piModelConfig{
						{
							ID:            cfg.Model,
							Name:          cfg.Model,
							ContextWindow: cfg.Limits.ContextWindow,
							MaxTokens:     cfg.Limits.MaxTokens,
							Compat: piModelCompat{
								SupportsStore:         false,
								SupportsDeveloperRole: false,
								SupportsStrictMode:    false,
								MaxTokensField:        "max_tokens",
							},
						},
					},
				},
			},
		}

		raw, err := json.MarshalIndent(modelsDoc, "", "  ")
		if err != nil {
			_ = os.RemoveAll(dir)
			return "", fmt.Errorf("marshaling models.json: %w", err)
		}
		if err := os.WriteFile(filepath.Join(dir, "models.json"), append(raw, '\n'), 0o600); err != nil {
			_ = os.RemoveAll(dir)
			return "", fmt.Errorf("writing models.json: %w", err)
		}
	}

	return dir, nil
}
