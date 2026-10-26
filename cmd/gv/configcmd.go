package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

// configKey is one setting that gv config can show and change. Adding a
// setting to the configuration file means adding an entry to configKeys.
type configKey struct {
	name string
	// env lists the environment variables that override the file, first
	// match wins.
	env          []string
	defaultValue string
	secret       bool
	field        func(*config) *string
	check        func(string) error
}

var configKeys = []configKey{
	{
		name:         "endpoint",
		env:          []string{"GREPVINE_API_ENDPOINT", "GREPVINE_ENDPOINT"},
		defaultValue: defaultEndpoint,
		field:        func(cfg *config) *string { return &cfg.Endpoint },
		check:        checkEndpoint,
	},
	{
		name:   "api_key",
		env:    []string{"GREPVINE_API_KEY"},
		secret: true,
		field:  func(cfg *config) *string { return &cfg.APIKey },
	},
	{
		name:  "organization",
		env:   []string{"GREPVINE_ORGANIZATION"},
		field: func(cfg *config) *string { return &cfg.Organization },
	},
}

func findConfigKey(name string) (configKey, error) {
	names := make([]string, len(configKeys))
	for i, key := range configKeys {
		if key.name == name {
			return key, nil
		}
		names[i] = key.name
	}
	return configKey{}, fmt.Errorf("unknown configuration key %q; use one of: %s", name, strings.Join(names, ", "))
}

func checkEndpoint(value string) error {
	parsed, err := url.Parse(value)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return fmt.Errorf("endpoint %q must be an http or https URL, such as %s", value, defaultEndpoint)
	}
	return nil
}

// resolve returns the value gv uses for key and where it comes from: an
// environment variable, the configuration file, or the default.
func (a *app) resolve(key configKey, cfg config) (value, source string) {
	for _, name := range key.env {
		if value := a.getenv(name); value != "" {
			return value, name
		}
	}
	if value := *key.field(&cfg); value != "" {
		return value, "config file"
	}
	if key.defaultValue != "" {
		return key.defaultValue, "default"
	}
	return "", ""
}

// displayValue never shows a secret in full: only enough of its start to
// tell keys apart.
func displayValue(key configKey, value string) string {
	if !key.secret || value == "" {
		return value
	}
	if len(value) <= 16 {
		return "****"
	}
	return value[:8] + "****"
}

func completeConfigKey(_ *cobra.Command, args []string, _ string) ([]string, cobra.ShellCompDirective) {
	if len(args) > 0 {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	names := make([]string, len(configKeys))
	for i, key := range configKeys {
		names[i] = key.name
	}
	return names, cobra.ShellCompDirectiveNoFileComp
}

func newConfigCommand(a *app) *cobra.Command {
	command := &cobra.Command{
		Use:   "config",
		Short: "Show and change the configuration file",
		Long: `Show and change the configuration file: the file passed with --config, else
$GREPVINE_CONFIG, else ~/.grepvine/config.yaml. Environment variables override
the settings in the file.`,
	}
	command.AddCommand(
		newConfigListCommand(a),
		newConfigGetCommand(a),
		newConfigSetCommand(a),
		newConfigUnsetCommand(a),
	)
	return command
}

func newConfigListCommand(a *app) *cobra.Command {
	return &cobra.Command{
		Use:     "ls",
		Aliases: []string{"list"},
		Short:   "Show every setting, its value and where the value comes from",
		Args:    cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			cfg, _, err := a.loadConfig()
			if err != nil {
				return err
			}
			table := tabwriter.NewWriter(a.stdout, 0, 8, 2, ' ', 0)
			fmt.Fprintln(table, "KEY\tVALUE\tSOURCE")
			for _, key := range configKeys {
				value, source := a.resolve(key, cfg)
				value = displayValue(key, value)
				if value == "" {
					value, source = "-", "-"
				}
				fmt.Fprintf(table, "%s\t%s\t%s\n", key.name, value, source)
			}
			return table.Flush()
		},
	}
}

func newConfigGetCommand(a *app) *cobra.Command {
	return &cobra.Command{
		Use:               "get KEY",
		Short:             "Print the value gv uses for a setting",
		Long:              "Print the value gv uses for a setting. An API key is shown only in part.",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: completeConfigKey,
		RunE: func(_ *cobra.Command, args []string) error {
			key, err := findConfigKey(args[0])
			if err != nil {
				return err
			}
			cfg, _, err := a.loadConfig()
			if err != nil {
				return err
			}
			value, source := a.resolve(key, cfg)
			if value == "" {
				return &exitError{code: 1, err: fmt.Errorf("%s is not set; set it with gv config set %s VALUE", key.name, key.name)}
			}
			if source != "config file" && source != "default" {
				fmt.Fprintf(a.stderr, "From %s.\n", source)
			}
			fmt.Fprintln(a.stdout, displayValue(key, value))
			return nil
		},
	}
}

func newConfigSetCommand(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "set KEY VALUE",
		Short: "Save a setting in the configuration file",
		Long: `Save a setting in the configuration file. Pass - as VALUE to read the
value from standard input, which keeps an API key out of your shell history.`,
		Args:              cobra.ExactArgs(2),
		ValidArgsFunction: completeConfigKey,
		RunE: func(_ *cobra.Command, args []string) error {
			key, err := findConfigKey(args[0])
			if err != nil {
				return err
			}
			value := args[1]
			if value == "-" {
				line, err := bufio.NewReader(a.stdin).ReadString('\n')
				if err != nil && !errors.Is(err, io.EOF) {
					return fmt.Errorf("read %s from standard input: %w", key.name, err)
				}
				value = strings.TrimSpace(line)
			}
			if value == "" {
				return fmt.Errorf("%s needs a value; to remove it, run gv config unset %s", key.name, key.name)
			}
			if key.check != nil {
				if err := key.check(value); err != nil {
					return err
				}
			}
			cfg, path, err := a.loadConfig()
			if err != nil {
				return err
			}
			*key.field(&cfg) = value
			if err := saveConfig(path, cfg); err != nil {
				return err
			}
			a.warnOverridden(key)
			return nil
		},
	}
}

func newConfigUnsetCommand(a *app) *cobra.Command {
	return &cobra.Command{
		Use:               "unset KEY",
		Short:             "Remove a setting from the configuration file",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: completeConfigKey,
		RunE: func(_ *cobra.Command, args []string) error {
			key, err := findConfigKey(args[0])
			if err != nil {
				return err
			}
			cfg, path, err := a.loadConfig()
			if err != nil {
				return err
			}
			*key.field(&cfg) = ""
			if err := saveConfig(path, cfg); err != nil {
				return err
			}
			a.warnOverridden(key)
			return nil
		},
	}
}

// warnOverridden tells the user when a change to the file has no effect
// because an environment variable takes precedence.
func (a *app) warnOverridden(key configKey) {
	for _, name := range key.env {
		if a.getenv(name) != "" {
			fmt.Fprintf(a.stderr, "Note: %s is set and takes precedence over %s in the configuration file.\n", name, key.name)
			return
		}
	}
}
