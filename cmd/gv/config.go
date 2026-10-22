package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v2"
)

type config struct {
	Endpoint string `yaml:"endpoint,omitempty"`
	APIKey   string `yaml:"api_key,omitempty"`
}

func (a *app) resolvedConfigPath() (string, error) {
	if a.configPath != "" {
		return a.configPath, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("find home directory: %w", err)
	}
	return filepath.Join(home, ".grepvine", "config.yaml"), nil
}

func (a *app) loadConfig() (config, string, error) {
	path, err := a.resolvedConfigPath()
	if err != nil {
		return config{}, "", err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return config{}, path, nil
	}
	if err != nil {
		return config{}, path, fmt.Errorf("read configuration: %w", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		return config{}, path, fmt.Errorf("inspect configuration: %w", err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return config{}, path, fmt.Errorf("configuration %s must have mode 0600", path)
	}
	var cfg config
	if err := yaml.UnmarshalStrict(data, &cfg); err != nil {
		return config{}, path, fmt.Errorf("parse configuration: %w", err)
	}
	return cfg, path, nil
}

func saveConfig(path string, cfg config) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create configuration directory: %w", err)
	}
	data, err := yaml.Marshal(cfg) //nolint:gosec // the CLI stores its API key in its own 0600 configuration file by design
	if err != nil {
		return fmt.Errorf("encode configuration: %w", err)
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".config-*")
	if err != nil {
		return fmt.Errorf("create configuration: %w", err)
	}
	temporaryPath := temporary.Name()
	keep := false
	defer func() {
		_ = temporary.Close()
		if !keep {
			_ = os.Remove(temporaryPath)
		}
	}()
	if err = temporary.Chmod(0o600); err != nil {
		return fmt.Errorf("secure configuration: %w", err)
	}
	if _, err = temporary.Write(data); err != nil {
		return fmt.Errorf("write configuration: %w", err)
	}
	if err = temporary.Sync(); err != nil {
		return fmt.Errorf("sync configuration: %w", err)
	}
	if err = temporary.Close(); err != nil {
		return fmt.Errorf("close configuration: %w", err)
	}
	if err = os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("publish configuration: %w", err)
	}
	keep = true
	return nil
}

func (a *app) endpointAndKey() (config, string, error) {
	cfg, path, err := a.loadConfig()
	if err != nil {
		return config{}, path, err
	}
	endpoint := strings.TrimRight(a.getenv("GREPVINE_API_ENDPOINT"), "/")
	if endpoint == "" {
		// Keep the shorter pre-beta spelling as a compatibility alias.
		endpoint = strings.TrimRight(a.getenv("GREPVINE_ENDPOINT"), "/")
	}
	if endpoint == "" {
		endpoint = strings.TrimRight(cfg.Endpoint, "/")
	}
	if endpoint == "" {
		endpoint = defaultEndpoint
	}
	key := a.getenv("GREPVINE_API_KEY")
	if key == "" {
		key = cfg.APIKey
	}
	return config{Endpoint: endpoint, APIKey: key}, path, nil
}
