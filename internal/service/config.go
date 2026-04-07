package service

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// Config represents the mgit configuration stored in .mgit/config.json.
// Note: Uses JSON format because gopkg.in/yaml.v3 is not in APPROVED-PACKAGES.md.
// YAML support requires package approval per PACKAGE-APPROVAL-PROCESS.md.
// Refs: FR-13, MGIT-3.2.4
type Config struct {
	Project  ProjectConfig  `json:"project"`
	API      APIConfig      `json:"api"`
	MCP      MCPConfig      `json:"mcp"`
	Logging  LoggingConfig  `json:"logging"`
	Git      GitConfig      `json:"git"`
	Squash   SquashConfig   `json:"squash"`
	Rollback RollbackConfig `json:"rollback"`
	Branch   BranchConfig   `json:"branch"`
	Audit    AuditConfig    `json:"audit"`
}

// ProjectConfig holds project-level settings.
type ProjectConfig struct {
	Prefix string `json:"prefix"`
	Name   string `json:"name"`
}

// APIConfig holds REST API settings.
type APIConfig struct {
	HTTPPort    int    `json:"http_port"`
	BindAddress string `json:"bind_address"`
}

// MCPConfig holds MCP server settings.
type MCPConfig struct {
	Transport string `json:"transport"`
}

// LoggingConfig holds logging settings.
type LoggingConfig struct {
	Level string `json:"level"`
}

// GitConfig holds git-related settings.
type GitConfig struct {
	AutoStage   bool `json:"auto_stage"`
	SignCommits bool `json:"sign_commits"`
}

// SquashConfig holds squash behavior settings.
type SquashConfig struct {
	AutoNotify bool `json:"auto_notify"`
}

// RollbackConfig holds rollback behavior settings.
type RollbackConfig struct {
	AutoReopen bool `json:"auto_reopen"`
}

// BranchConfig holds branch behavior settings.
type BranchConfig struct {
	AutoCreate bool `json:"auto_create"`
}

// AuditConfig holds audit log settings.
type AuditConfig struct {
	LogFile   string `json:"log_file"`
	MaxSizeMB int    `json:"max_size_mb"`
}

// DefaultConfig returns a Config with sensible defaults per FR-13.
func DefaultConfig() Config {
	return Config{
		Project:  ProjectConfig{Prefix: "MGIT", Name: "mgit"},
		API:      APIConfig{HTTPPort: 6860, BindAddress: "127.0.0.1"},
		MCP:      MCPConfig{Transport: "stdio"},
		Logging:  LoggingConfig{Level: "info"},
		Git:      GitConfig{AutoStage: false, SignCommits: false},
		Squash:   SquashConfig{AutoNotify: true},
		Rollback: RollbackConfig{AutoReopen: true},
		Branch:   BranchConfig{AutoCreate: true},
		Audit:    AuditConfig{LogFile: ".mgit/audit.log", MaxSizeMB: 100},
	}
}

// ConfigService manages mgit configuration via .mgit/config.json.
// Refs: FR-13, MGIT-3.2.4
type ConfigService struct {
	configPath string
	config     Config
}

// NewConfigService loads or creates config at the given path.
func NewConfigService(configPath string) (*ConfigService, error) {
	svc := &ConfigService{
		configPath: configPath,
		config:     DefaultConfig(),
	}

	// Try to load existing config
	if data, err := os.ReadFile(configPath); err == nil { //nolint:gosec // internal path
		if err := json.Unmarshal(data, &svc.config); err != nil {
			return nil, fmt.Errorf("parse config: %w", err)
		}
	}

	return svc, nil
}

// Get retrieves a config value by dot-notation key.
// Refs: FR-13
func (s *ConfigService) Get(key string) (any, error) {
	// Marshal config to map for dot-notation access
	data, err := json.Marshal(s.config)
	if err != nil {
		return nil, fmt.Errorf("marshal config: %w", err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("unmarshal config: %w", err)
	}

	parts := strings.Split(key, ".")
	var current any = m
	for _, part := range parts {
		cm, ok := current.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("config key not found: %s", key)
		}
		val, exists := cm[part]
		if !exists {
			return nil, fmt.Errorf("config key not found: %s", key)
		}
		current = val
	}

	return current, nil
}

// Set updates a config value by dot-notation key.
// Refs: FR-13
func (s *ConfigService) Set(key string, value any) error {
	data, err := json.Marshal(s.config)
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		return fmt.Errorf("unmarshal config: %w", err)
	}

	parts := strings.Split(key, ".")
	setNestedValue(m, parts, value)

	data, err = json.Marshal(m)
	if err != nil {
		return fmt.Errorf("marshal updated config: %w", err)
	}
	if err := json.Unmarshal(data, &s.config); err != nil {
		return fmt.Errorf("apply updated config: %w", err)
	}

	return nil
}

// GetAll returns the full config as a map.
func (s *ConfigService) GetAll() Config {
	return s.config
}

// Save persists the config to disk.
// Refs: FR-13
func (s *ConfigService) Save() error {
	data, err := json.MarshalIndent(s.config, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	if err := os.WriteFile(s.configPath, data, 0o600); err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	return nil
}

// setNestedValue sets a value in a nested map by path parts.
func setNestedValue(m map[string]any, parts []string, value any) {
	if len(parts) == 1 {
		m[parts[0]] = value
		return
	}
	sub, ok := m[parts[0]].(map[string]any)
	if !ok {
		sub = make(map[string]any)
		m[parts[0]] = sub
	}
	setNestedValue(sub, parts[1:], value)
}
