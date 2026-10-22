package main

import (
	"fmt"
	"runtime/debug"
)

// version, commit and commitTime are stamped at build time with
// -ldflags '-X main.version=...'. An unstamped build falls back to what the
// Go toolchain recorded.
var (
	version    = "dev"
	commit     = "unknown"
	commitTime = "unknown"
)

// versionString returns the text `gv --version` prints. A binary built with
// `go install ...@v0.1.0` carries its module version; a binary built from a
// checkout carries its VCS revision.
func versionString(info *debug.BuildInfo, ok bool) string {
	v, c, t := version, commit, commitTime
	if ok {
		if v == "dev" && info.Main.Version != "" && info.Main.Version != "(devel)" {
			v = info.Main.Version
		}
		var rev, modified, when string
		for _, s := range info.Settings {
			switch s.Key {
			case "vcs.revision":
				rev = s.Value
			case "vcs.modified":
				modified = s.Value
			case "vcs.time":
				when = s.Value
			}
		}
		if c == "unknown" && rev != "" {
			if len(rev) > 12 {
				rev = rev[:12]
			}
			if modified == "true" {
				rev += "-dirty"
			}
			c = rev
		}
		if t == "unknown" && when != "" {
			t = when
		}
	}
	return fmt.Sprintf("%s (commit %s, committed %s)", v, c, t)
}
