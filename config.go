package main

import (
	"os"
	"path/filepath"
	"sync"

	"gopkg.in/yaml.v3"
)

// pluginConfig is the plugin's own config block under plugins.configs.deepinfra-autoscale,
// delivered as base64 YAML in every plugin.register / plugin.reconfigure call.
type pluginConfig struct {
	APIKey           string `yaml:"api-key"`
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

func apiKey() string {
	configMu.RLock()
	defer configMu.RUnlock()
	return configCache.APIKey
}

// defaultStateFile resolves the default path for the persisted model set.
func defaultStateFile() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "deepinfra-autoscale-state.json"
	}
	return filepath.Join(home, ".cli-proxy-api", "deepinfra-autoscale-state.json")
}
