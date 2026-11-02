// Copyright 2026 Treeverse, Inc.
// SPDX-License-Identifier: Apache-2.0

// Package skills holds the agent skills that `gv skills install` copies into
// a coding agent's skills directory. Each skill is a directory with a
// SKILL.md and the reference files it links to.
package skills

import (
	"embed"
	"io/fs"
)

// Name is the directory, and the frontmatter name, of the gv skill.
const Name = "grepvine-cli"

//go:embed grepvine-cli
var files embed.FS

// Files returns the gv skill's files, rooted at its directory.
func Files() fs.FS {
	sub, err := fs.Sub(files, Name)
	if err != nil {
		panic(err) // the embedded directory always exists
	}
	return sub
}
