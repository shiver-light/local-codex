package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDefaultConfig(t *testing.T) {
	cfg, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Agent.MaxIterations != 30 {
		t.Errorf("default max_iterations = %d", cfg.Agent.MaxIterations)
	}
	if cfg.LLM.BaseURL == "" || cfg.LLM.Model == "" {
		t.Error("default LLM config should be populated")
	}
}

func TestYAMLLoad(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	yaml := "llm:\n  model: yaml-model\n  base_url: http://yaml:1/v1\nagent:\n  max_iterations: 7\n"
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.LLM.Model != "yaml-model" || cfg.Agent.MaxIterations != 7 {
		t.Errorf("yaml not applied: %+v", cfg)
	}
	if cfg.Server.Port != 8080 {
		t.Error("unspecified values should keep defaults")
	}
}

func TestEnvOverridesYAML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("llm:\n  model: yaml-model\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LLM_MODEL", "env-model")
	t.Setenv("MAX_AGENT_ITERATIONS", "99")
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.LLM.Model != "env-model" {
		t.Errorf("env should override yaml, got %q", cfg.LLM.Model)
	}
	if cfg.Agent.MaxIterations != 99 {
		t.Errorf("env int override failed, got %d", cfg.Agent.MaxIterations)
	}
}

func TestMissingFileUsesDefaults(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "nope.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.Port != 8080 {
		t.Error("missing config file should fall back to defaults")
	}
}
