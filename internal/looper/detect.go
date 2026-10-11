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
		return Detection{"go test ./...", "go.mod"}
	}
	if command, source, ok := nodeTest(root); ok {
		return Detection{command, source}
	}
	if _, err := os.Stat(filepath.Join(root, "Cargo.toml")); err == nil {
		return Detection{"cargo test", "Cargo.toml"}
	}
	return Detection{}
}

func makefileTest(root string) (string, bool) {
	b, err := os.ReadFile(filepath.Join(root, "Makefile"))
	if err != nil {
		return "", false
	}
	return "make test", makeTarget.Match(b)
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
