package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/grepvine-ai/grepvine-cli/skills"
)

const skillFile = "SKILL.md"

// skillAgent is a coding agent gv installs the skill for, and the directory
// it reads skills from, relative to the home directory for the user scope and
// to the project root for the project scope.
type skillAgent struct {
	name  string
	title string
	dir   string
}

var skillAgents = []skillAgent{
	{name: "claude", title: "Claude Code", dir: filepath.Join(".claude", "skills")},
	// Codex reads ~/.agents/skills and a repository's .agents/skills, as do
	// other agents that follow the same convention.
	{name: "codex", title: "Codex", dir: filepath.Join(".agents", "skills")},
}

var skillScopes = []string{"user", "project"}

// skillTarget is one place the skill is installed: its directory, and the
// agent and scope it serves. agent is nil for a directory given with --dir.
type skillTarget struct {
	agent *skillAgent
	scope string
	dir   string
}

func (t skillTarget) agentName() string {
	if t.agent == nil {
		return "-"
	}
	return t.agent.name
}

func (t skillTarget) scopeName() string {
	if t.scope == "" {
		return "-"
	}
	return t.scope
}

// describe names the target for messages: "Claude Code (user)" or a path.
func (t skillTarget) describe() string {
	if t.agent == nil {
		return t.dir
	}
	return fmt.Sprintf("%s (%s)", t.agent.title, t.scope)
}

type skillOptions struct {
	agent string
	scope string
	dir   string
	force bool
	json  bool
}

func newSkillsCommand(a *app) *cobra.Command {
	command := &cobra.Command{
		Use:   "skills",
		Short: "Install the gv skill for coding agents such as Claude Code and Codex",
		Long: `Install the gv skill for coding agents such as Claude Code and Codex.

The skill teaches an agent to search, read and update Grepvine indexes with gv.
It is part of this gv release, so reinstall it after you upgrade gv to keep it
in step with the commands it describes.

With --scope user, the default, the skill is installed in your home directory
and every project sees it. With --scope project, it is installed in the
current git repository (or the current directory outside one), where you can
commit it for everyone who works on the project. --dir installs it in any
other skills directory.`,
		Args: cobra.NoArgs,
	}
	command.AddCommand(
		newSkillsListCommand(a),
		newSkillsInstallCommand(a),
		newSkillsUninstallCommand(a),
		newSkillsShowCommand(),
	)
	return command
}

func addSkillTargetFlags(command *cobra.Command, options *skillOptions, scopeDefault string) {
	flags := command.Flags()
	flags.StringVar(&options.agent, "agent", "all", "the agent: claude, codex or all")
	scopeHelp := "user (your home directory) or project (the current git repository)"
	if scopeDefault == "" {
		scopeHelp += "; both when omitted"
	}
	flags.StringVar(&options.scope, "scope", scopeDefault, scopeHelp)
	flags.StringVar(&options.dir, "dir", "", "a skills directory to use instead of --agent and --scope")
	_ = command.RegisterFlagCompletionFunc("agent", cobra.FixedCompletions([]string{"claude", "codex", "all"}, cobra.ShellCompDirectiveNoFileComp))
	_ = command.RegisterFlagCompletionFunc("scope", cobra.FixedCompletions(skillScopes, cobra.ShellCompDirectiveNoFileComp))
}

func newSkillsListCommand(a *app) *cobra.Command {
	var options skillOptions
	command := &cobra.Command{
		Use:     "ls",
		Aliases: []string{"list"},
		Short:   "Show where the gv skill is installed and whether it is current",
		Args:    cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			targets, err := a.skillTargets(options)
			if err != nil {
				return err
			}
			bundle, err := newSkillBundle(a.releaseVersion())
			if err != nil {
				return err
			}
			if options.json {
				for _, target := range targets {
					state, err := inspectSkill(target.dir, bundle)
					if err != nil {
						return err
					}
					record := map[string]any{
						"agent": nil, "scope": nil,
						"path":   filepath.Join(target.dir, skills.Name),
						"status": state.status, "version": nil,
					}
					if target.agent != nil {
						record["agent"], record["scope"] = target.agent.name, target.scope
					}
					if state.version != "" {
						record["version"] = state.version
					}
					if err := writeJSONLine(a.stdout, record); err != nil {
						return err
					}
				}
				return nil
			}
			table := tabwriter.NewWriter(a.stdout, 0, 8, 2, ' ', 0)
			fmt.Fprintln(table, "AGENT\tSCOPE\tSTATUS\tPATH")
			for _, target := range targets {
				state, err := inspectSkill(target.dir, bundle)
				if err != nil {
					return err
				}
				fmt.Fprintf(table, "%s\t%s\t%s\t%s\n", target.agentName(), target.scopeName(), state.describe(), filepath.Join(target.dir, skills.Name))
			}
			return table.Flush()
		},
	}
	addSkillTargetFlags(command, &options, "")
	command.Flags().BoolVar(&options.json, "json", false, "print one JSON object per location instead of a table")
	return command
}

func newSkillsInstallCommand(a *app) *cobra.Command {
	var options skillOptions
	command := &cobra.Command{
		Use:   "install",
		Short: "Install or update the gv skill",
		Long: `Install the gv skill, or update it to this gv release.

An installed copy is replaced only when gv installed it and it has not been
changed since. Pass --force to replace a copy that was changed or that gv did
not install.`,
		Example: `  gv skills install                         # Claude Code and Codex, in your home directory
  gv skills install --agent claude --scope project
  gv skills install --dir ~/.config/agent/skills`,
		Args: cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			targets, err := a.skillTargets(options)
			if err != nil {
				return err
			}
			bundle, err := newSkillBundle(a.releaseVersion())
			if err != nil {
				return err
			}
			var refused []string
			for _, target := range targets {
				if err := a.installSkill(target, bundle, options.force); err != nil {
					if errors.Is(err, errSkillRefused) {
						refused = append(refused, err.Error())
						continue
					}
					return err
				}
				a.noteOtherScope(target, bundle)
			}
			if len(refused) > 0 {
				return errors.New(strings.Join(refused, "\n"))
			}
			return nil
		},
	}
	addSkillTargetFlags(command, &options, "user")
	command.Flags().BoolVarP(&options.force, "force", "f", false, "replace a copy that was changed or that gv did not install")
	return command
}

func newSkillsUninstallCommand(a *app) *cobra.Command {
	var options skillOptions
	command := &cobra.Command{
		Use:     "uninstall",
		Aliases: []string{"rm", "remove"},
		Short:   "Remove the gv skill",
		Long: `Remove the gv skill.

Only a copy that gv installed is removed. A copy that was changed since is
kept unless you pass --force; a directory gv did not install is never removed.`,
		Args: cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			targets, err := a.skillTargets(options)
			if err != nil {
				return err
			}
			bundle, err := newSkillBundle(a.releaseVersion())
			if err != nil {
				return err
			}
			var refused []string
			for _, target := range targets {
				if err := a.uninstallSkill(target, bundle, options.force); err != nil {
					if errors.Is(err, errSkillRefused) {
						refused = append(refused, err.Error())
						continue
					}
					return err
				}
			}
			if len(refused) > 0 {
				return errors.New(strings.Join(refused, "\n"))
			}
			return nil
		},
	}
	addSkillTargetFlags(command, &options, "user")
	command.Flags().BoolVarP(&options.force, "force", "f", false, "remove a copy that was changed since gv installed it")
	return command
}

func newSkillsShowCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "show [FILE]",
		Short: "Print the gv skill, or one of its reference files",
		Long: `Print the gv skill's SKILL.md, or one of its reference files, such as
references/search.md. Use it to read the skill, or to give it to an agent that
has no skills directory.`,
		Args: cobra.MaximumNArgs(1),
		ValidArgsFunction: func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
			names, _ := skillFileNames()
			return names, cobra.ShellCompDirectiveNoFileComp
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			name := skillFile
			if len(args) == 1 {
				name = path.Clean(filepath.ToSlash(args[0]))
			}
			data, err := fs.ReadFile(skills.Files(), name)
			if err != nil {
				names, _ := skillFileNames()
				return fmt.Errorf("the skill has no file %s; choose one of: %s", args[0], strings.Join(names, ", "))
			}
			_, err = cmd.OutOrStdout().Write(data)
			return err
		},
	}
}

func skillFileNames() ([]string, error) {
	var names []string
	err := fs.WalkDir(skills.Files(), ".", func(name string, entry fs.DirEntry, err error) error {
		if err == nil && !entry.IsDir() {
			names = append(names, name)
		}
		return err
	})
	return names, err
}

// skillTargets resolves --agent, --scope and --dir into the directories to
// act on. An empty scope means both.
func (a *app) skillTargets(options skillOptions) ([]skillTarget, error) {
	if options.dir != "" {
		dir, err := filepath.Abs(options.dir)
		if err != nil {
			return nil, fmt.Errorf("resolve --dir: %w", err)
		}
		return []skillTarget{{dir: dir}}, nil
	}
	var agents []*skillAgent
	switch options.agent {
	case "all", "":
		for i := range skillAgents {
			agents = append(agents, &skillAgents[i])
		}
	default:
		for i := range skillAgents {
			if skillAgents[i].name == options.agent {
				agents = append(agents, &skillAgents[i])
			}
		}
		if agents == nil {
			return nil, fmt.Errorf("unknown agent %q; use claude, codex or all", options.agent)
		}
	}
	scopes := skillScopes
	if options.scope != "" {
		if !slices.Contains(skillScopes, options.scope) {
			return nil, fmt.Errorf("unknown scope %q; use user or project", options.scope)
		}
		scopes = []string{options.scope}
	}
	var targets []skillTarget
	for _, scope := range scopes {
		base, err := a.skillScopeRoot(scope)
		if err != nil {
			return nil, err
		}
		for _, agent := range agents {
			targets = append(targets, skillTarget{agent: agent, scope: scope, dir: filepath.Join(base, agent.dir)})
		}
	}
	return targets, nil
}

// skillScopeRoot is the home directory for the user scope, and for the
// project scope the root of the git work tree that holds the current
// directory, or the current directory outside git.
func (a *app) skillScopeRoot(scope string) (string, error) {
	if scope == "user" {
		home, err := a.homeDirectory()
		if err != nil {
			return "", fmt.Errorf("find home directory: %w", err)
		}
		return home, nil
	}
	dir, err := a.workingDirectory()
	if err != nil {
		return "", fmt.Errorf("find current directory: %w", err)
	}
	dir, err = filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("find current directory: %w", err)
	}
	for candidate := dir; ; {
		if _, err := os.Lstat(filepath.Join(candidate, ".git")); err == nil {
			return candidate, nil
		}
		parent := filepath.Dir(candidate)
		if parent == candidate {
			return dir, nil
		}
		candidate = parent
	}
}

func (a *app) homeDirectory() (string, error) {
	if a.homeDir != nil {
		return a.homeDir()
	}
	return os.UserHomeDir()
}

func (a *app) workingDirectory() (string, error) {
	if a.workDir != nil {
		return a.workDir()
	}
	return os.Getwd()
}

func (a *app) releaseVersion() string {
	if a.version != "" {
		return a.version
	}
	return "dev"
}

// skillBundle is the skill as this gv installs it: its files, with SKILL.md
// stamped with the gv version and a checksum of the unstamped files. The
// checksum tells a copy gv installed and nobody changed from any other.
type skillBundle struct {
	version  string
	checksum string
	files    map[string][]byte
}

func newSkillBundle(version string) (skillBundle, error) {
	source := map[string][]byte{}
	err := fs.WalkDir(skills.Files(), ".", func(name string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		data, err := fs.ReadFile(skills.Files(), name)
		source[name] = data
		return err
	})
	if err != nil {
		return skillBundle{}, err
	}
	sum := skillChecksum(source)
	stamped, err := stampSkill(source[skillFile], version, sum)
	if err != nil {
		return skillBundle{}, err
	}
	files := maps.Clone(source)
	files[skillFile] = stamped
	return skillBundle{version: version, checksum: sum, files: files}, nil
}

// skillChecksum hashes a skill's files by their slash-separated paths.
func skillChecksum(files map[string][]byte) string {
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	slices.Sort(names)
	hash := sha256.New()
	for _, name := range names {
		fmt.Fprintf(hash, "%s\x00%d\x00", name, len(files[name]))
		hash.Write(files[name])
	}
	return "sha256:" + hex.EncodeToString(hash.Sum(nil))
}

// stampPattern matches the metadata block stampSkill writes, as the last
// lines of the frontmatter.
var stampPattern = regexp.MustCompile(`\nmetadata:\n  gv-version: ("(?:[^"\\\n]|\\.)*")\n  gv-checksum: ("(?:[^"\\\n]|\\.)*")\n---\n`)

// stampSkill adds the gv version and checksum to the end of SKILL.md's
// frontmatter, where agents ignore them.
func stampSkill(skill []byte, version, checksum string) ([]byte, error) {
	end := frontmatterEnd(skill)
	if end < 0 {
		return nil, errors.New("the bundled SKILL.md has no frontmatter")
	}
	var out bytes.Buffer
	out.Write(skill[:end])
	fmt.Fprintf(&out, "\nmetadata:\n  gv-version: %s\n  gv-checksum: %s", strconv.Quote(version), strconv.Quote(checksum))
	out.Write(skill[end:])
	return out.Bytes(), nil
}

// unstampSkill returns SKILL.md as it was before stampSkill, with the version
// and checksum it held; ok is false when it holds no stamp.
func unstampSkill(skill []byte) (source []byte, version, checksum string, ok bool) {
	end := frontmatterEnd(skill)
	if end < 0 {
		return nil, "", "", false
	}
	match := stampPattern.FindSubmatchIndex(skill)
	// The stamp must close the frontmatter: end is the line break before it.
	if match == nil || match[1]-len("\n---\n") != end {
		return nil, "", "", false
	}
	version, err1 := strconv.Unquote(string(skill[match[2]:match[3]]))
	checksum, err2 := strconv.Unquote(string(skill[match[4]:match[5]]))
	if err1 != nil || err2 != nil {
		return nil, "", "", false
	}
	source = slices.Concat(skill[:match[0]], skill[end:])
	return source, version, checksum, true
}

// frontmatterEnd returns the offset of the line break before the closing
// "---" of the frontmatter, or -1.
func frontmatterEnd(skill []byte) int {
	if !bytes.HasPrefix(skill, []byte("---\n")) {
		return -1
	}
	end := bytes.Index(skill[3:], []byte("\n---\n"))
	if end < 0 {
		return -1
	}
	return end + 3
}

// skillState describes what a skills directory holds under the skill's name.
type skillState struct {
	// status is absent, current, outdated, modified or foreign.
	status  string
	version string
}

func (s skillState) describe() string {
	switch s.status {
	case "absent":
		return "not installed"
	case "current":
		return "installed, current (" + s.version + ")"
	case "outdated":
		return "installed, from gv " + s.version + "; run gv skills install to update"
	case "modified":
		return "changed since gv installed it"
	default:
		return "not installed by gv"
	}
}

func inspectSkill(dir string, bundle skillBundle) (skillState, error) {
	root := filepath.Join(dir, skills.Name)
	info, err := os.Lstat(root)
	if errors.Is(err, fs.ErrNotExist) {
		return skillState{status: "absent"}, nil
	}
	if err != nil {
		return skillState{}, err
	}
	if !info.IsDir() {
		return skillState{status: "foreign"}, nil
	}
	tree, err := os.OpenRoot(root)
	if err != nil {
		return skillState{}, err
	}
	defer tree.Close()
	files := map[string][]byte{}
	links := false
	err = fs.WalkDir(tree.FS(), ".", func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.Type()&fs.ModeSymlink != 0 || (!entry.IsDir() && !entry.Type().IsRegular()) {
			links = true
			return nil
		}
		if entry.IsDir() {
			return nil
		}
		data, err := fs.ReadFile(tree.FS(), name)
		files[name] = data
		return err
	})
	if err != nil {
		return skillState{}, fmt.Errorf("read %s: %w", root, err)
	}
	source, version, checksum, ok := unstampSkill(files[skillFile])
	if !ok {
		return skillState{status: "foreign"}, nil
	}
	files[skillFile] = source
	if links || skillChecksum(files) != checksum {
		return skillState{status: "modified", version: version}, nil
	}
	if version == bundle.version && checksum == bundle.checksum {
		return skillState{status: "current", version: version}, nil
	}
	return skillState{status: "outdated", version: version}, nil
}

// errSkillRefused marks a location gv left alone; the command reports it and
// carries on with the other locations.
var errSkillRefused = errors.New("refused")

type refusedError struct{ message string }

func (e *refusedError) Error() string { return e.message }
func (e *refusedError) Is(target error) bool {
	return target == errSkillRefused
}

func (a *app) installSkill(target skillTarget, bundle skillBundle, force bool) error {
	state, err := inspectSkill(target.dir, bundle)
	if err != nil {
		return err
	}
	root := filepath.Join(target.dir, skills.Name)
	switch state.status {
	case "current":
		if !force {
			fmt.Fprintf(a.stdout, "%s: %s is already current (%s)\n", target.describe(), root, bundle.version)
			return nil
		}
	case "modified":
		if !force {
			return &refusedError{fmt.Sprintf("%s: %s was changed since gv installed it; pass --force to replace it", target.describe(), root)}
		}
	case "foreign":
		if !force {
			return &refusedError{fmt.Sprintf("%s: %s was not installed by gv; pass --force to replace it", target.describe(), root)}
		}
	}
	if err := writeSkill(target.dir, bundle); err != nil {
		return fmt.Errorf("install the skill in %s: %w", root, err)
	}
	verb := "Installed"
	if state.status != "absent" {
		verb = "Updated"
	}
	fmt.Fprintf(a.stdout, "%s: %s %s %s in %s\n", target.describe(), verb, skills.Name, bundle.version, root)
	return nil
}

// writeSkill writes the bundle to a new directory next to the skill's and
// then swaps it in, so an agent never reads a half-written skill.
func writeSkill(dir string, bundle skillBundle) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	staging, err := os.MkdirTemp(dir, "."+skills.Name+"-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(staging)
	if err := os.Chmod(staging, 0o755); err != nil {
		return err
	}
	for name, data := range bundle.files {
		file := filepath.Join(staging, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(file, data, 0o644); err != nil {
			return err
		}
	}
	root := filepath.Join(dir, skills.Name)
	if err := os.RemoveAll(root); err != nil {
		return err
	}
	return os.Rename(staging, root)
}

func (a *app) uninstallSkill(target skillTarget, bundle skillBundle, force bool) error {
	state, err := inspectSkill(target.dir, bundle)
	if err != nil {
		return err
	}
	root := filepath.Join(target.dir, skills.Name)
	switch state.status {
	case "absent":
		fmt.Fprintf(a.stdout, "%s: %s is not installed in %s\n", target.describe(), skills.Name, target.dir)
		return nil
	case "foreign":
		return &refusedError{fmt.Sprintf("%s: %s was not installed by gv; remove it yourself if you no longer want it", target.describe(), root)}
	case "modified":
		if !force {
			return &refusedError{fmt.Sprintf("%s: %s was changed since gv installed it; pass --force to remove it", target.describe(), root)}
		}
	}
	if err := os.RemoveAll(root); err != nil {
		return fmt.Errorf("remove %s: %w", root, err)
	}
	fmt.Fprintf(a.stdout, "%s: Removed %s\n", target.describe(), root)
	return nil
}

// noteOtherScope warns when the agent also has the skill at its other scope,
// where it would see two copies, possibly of different versions.
func (a *app) noteOtherScope(target skillTarget, bundle skillBundle) {
	if target.agent == nil {
		return
	}
	other := "project"
	if target.scope == "project" {
		other = "user"
	}
	base, err := a.skillScopeRoot(other)
	if err != nil {
		return
	}
	dir := filepath.Join(base, target.agent.dir)
	if dir == target.dir {
		return
	}
	state, err := inspectSkill(dir, bundle)
	if err != nil || state.status == "absent" {
		return
	}
	fmt.Fprintf(a.stderr, "gv: %s also has %s at %s scope, in %s; keep one with gv skills uninstall --agent %s --scope %s\n",
		target.agent.title, skills.Name, other, filepath.Join(dir, skills.Name), target.agent.name, other)
}
