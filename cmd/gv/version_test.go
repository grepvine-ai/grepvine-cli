package main

import (
	"runtime/debug"
	"testing"
)

func TestVersionStringGoInstall(t *testing.T) {
	info := &debug.BuildInfo{Main: debug.Module{Version: "v0.1.0"}}
	if got, want := versionString(info, true), "v0.1.0 (commit unknown, committed unknown)"; got != want {
		t.Fatalf("versionString = %q, want %q", got, want)
	}
}

func TestVersionStringVCS(t *testing.T) {
	info := &debug.BuildInfo{Main: debug.Module{Version: "(devel)"}, Settings: []debug.BuildSetting{
		{Key: "vcs.revision", Value: "0123456789abcdef"},
		{Key: "vcs.time", Value: "2026-10-21T00:00:00Z"},
		{Key: "vcs.modified", Value: "true"},
	}}
	if got, want := versionString(info, true), "dev (commit 0123456789ab-dirty, committed 2026-10-21T00:00:00Z)"; got != want {
		t.Fatalf("versionString = %q, want %q", got, want)
	}
}

func TestVersionStringStamped(t *testing.T) {
	saved := [3]string{version, commit, commitTime}
	t.Cleanup(func() { version, commit, commitTime = saved[0], saved[1], saved[2] })
	version, commit, commitTime = "1.2.3", "abc", "t"
	info := &debug.BuildInfo{Main: debug.Module{Version: "v9.9.9"}, Settings: []debug.BuildSetting{{Key: "vcs.revision", Value: "ffff"}}}
	if got, want := versionString(info, true), "1.2.3 (commit abc, committed t)"; got != want {
		t.Fatalf("versionString = %q, want %q", got, want)
	}
}
