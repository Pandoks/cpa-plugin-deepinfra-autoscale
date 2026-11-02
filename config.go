package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"
)

// pluginConfig is the plugin's own config block under plugins.configs.deepinfra-autoscale,
// delivered as base64 YAML in every plugin.register / plugin.reconfigure call.
type pluginConfig struct {
	IdleMinutes      int    `yaml:"idle-minutes"`
	MaxUptimeMinutes int    `yaml:"max-uptime-minutes"`
	StateFile        string `yaml:"state-file"`
}

const (
	defaultIdleMinutes      = 30
	defaultMaxUptimeMinutes = 720 // 12h backstop
)

var (
	configMu    sync.RWMutex
	configCache pluginConfig
)

// applyConfigYAML parses the delivered YAML config block and replaces the cache. Missing
// values fall back to defaults. Returns the parsed config.
func applyConfigYAML(yamlBytes []byte) pluginConfig {
	var cfg pluginConfig
	if len(yamlBytes) > 0 {
		_ = yaml.Unmarshal(yamlBytes, &cfg)
	}
	if cfg.IdleMinutes <= 0 {
		cfg.IdleMinutes = defaultIdleMinutes
	}
	if cfg.MaxUptimeMinutes <= 0 {
		cfg.MaxUptimeMinutes = defaultMaxUptimeMinutes
	}
	if cfg.StateFile == "" {
		cfg.StateFile = defaultStateFile()
	}

	configMu.Lock()
	configCache = cfg
	configMu.Unlock()
	return cfg
}

func getConfig() pluginConfig {
	configMu.RLock()
	defer configMu.RUnlock()
	return configCache
}

// The DeepInfra API key is entered on the plugin page and kept in its own 0600 file next to the
// state file, not in config.yaml: CPA config fields render as plain text.
var (
	keyMu   sync.RWMutex
	keyPath string
	keyVal  string
)

func apiKey() string {
	keyMu.RLock()
	defer keyMu.RUnlock()
	return keyVal
}

// loadAPIKey reads the key file beside stateFile. A missing file means no key yet.
func loadAPIKey(stateFile string) {
	path := filepath.Join(filepath.Dir(stateFile), "deepinfra-autoscale.key")
	data, _ := os.ReadFile(path)
	keyMu.Lock()
	keyPath, keyVal = path, strings.TrimSpace(string(data))
	keyMu.Unlock()
}

// saveAPIKey persists a new key atomically and makes it live.
func saveAPIKey(key string) error {
	keyMu.Lock()
	defer keyMu.Unlock()
	if err := os.MkdirAll(filepath.Dir(keyPath), 0o755); err != nil {
		return fmt.Errorf("mkdir key dir: %w", err)
	}
	tmp := keyPath + ".tmp"
	if err := os.WriteFile(tmp, []byte(key+"\n"), 0o600); err != nil {
		return fmt.Errorf("write key: %w", err)
	}
	if err := os.Rename(tmp, keyPath); err != nil {
		return fmt.Errorf("rename key: %w", err)
	}
	keyVal = key
	resetGPUCache()
	return nil
}

// defaultStateFile resolves the default path for the persisted model set.
func defaultStateFile() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "deepinfra-autoscale-state.json"
	}
	return filepath.Join(home, ".cli-proxy-api", "deepinfra-autoscale-state.json")
}
