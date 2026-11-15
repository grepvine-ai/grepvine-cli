// Copyright 2026 Treeverse, Inc.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"go.yaml.in/yaml/v3"
)

// config is the configuration file. Each field is a setting: yaml names
// it, env lists the environment variables that override it, first match
// wins, and secret keeps gv config from printing it in full.
type config struct {
	Endpoint     string `yaml:"endpoint,omitempty" env:"GREPVINE_API_ENDPOINT"`
	APIKey       string `yaml:"api_key,omitempty" env:"GREPVINE_API_KEY" secret:"true"`
	AgentSecret  string `yaml:"agent_secret,omitempty" env:"GREPVINE_AGENT_SECRET" secret:"true"`
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
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	// An empty file is a valid, empty configuration.
	if err := decoder.Decode(&cfg); err != nil && !errors.Is(err, io.EOF) {
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

// endpointAndKey resolves every setting. At most one of the API key and the
// agent secret is left set: the one from the environment wins over the one
// from the file, and two from the same place are refused, since they act as
// different identities.
func (a *app) endpointAndKey() (config, string, error) {
	cfg, path, err := a.loadConfig()
	if err != nil {
		return config{}, path, err
	}
	var resolved config
	sources := map[string]string{}
	for _, key := range configKeys {
		value, source := a.configValue(key, cfg)
		key.field(&resolved).SetString(value)
		sources[key.name] = source
	}
	if resolved.APIKey == "" || resolved.AgentSecret == "" {
		return resolved, path, nil
	}
	keyFromFile, secretFromFile := sources["api_key"] == "config file", sources["agent_secret"] == "config file"
	switch {
	case keyFromFile && !secretFromFile:
		resolved.APIKey = ""
	case secretFromFile && !keyFromFile:
		resolved.AgentSecret = ""
	case keyFromFile:
		return config{}, path, fmt.Errorf("both an API key and an agent secret are set in %s; keep one:\n  gv config unset agent_secret   (act as yourself)\n  gv config unset api_key        (act as the agent)", path)
	default:
		return config{}, path, fmt.Errorf("both %s and %s are set; unset one", sources["api_key"], sources["agent_secret"])
	}
	return resolved, path, nil
}
