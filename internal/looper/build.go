package looper

import (
	"fmt"
	"runtime/debug"
)

// BuildInfo describes this executable, not the currently installed binary.
// Source archives without VCS metadata explicitly report an unknown revision.
type BuildInfo struct {
	Release  string `json:"release"`
	Revision string `json:"revision"`
	Modified bool   `json:"modified"`
}

func currentBuild() BuildInfo {
	build := BuildInfo{Release: Version, Revision: "unknown"}
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, setting := range info.Settings {
			switch setting.Key {
			case "vcs.revision":
				build.Revision = setting.Value
			case "vcs.modified":
				build.Modified = setting.Value == "true"
			}
		}
	}
	return build
}

func (build BuildInfo) String() string {
	revision := build.Revision
	if len(revision) > 12 {
		revision = revision[:12]
	}
	if build.Modified {
		revision += ", modified"
	}
	return fmt.Sprintf("looper %s (%s)", build.Release, revision)
}
