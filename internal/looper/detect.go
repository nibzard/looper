package looper

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Detection is a candidate verify command with the evidence that produced it.
// An empty Command leaves verification to execution planning.
type Detection struct {
	Command string
	Source  string
}

// A Makefile test target is "test:" at line start.
var makeTarget = regexp.MustCompile(`(?m)^test\s*:`)

// DetectVerifyCommand inspects repository files without running commands. It
// only reads evidence at the repository root, in the documented order: an
// explicit Makefile test target, go.mod, package.json with an unambiguous
// package manager, then Cargo.toml. Nested project roots and ambiguous
// package managers leave the override empty so planning selects the command.
// A detected command is a candidate override, not proof that checks cover the
// whole product.
func DetectVerifyCommand(root string) Detection {
	if command, ok := makefileTest(root); ok {
		return Detection{command, "Makefile test target"}
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err == nil {
		if nestedProject(root, "go.mod") {
			return Detection{}
		}
		return Detection{"go test ./...", "go.mod"}
	}
	if command, source, ok := nodeTest(root); ok {
		if nestedProject(root, "package.json") {
			return Detection{}
		}
		return Detection{command, source}
	}
	if _, err := os.Stat(filepath.Join(root, "Cargo.toml")); err == nil {
		if nestedProject(root, "Cargo.toml") {
			return Detection{}
		}
		return Detection{"cargo test", "Cargo.toml"}
	}
	return Detection{}
}

// nestedProject reports whether a different project root hides below the
// repository root, which makes root evidence ambiguous for whole-product
// checks. Only one shallow, non-ignored level is inspected.
func nestedProject(root, rootMarker string) bool {
	entries, err := os.ReadDir(root)
	if err != nil {
		return false
	}
	for _, entry := range entries {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		switch entry.Name() {
		case "node_modules", "vendor", "target", "tmp", "dist", "build":
			continue
		}
		for _, marker := range []string{"go.mod", "package.json", "Cargo.toml"} {
			if marker == rootMarker {
				continue
			}
			if _, err := os.Stat(filepath.Join(root, entry.Name(), marker)); err == nil {
				return true
			}
		}
	}
	return false
}

func makefileTest(root string) (string, bool) {
	b, err := os.ReadFile(filepath.Join(root, "Makefile"))
	if err != nil {
		return "", false
	}
	for _, loc := range makeTarget.FindAllIndex(b, -1) {
		// ":=" and "::=" assign a variable; they are not targets.
		if loc[1] < len(b) && (b[loc[1]] == ':' || b[loc[1]] == '=') {
			continue
		}
		return "make test", true
	}
	return "", false
}

// nodeTest requires a test script and one unambiguous package manager. The
// packageManager declaration wins over lockfiles; disagreeing or multiple
// lockfiles make the manager ambiguous.
func nodeTest(root string) (string, string, bool) {
	b, err := os.ReadFile(filepath.Join(root, "package.json"))
	if err != nil {
		return "", "", false
	}
	var pkg struct {
		Scripts        map[string]string `json:"scripts"`
		PackageManager string            `json:"packageManager"`
	}
	if err := json.Unmarshal(b, &pkg); err != nil {
		return "", "", false
	}
	if pkg.Scripts["test"] == "" {
		return "", "", false
	}
	declared := ""
	if pkg.PackageManager != "" {
		declared, _, _ = strings.Cut(pkg.PackageManager, "@")
	}
	locks := map[string]string{
		"package-lock.json": "npm", "pnpm-lock.yaml": "pnpm",
		"yarn.lock": "yarn", "bun.lockb": "bun", "bun.lock": "bun",
	}
	found := map[string]bool{}
	for file, manager := range locks {
		if _, err := os.Stat(filepath.Join(root, file)); err == nil {
			found[manager] = true
		}
	}
	manager := declared
	if manager == "" {
		if len(found) == 1 {
			for m := range found {
				manager = m
			}
		}
	} else if len(found) > 0 && !found[manager] {
		return "", "", false
	}
	if manager == "" || len(found) > 1 {
		return "", "", false
	}
	return manager + " test", "package.json test script via " + manager, true
}
