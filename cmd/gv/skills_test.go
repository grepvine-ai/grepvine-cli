package main

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/grepvine-ai/grepvine-cli/skills"
)

func TestSkillsInstallLifecycle(t *testing.T) {
	a, stdout, stderr := testApp(t, "")
	home := t.TempDir()
	project := t.TempDir()
	if err := os.Mkdir(filepath.Join(project, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	work := filepath.Join(project, "src", "app")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	a.homeDir = func() (string, error) { return home, nil }
	a.workDir = func() (string, error) { return work, nil }
	a.version = "v1.2.0"
	execute := func(args ...string) error {
		t.Helper()
		stdout.Reset()
		stderr.Reset()
		command := newRootCommand(a)
		command.SetArgs(args)
		return command.Execute()
	}
	listed := func() map[string]map[string]any {
		t.Helper()
		if err := execute("skills", "ls", "--json"); err != nil {
			t.Fatal(err)
		}
		out := map[string]map[string]any{}
		for line := range strings.Lines(stdout.String()) {
			var record map[string]any
			if err := json.Unmarshal([]byte(line), &record); err != nil {
				t.Fatalf("%v: %q", err, line)
			}
			out[record["agent"].(string)+"/"+record["scope"].(string)] = record
		}
		return out
	}
	claudeUser := filepath.Join(home, ".claude", "skills", skills.Name)
	codexUser := filepath.Join(home, ".agents", "skills", skills.Name)
	claudeProject := filepath.Join(project, ".claude", "skills", skills.Name)

	if got := listed(); len(got) != 4 || got["claude/user"]["status"] != "absent" || got["codex/project"]["status"] != "absent" {
		t.Fatalf("before install: %v", got)
	}

	// The default installs for every agent in the home directory.
	if err := execute("skills", "install"); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{claudeUser, codexUser} {
		skill, err := os.ReadFile(filepath.Join(dir, "SKILL.md"))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(skill), "  gv-version: \"v1.2.0\"\n") {
			t.Fatalf("%s not stamped:\n%s", dir, skill)
		}
		if _, err := os.Stat(filepath.Join(dir, "references", "search.md")); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(claudeProject); !os.IsNotExist(err) {
		t.Fatalf("project install without --scope project: %v", err)
	}
	if got := listed(); got["claude/user"]["status"] != "current" || got["claude/user"]["version"] != "v1.2.0" || got["claude/project"]["status"] != "absent" {
		t.Fatalf("after install: %v", got)
	}
	entries, _ := os.ReadDir(filepath.Dir(claudeUser))
	if len(entries) != 1 {
		t.Fatalf("staging directory left behind: %v", entries)
	}

	if err := execute("skills", "install", "--agent", "claude"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "already current") {
		t.Fatalf("reinstall: %q", stdout.String())
	}

	// The project scope resolves to the git work tree, not the current
	// directory, and notes the copy at the other scope.
	if err := execute("skills", "install", "--agent", "claude", "--scope", "project"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(claudeProject, "SKILL.md")); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stderr.String(), "also has grepvine-cli at user scope") {
		t.Fatalf("no note about the user copy: %q", stderr.String())
	}

	// A newer gv updates an unchanged copy.
	a.version = "v1.3.0"
	if got := listed(); got["claude/user"]["status"] != "outdated" || got["claude/user"]["version"] != "v1.2.0" {
		t.Fatalf("after upgrade: %v", got)
	}
	if err := execute("skills", "install", "--agent", "claude"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "Updated grepvine-cli v1.3.0") {
		t.Fatalf("update: %q", stdout.String())
	}

	// A changed copy is kept unless --force, and the other locations are
	// still installed.
	edited := filepath.Join(claudeUser, "references", "search.md")
	if err := os.WriteFile(edited, []byte("my notes\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := listed(); got["claude/user"]["status"] != "modified" {
		t.Fatalf("after edit: %v", got)
	}
	err := execute("skills", "install")
	if err == nil || !strings.Contains(err.Error(), "was changed since gv installed it; pass --force") {
		t.Fatalf("install over an edit: %v", err)
	}
	if data, _ := os.ReadFile(edited); string(data) != "my notes\n" {
		t.Fatalf("edit overwritten: %q", data)
	}
	if got := listed(); got["codex/user"]["status"] != "current" {
		t.Fatalf("codex not updated alongside the refusal: %v", got)
	}
	if err := execute("skills", "uninstall", "--agent", "claude"); err == nil || !strings.Contains(err.Error(), "pass --force to remove it") {
		t.Fatalf("uninstall an edited copy: %v", err)
	}
	if err := execute("skills", "install", "--agent", "claude", "--force"); err != nil {
		t.Fatal(err)
	}
	if got := listed(); got["claude/user"]["status"] != "current" {
		t.Fatalf("after --force: %v", got)
	}

	// An added file counts as a change too.
	if err := os.WriteFile(filepath.Join(claudeUser, "extra.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := listed(); got["claude/user"]["status"] != "modified" {
		t.Fatalf("after adding a file: %v", got)
	}

	if err := execute("skills", "uninstall", "--agent", "claude", "--force"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(claudeUser); !os.IsNotExist(err) {
		t.Fatalf("not removed: %v", err)
	}
	if err := execute("skills", "uninstall", "--agent", "claude"); err != nil || !strings.Contains(stdout.String(), "is not installed") {
		t.Fatalf("uninstall twice: %v %q", err, stdout.String())
	}

	// A skill of the same name that gv did not install is never removed, and
	// replaced only with --force.
	if err := os.MkdirAll(claudeUser, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(claudeUser, "SKILL.md"), []byte("---\nname: grepvine-cli\ndescription: mine\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := listed(); got["claude/user"]["status"] != "foreign" {
		t.Fatalf("foreign: %v", got)
	}
	if err := execute("skills", "install", "--agent", "claude"); err == nil || !strings.Contains(err.Error(), "was not installed by gv") {
		t.Fatalf("install over a foreign skill: %v", err)
	}
	if err := execute("skills", "uninstall", "--agent", "claude", "--force"); err == nil || !strings.Contains(err.Error(), "remove it yourself") {
		t.Fatalf("uninstall a foreign skill: %v", err)
	}
	if err := execute("skills", "install", "--agent", "claude", "--force"); err != nil {
		t.Fatal(err)
	}

	// --dir takes any skills directory.
	custom := filepath.Join(t.TempDir(), "skills")
	if err := execute("skills", "install", "--dir", custom); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(custom, skills.Name, "SKILL.md")); err != nil {
		t.Fatal(err)
	}
	if err := execute("skills", "ls", "--dir", custom); err != nil || !strings.Contains(stdout.String(), "installed, current (v1.3.0)") {
		t.Fatalf("ls --dir: %v %q", err, stdout.String())
	}

	for _, args := range [][]string{
		{"skills", "install", "--agent", "cursor"},
		{"skills", "install", "--scope", "global"},
	} {
		if err := execute(args...); err == nil || !strings.Contains(err.Error(), "; use ") {
			t.Fatalf("%v: %v", args, err)
		}
	}
}

func TestSkillsProjectScopeOutsideGit(t *testing.T) {
	a, _, _ := testApp(t, "")
	work := t.TempDir()
	a.workDir = func() (string, error) { return work, nil }
	root, err := a.skillScopeRoot("project")
	if err != nil {
		t.Fatal(err)
	}
	// t.TempDir is not inside a git work tree on CI, but a developer's
	// temporary directory might be; either way the root contains work.
	if rel, err := filepath.Rel(root, work); err != nil || strings.HasPrefix(rel, "..") {
		t.Fatalf("root %s does not contain %s", root, work)
	}
}

func TestSkillsShow(t *testing.T) {
	a, stdout, _ := testApp(t, "")
	command := newRootCommand(a)
	command.SetArgs([]string{"skills", "show"})
	if err := command.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(stdout.String(), "---\nname: grepvine-cli\n") || strings.Contains(stdout.String(), "gv-checksum") {
		t.Fatalf("show:\n%s", stdout.String())
	}
	stdout.Reset()
	command = newRootCommand(a)
	command.SetArgs([]string{"skills", "show", "references/search.md"})
	if err := command.Execute(); err != nil || !strings.HasPrefix(stdout.String(), "# Searching with gv") {
		t.Fatalf("show reference: %v %q", err, stdout.String())
	}
	command = newRootCommand(a)
	command.SetArgs([]string{"skills", "show", "../go.mod"})
	if err := command.Execute(); err == nil || !strings.Contains(err.Error(), "references/search.md") {
		t.Fatalf("show unknown file: %v", err)
	}
}

func TestSkillStamp(t *testing.T) {
	source := []byte("---\nname: x\ndescription: y\n---\n\n# Body\n---\n")
	stamped, err := stampSkill(source, "v1.0.0", "sha256:abc")
	if err != nil {
		t.Fatal(err)
	}
	want := "---\nname: x\ndescription: y\nmetadata:\n  gv-version: \"v1.0.0\"\n  gv-checksum: \"sha256:abc\"\n---\n\n# Body\n---\n"
	if string(stamped) != want {
		t.Fatalf("stamped:\n%s", stamped)
	}
	back, version, checksum, ok := unstampSkill(stamped)
	if !ok || string(back) != string(source) || version != "v1.0.0" || checksum != "sha256:abc" {
		t.Fatalf("unstamp: %v %q %q %q", ok, back, version, checksum)
	}
	for _, skill := range []string{
		string(source),
		"no frontmatter",
		// A stamp anywhere but at the end of the frontmatter is not gv's.
		"---\nname: x\nmetadata:\n  gv-version: \"v1\"\n  gv-checksum: \"s\"\nother: y\n---\n",
		"---\nname: x\n---\nmetadata:\n  gv-version: \"v1\"\n  gv-checksum: \"s\"\n---\n",
	} {
		if _, _, _, ok := unstampSkill([]byte(skill)); ok {
			t.Fatalf("unstamped %q", skill)
		}
	}
}

// TestSkillContent checks the skill against what agents require of one and
// that it links every reference file it ships.
func TestSkillContent(t *testing.T) {
	files := skillSources(t)
	skill := files["SKILL.md"]
	end := frontmatterEnd([]byte(skill))
	if end < 0 {
		t.Fatal("SKILL.md has no frontmatter")
	}
	fields := map[string]string{}
	for line := range strings.Lines(skill[4:end]) {
		key, value, ok := strings.Cut(strings.TrimSuffix(line, "\n"), ": ")
		if !ok || strings.HasPrefix(key, " ") {
			t.Fatalf("frontmatter line %q: keep each field on one line", line)
		}
		fields[key] = value
	}
	if fields["name"] != skills.Name || !regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`).MatchString(fields["name"]) || len(fields["name"]) > 64 {
		t.Fatalf("name %q", fields["name"])
	}
	if description := fields["description"]; description == "" || len(description) > 1024 || strings.ContainsAny(description, "<>") {
		t.Fatalf("description must be 1 to 1024 characters without angle brackets: %q", description)
	}
	if _, ok := fields["metadata"]; ok || len(fields) != 2 {
		t.Fatalf("frontmatter holds %v; gv adds metadata itself", fields)
	}

	linked := map[string]bool{}
	for name, text := range files {
		for _, match := range regexp.MustCompile(`\]\(([^)#]+)\)`).FindAllStringSubmatch(text, -1) {
			target := match[1]
			if strings.HasPrefix(target, "https://") {
				continue
			}
			target = filepath.ToSlash(filepath.Join(filepath.Dir(name), target))
			if _, ok := files[target]; !ok {
				t.Errorf("%s links to %s, which the skill does not have", name, match[1])
			}
			linked[target] = true
		}
	}
	for name := range files {
		if name != "SKILL.md" && !linked[name] {
			t.Errorf("SKILL.md does not lead to %s", name)
		}
	}
}

// TestSkillCommandsExist runs every gv command line the skill shows through
// the command tree, so a renamed command or flag fails here instead of
// misleading an agent.
func TestSkillCommandsExist(t *testing.T) {
	a, _, _ := testApp(t, "")
	root := newRootCommand(a)
	inline := regexp.MustCompile("`(gv [^`]+)`")
	checked := 0
	for name, text := range skillSources(t) {
		var lines []string
		inBlock := false
		for line := range strings.Lines(text) {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "```") {
				inBlock = !inBlock
				continue
			}
			if inBlock && strings.HasPrefix(line, "gv ") {
				lines = append(lines, line)
				continue
			}
			for _, match := range inline.FindAllStringSubmatch(line, -1) {
				lines = append(lines, match[1])
			}
		}
		for _, line := range lines {
			if err := checkSkillCommand(root, line); err != nil {
				t.Errorf("%s: %s: %v", name, line, err)
			}
			checked++
		}
	}
	if checked < 40 {
		t.Fatalf("checked only %d command lines", checked)
	}
}

func checkSkillCommand(root *cobra.Command, line string) error {
	words := shellWords(line)
	command := root
	i := 1
	for ; i < len(words); i++ {
		word := words[i]
		if strings.HasPrefix(word, "-") || strings.HasPrefix(word, "<") {
			break
		}
		next := findSubcommand(command, word)
		if next == nil {
			if command.HasSubCommands() && !command.Runnable() {
				return fmt.Errorf("%q is not a subcommand of %q", word, command.CommandPath())
			}
			break
		}
		command = next
	}
	for ; i < len(words); i++ {
		word := words[i]
		if word == "--" {
			break
		}
		if word == "-" || !strings.HasPrefix(word, "-") {
			continue
		}
		var found bool
		if long, ok := strings.CutPrefix(word, "--"); ok {
			long, _, _ = strings.Cut(long, "=")
			found = long == "help" || command.Flags().Lookup(long) != nil || command.InheritedFlags().Lookup(long) != nil
		} else {
			short := word[1:2]
			found = command.Flags().ShorthandLookup(short) != nil || command.InheritedFlags().ShorthandLookup(short) != nil
		}
		if !found {
			return fmt.Errorf("%q has no flag %s", command.CommandPath(), word)
		}
	}
	return nil
}

func findSubcommand(command *cobra.Command, name string) *cobra.Command {
	for _, sub := range command.Commands() {
		if sub.Name() == name || slices.Contains(sub.Aliases, name) {
			return sub
		}
	}
	return nil
}

// shellWords splits a command line the way a shell would for these
// examples: quotes group words, and a comment, pipe or redirect ends it.
func shellWords(line string) []string {
	var words []string
	var word strings.Builder
	inWord := false
	var quote rune
	for _, r := range line {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				word.WriteRune(r)
			}
		case r == '\'' || r == '"':
			quote, inWord = r, true
		case r == ' ' || r == '\t':
			if inWord {
				words = append(words, word.String())
				word.Reset()
				inWord = false
			}
		case (r == '#' || r == '|' || r == '>') && !inWord:
			return words
		default:
			word.WriteRune(r)
			inWord = true
		}
	}
	if inWord {
		words = append(words, word.String())
	}
	return words
}

func skillSources(t *testing.T) map[string]string {
	t.Helper()
	files := map[string]string{}
	err := fs.WalkDir(skills.Files(), ".", func(name string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		data, err := fs.ReadFile(skills.Files(), name)
		files[name] = string(data)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}
