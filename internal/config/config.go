package config

import (
	"fmt"
	"os"
	"strconv"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Server    ServerConfig    `yaml:"server"`
	LLM       LLMConfig       `yaml:"llm"`
	Agent     AgentConfig     `yaml:"agent"`
	Workspace WorkspaceConfig `yaml:"workspace"`
	Shell     ShellConfig     `yaml:"shell"`
	Logging   LoggingConfig   `yaml:"logging"`
}

type ServerConfig struct {
	Port int    `yaml:"port"`
	Host string `yaml:"host"`
	// Token authenticates /api/* requests (Bearer header). Empty means the
	// server generates a random token at startup and prints it.
	Token string `yaml:"token"`
}

type LLMConfig struct {
	BaseURL     string   `yaml:"base_url"`
	APIKey      string   `yaml:"api_key"`
	Model       string   `yaml:"model"`
	Temperature *float64 `yaml:"temperature"`
	MaxTokens   int      `yaml:"max_tokens"`
}

type AgentConfig struct {
	MaxIterations       int `yaml:"max_iterations"`
	MaxRepeatToolCalls  int `yaml:"max_consecutive_repeat_tool_calls"`
	MaxContextBytes     int `yaml:"max_context_bytes"`
}

type WorkspaceConfig struct {
	Root string `yaml:"root"`
}

type ShellConfig struct {
	DefaultTimeout int `yaml:"default_timeout"`
	MaxTimeout     int `yaml:"max_timeout"`
}

type LoggingConfig struct {
	Level string `yaml:"level"`
	File  string `yaml:"file"`
}

func Default() *Config {
	temp := 0.2
	return &Config{
		Server: ServerConfig{Port: 8080, Host: "127.0.0.1"},
		LLM: LLMConfig{
			BaseURL:     "http://127.0.0.1:11434/v1",
			APIKey:      "ollama",
			Model:       "qwen3-coder",
			Temperature: &temp,
			MaxTokens:   4096,
		},
		Agent: AgentConfig{
			MaxIterations:      30,
			MaxRepeatToolCalls: 3,
			MaxContextBytes:    200_000,
		},
		Workspace: WorkspaceConfig{Root: "."},
		Shell:     ShellConfig{DefaultTimeout: 120, MaxTimeout: 600},
		Logging:   LoggingConfig{Level: "info"},
	}
}

// Load reads YAML config (if path is non-empty and the file exists), then
// applies environment variable overrides. Env always wins.
func Load(path string) (*Config, error) {
	cfg := Default()
	if path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			if !os.IsNotExist(err) {
				return nil, fmt.Errorf("read config %s: %w", path, err)
			}
			// The default path is relative to the CWD; say so out loud when
			// we silently fall back to built-in defaults.
			fmt.Fprintf(os.Stderr, "local-codex: config %s not found; using built-in defaults (env vars still override)\n", path)
		} else if err := yaml.Unmarshal(data, cfg); err != nil {
			return nil, fmt.Errorf("parse config %s: %w", path, err)
		}
	}
	applyEnv(cfg)
	return cfg, nil
}

func applyEnv(cfg *Config) {
	setStr := func(env string, dst *string) {
		if v, ok := os.LookupEnv(env); ok && v != "" {
			*dst = v
		}
	}
	setInt := func(env string, dst *int) {
		if v, ok := os.LookupEnv(env); ok && v != "" {
			if n, err := strconv.Atoi(v); err == nil {
				*dst = n
			}
		}
	}

	setStr("LLM_BASE_URL", &cfg.LLM.BaseURL)
	setStr("LLM_API_KEY", &cfg.LLM.APIKey)
	setStr("LLM_MODEL", &cfg.LLM.Model)
	setInt("MAX_AGENT_ITERATIONS", &cfg.Agent.MaxIterations)
	setInt("PORT", &cfg.Server.Port)
	setStr("HOST", &cfg.Server.Host)
	setStr("LOCAL_CODEX_TOKEN", &cfg.Server.Token)
	setInt("SHELL_DEFAULT_TIMEOUT", &cfg.Shell.DefaultTimeout)
	setStr("LOG_LEVEL", &cfg.Logging.Level)
	setStr("LOG_FILE", &cfg.Logging.File)
}
