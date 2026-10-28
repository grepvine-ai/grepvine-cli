package main

import (
	"strings"
	"testing"
)

func TestConfigCommand(t *testing.T) {
	a, stdout, stderr := testApp(t, "")
	env := map[string]string{}
	a.getenv = func(name string) string { return env[name] }
	execute := func(stdin string, args ...string) error {
		t.Helper()
		stdout.Reset()
		stderr.Reset()
		a.stdin = strings.NewReader(stdin)
		command := newRootCommand(a)
		command.SetArgs(args)
		return command.Execute()
	}
	// listed is the output of gv config ls with each row's columns one space apart.
	listed := func() string {
		var rows strings.Builder
		for line := range strings.Lines(stdout.String()) {
			rows.WriteString(strings.Join(strings.Fields(line), " ") + "\n")
		}
		return rows.String()
	}

	if err := execute("", "config", "ls"); err != nil {
		t.Fatal(err)
	}
	if got := listed(); !strings.Contains(got, "endpoint https://api.grepvine.ai default\n") || !strings.Contains(got, "api_key - -\n") || !strings.Contains(got, "organization - -\n") {
		t.Fatalf("empty list:\n%s", got)
	}
	if err := execute("", "config", "get", "api_key"); err == nil || !strings.Contains(err.Error(), "gv config set api_key") {
		t.Fatalf("get unset key: %v", err)
	}

	if err := execute("gv_secret_0123456789abcdef\n", "config", "set", "api_key", "-"); err != nil {
		t.Fatal(err)
	}
	if err := execute("", "config", "set", "endpoint", "https://config.example/v1"); err != nil {
		t.Fatal(err)
	}
	cfg, _, err := a.loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.APIKey != "gv_secret_0123456789abcdef" || cfg.Endpoint != "https://config.example/v1" {
		t.Fatalf("saved %+v", cfg)
	}
	if err := execute("", "config", "get", "api_key"); err != nil {
		t.Fatal(err)
	}
	if got := stdout.String(); got != "gv_secre****\n" {
		t.Fatalf("api_key shown as %q", got)
	}

	env["GREPVINE_API_ENDPOINT"] = "https://environment.example"
	if err := execute("", "config", "ls"); err != nil {
		t.Fatal(err)
	}
	if got := listed(); !strings.Contains(got, "endpoint https://environment.example GREPVINE_API_ENDPOINT\n") || !strings.Contains(got, "api_key gv_secre**** config file\n") || strings.Contains(got, "0123456789") {
		t.Fatalf("list:\n%s", got)
	}
	if err := execute("", "config", "set", "endpoint", "https://other.example"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stderr.String(), "GREPVINE_API_ENDPOINT is set") {
		t.Fatalf("no override note: %q", stderr.String())
	}

	if err := execute("", "config", "set", "organization", "example"); err != nil {
		t.Fatal(err)
	}
	env["GREPVINE_ORGANIZATION"] = "other"
	if err := execute("", "config", "get", "organization"); err != nil {
		t.Fatal(err)
	}
	if stdout.String() != "other\n" || !strings.Contains(stderr.String(), "GREPVINE_ORGANIZATION") {
		t.Fatalf("organization = %q, %q", stdout.String(), stderr.String())
	}

	if err := execute("", "config", "unset", "api_key"); err != nil {
		t.Fatal(err)
	}
	if cfg, _, _ = a.loadConfig(); cfg.APIKey != "" || cfg.Endpoint != "https://other.example" || cfg.Organization != "example" {
		t.Fatalf("after unset %+v", cfg)
	}

	for _, args := range [][]string{
		{"config", "get", "theme"},
		{"config", "set", "endpoint", "api.example"},
		{"config", "set", "api_key", ""},
	} {
		if err := execute("", args...); err == nil {
			t.Fatalf("%v succeeded", args)
		}
	}
}

func TestEndpointOfOnlySlashesFallsThrough(t *testing.T) {
	for _, test := range []struct {
		env  map[string]string
		file string
		want string
	}{
		{map[string]string{"GREPVINE_API_ENDPOINT": "/", "GREPVINE_ENDPOINT": "https://alias.example/"}, "https://file.example", "https://alias.example"},
		{map[string]string{"GREPVINE_API_ENDPOINT": "//"}, "https://file.example//", "https://file.example"},
		{map[string]string{"GREPVINE_API_ENDPOINT": "/"}, "/", defaultEndpoint},
	} {
		a, _, _ := testApp(t, "")
		a.getenv = func(name string) string { return test.env[name] }
		if err := saveConfig(a.configPath, config{Endpoint: test.file}); err != nil {
			t.Fatal(err)
		}
		resolved, _, err := a.endpointAndKey()
		if err != nil {
			t.Fatal(err)
		}
		if resolved.Endpoint != test.want {
			t.Errorf("env %v, file %q: endpoint %q, want %q", test.env, test.file, resolved.Endpoint, test.want)
		}
	}
}
