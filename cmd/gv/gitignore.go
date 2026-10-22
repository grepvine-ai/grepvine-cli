package main

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

type gitIgnoreMatcher struct {
	root  string
	rules []gitIgnoreRule
}

type gitIgnoreRule struct {
	base          string
	pattern       *regexp.Regexp
	negated       bool
	directoryOnly bool
}

func newGitIgnoreMatcher(root string) *gitIgnoreMatcher {
	return &gitIgnoreMatcher{root: root}
}

func (m *gitIgnoreMatcher) AddDirectory(directory string) error {
	info, err := os.Lstat(filepath.Join(directory, ".gitignore"))
	if errors.Is(err, os.ErrNotExist) || err == nil && !info.Mode().IsRegular() {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect %s: %w", filepath.Join(directory, ".gitignore"), err)
	}
	file, err := os.Open(filepath.Join(directory, ".gitignore"))
	if err != nil {
		return fmt.Errorf("open %s: %w", filepath.Join(directory, ".gitignore"), err)
	}
	defer file.Close()
	base, err := filepath.Rel(m.root, directory)
	if err != nil {
		return err
	}
	if base == "." {
		base = ""
	} else {
		base = filepath.ToSlash(base)
	}
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 4096), 1024*1024)
	line := 0
	for scanner.Scan() {
		line++
		rule, ok, parseErr := parseGitIgnoreRule(base, scanner.Text())
		if parseErr != nil {
			return fmt.Errorf("parse %s line %d: %w", filepath.Join(directory, ".gitignore"), line, parseErr)
		}
		if ok {
			m.rules = append(m.rules, rule)
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read %s: %w", filepath.Join(directory, ".gitignore"), err)
	}
	return nil
}

func (m *gitIgnoreMatcher) Match(relative string, directory bool) bool {
	relative = strings.TrimPrefix(filepath.ToSlash(relative), "./")
	ignored := false
	for _, rule := range m.rules {
		candidate, applies := rule.candidate(relative)
		if !applies || rule.directoryOnly && !directory {
			continue
		}
		if rule.pattern.MatchString(candidate) {
			ignored = !rule.negated
		}
	}
	return ignored
}

func (r gitIgnoreRule) candidate(relative string) (string, bool) {
	if r.base == "" {
		return relative, true
	}
	prefix := r.base + "/"
	if !strings.HasPrefix(relative, prefix) {
		return "", false
	}
	return strings.TrimPrefix(relative, prefix), true
}

func parseGitIgnoreRule(base, line string) (gitIgnoreRule, bool, error) {
	line = trimGitIgnoreSpaces(strings.TrimSuffix(line, "\r"))
	if line == "" || line[0] == '#' {
		return gitIgnoreRule{}, false, nil
	}
	negated := false
	if line[0] == '!' {
		negated = true
		line = line[1:]
	}
	if line == "" {
		return gitIgnoreRule{}, false, nil
	}
	directoryOnly := strings.HasSuffix(line, "/") && !strings.HasSuffix(line, `\/`)
	if directoryOnly {
		line = strings.TrimSuffix(line, "/")
	}
	anchored := strings.HasPrefix(line, "/") || strings.Contains(line, "/")
	line = strings.TrimPrefix(line, "/")
	expression, err := compileGitIgnorePattern(line, anchored)
	if err != nil {
		return gitIgnoreRule{}, false, err
	}
	return gitIgnoreRule{base: base, pattern: expression, negated: negated, directoryOnly: directoryOnly}, true, nil
}

func trimGitIgnoreSpaces(line string) string {
	for len(line) > 0 && line[len(line)-1] == ' ' {
		backslashes := 0
		for i := len(line) - 2; i >= 0 && line[i] == '\\'; i-- {
			backslashes++
		}
		if backslashes%2 == 1 {
			break
		}
		line = line[:len(line)-1]
	}
	return line
}

func compileGitIgnorePattern(pattern string, anchored bool) (*regexp.Regexp, error) {
	var expression strings.Builder
	if anchored {
		expression.WriteString("^")
	} else {
		expression.WriteString("(?:^|/)")
	}
	for i := 0; i < len(pattern); i++ {
		switch pattern[i] {
		case '\\':
			if i+1 < len(pattern) {
				i++
				expression.WriteString(regexp.QuoteMeta(pattern[i : i+1]))
			} else {
				expression.WriteString(`\\`)
			}
		case '*':
			if i+1 < len(pattern) && pattern[i+1] == '*' {
				for i+1 < len(pattern) && pattern[i+1] == '*' {
					i++
				}
				if i+1 < len(pattern) && pattern[i+1] == '/' {
					i++
					expression.WriteString("(?:.*/)?")
				} else {
					expression.WriteString(".*")
				}
			} else {
				expression.WriteString("[^/]*")
			}
		case '?':
			expression.WriteString("[^/]")
		case '[':
			end := gitIgnoreClassEnd(pattern, i+1)
			if end < 0 {
				expression.WriteString(`\[`)
				continue
			}
			class := pattern[i+1 : end]
			if strings.HasPrefix(class, "!") {
				class = "^" + class[1:]
			}
			expression.WriteByte('[')
			expression.WriteString(class)
			expression.WriteByte(']')
			i = end
		default:
			expression.WriteString(regexp.QuoteMeta(pattern[i : i+1]))
		}
	}
	expression.WriteString("$")
	return regexp.Compile(expression.String())
}

func gitIgnoreClassEnd(pattern string, start int) int {
	for i := start; i < len(pattern); i++ {
		if pattern[i] == '\\' {
			i++
			continue
		}
		if pattern[i] == ']' && i > start {
			return i
		}
	}
	return -1
}
