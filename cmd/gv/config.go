package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v2"
)

// config is the configuration file. Each field is a setting: yaml names
// it, env lists the environment variables that override it, first match
// wins, and secret keeps gv config from printing it in full.
type config struct {
	// GREPVINE_ENDPOINT is the shorter pre-beta spelling, kept as an alias.
	Endpoint     string `yaml:"endpoint,omitempty" env:"GREPVINE_API_ENDPOINT,GREPVINE_ENDPOINT"`
	APIKey       string `yaml:"api_key,omitempty" env:"GREPVINE_API_KEY" secret:"true"`
	Organization string `yaml:"organization,omitempty" env:"GREPVINE_ORGANIZATION"`
}

// configDefaults holds the values used when neither the environment nor the
// file sets a setting.
var configDefaults = config{Endpoint: defaultEndpoint}

func (a *app) resolvedConfigPath() (string, error) {
	if a.configPath != "" {
		return a.configPath, nil
	}
	if path := a.getenv("GREPVINE_CONFIG"); path != "" {
		return path, nil
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
	var resolved config
	for _, key := range configKeys {
		value, _ := a.resolve(key, cfg)
		key.field(&resolved).SetString(value)
	}
	return resolved, path, nil
}
