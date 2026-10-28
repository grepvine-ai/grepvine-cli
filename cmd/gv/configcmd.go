package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net/url"
	"reflect"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

// configKey is one setting of the configuration file, as its config field
// and tags describe it. Adding a setting means adding a field to config.
type configKey struct {
	name   string
	env    []string
	secret bool
	index  int
}

func (k configKey) field(cfg *config) reflect.Value {
	return reflect.ValueOf(cfg).Elem().Field(k.index)
}

var configKeys = func() []configKey {
	fields := reflect.TypeFor[config]()
	keys := make([]configKey, fields.NumField())
	for i := range keys {
		field := fields.Field(i)
		name, _, _ := strings.Cut(field.Tag.Get("yaml"), ",")
		keys[i] = configKey{
			name:   name,
			env:    strings.Split(field.Tag.Get("env"), ","),
			secret: field.Tag.Get("secret") == "true",
			index:  i,
		}
	}
	return keys
}()

var configValidators = map[string]func(string) error{
	"endpoint": validateEndpoint,
}

var configNormalizers = map[string]func(string) string{
	"endpoint": func(value string) string { return strings.TrimRight(value, "/") },
}

// validate reports whether value may be saved for k.
func (k configKey) validate(value string) error {
	if validate := configValidators[k.name]; validate != nil {
		return validate(value)
	}
	return nil
}

// normalize cleans a value from any source before gv decides whether it is
// set, so an endpoint of "/" counts as unset.
func (k configKey) normalize(value string) string {
	if normalize := configNormalizers[k.name]; normalize != nil {
		return normalize(value)
	}
	return value
}

// display never shows a secret in full: only enough of its start to tell
// keys apart.
func (k configKey) display(value string) string {
	if !k.secret || value == "" {
		return value
	}
	if len(value) <= 16 {
		return "****"
	}
	return value[:8] + "****"
}

func lookupConfigKey(name string) (configKey, error) {
	names := make([]string, len(configKeys))
	for i, key := range configKeys {
		if key.name == name {
			return key, nil
		}
		names[i] = key.name
	}
	return configKey{}, fmt.Errorf("unknown configuration key %q; use one of: %s", name, strings.Join(names, ", "))
}

func validateEndpoint(value string) error {
	parsed, err := url.Parse(value)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return fmt.Errorf("endpoint %q must be an http or https URL, such as %s", value, defaultEndpoint)
	}
	return nil
}

// configValue returns the value gv uses for key and where it comes from: an
// environment variable, the configuration file, or the default.
func (a *app) configValue(key configKey, cfg config) (value, source string) {
	for _, name := range key.env {
		if value := key.normalize(a.getenv(name)); value != "" {
			return value, name
		}
	}
	if value := key.normalize(key.field(&cfg).String()); value != "" {
		return value, "config file"
	}
	defaults := configDefaults
	if value := key.normalize(key.field(&defaults).String()); value != "" {
		return value, "default"
	}
	return "", ""
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
				value, source := a.configValue(key, cfg)
				value = key.display(value)
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
			key, err := lookupConfigKey(args[0])
			if err != nil {
				return err
			}
			cfg, _, err := a.loadConfig()
			if err != nil {
				return err
			}
			value, source := a.configValue(key, cfg)
			if value == "" {
				return &exitError{code: 1, err: fmt.Errorf("%s is not set; set it with gv config set %s VALUE", key.name, key.name)}
			}
			if source != "config file" && source != "default" {
				fmt.Fprintf(a.stderr, "From %s.\n", source)
			}
			fmt.Fprintln(a.stdout, key.display(value))
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
			key, err := lookupConfigKey(args[0])
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
			if err := key.validate(value); err != nil {
				return err
			}
			cfg, path, err := a.loadConfig()
			if err != nil {
				return err
			}
			key.field(&cfg).SetString(value)
			if err := saveConfig(path, cfg); err != nil {
				return err
			}
			a.warnIfEnvironmentOverrides(key)
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
			key, err := lookupConfigKey(args[0])
			if err != nil {
				return err
			}
			cfg, path, err := a.loadConfig()
			if err != nil {
				return err
			}
			key.field(&cfg).SetString("")
			if err := saveConfig(path, cfg); err != nil {
				return err
			}
			a.warnIfEnvironmentOverrides(key)
			return nil
		},
	}
}

// warnIfEnvironmentOverrides tells the user when a change to the file has no
// effect because an environment variable takes precedence.
func (a *app) warnIfEnvironmentOverrides(key configKey) {
	for _, name := range key.env {
		if key.normalize(a.getenv(name)) != "" {
			fmt.Fprintf(a.stderr, "Note: %s is set and takes precedence over %s in the configuration file.\n", name, key.name)
			return
		}
	}
}
