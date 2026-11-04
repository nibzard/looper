package looper

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}

func TestDetectVerifyCommand(t *testing.T) {
	root := t.TempDir()
	if d := DetectVerifyCommand(root); d.Command != "" {
		t.Fatalf("empty project must detect nothing: %+v", d)
	}
	// go.mod alone selects go test.
	write(t, filepath.Join(root, "go.mod"), "module example.com/x\n")
	if d := DetectVerifyCommand(root); d.Command != "go test ./..." || d.Source != "go.mod" {
		t.Fatalf("go.mod detection broken: %+v", d)
	}
	// A Makefile with an explicit test target wins over go.mod.
	write(t, filepath.Join(root, "Makefile"), "build:\n\tgo build\n\ntest:\n\tgo test ./...\n")
	if d := DetectVerifyCommand(root); d.Command != "make test" {
		t.Fatalf("Makefile order broken: %+v", d)
	}
	// A Makefile without a test target falls through to go.mod.
	write(t, filepath.Join(root, "Makefile"), "build:\n\tgo build\n")
	if d := DetectVerifyCommand(root); d.Command != "go test ./..." {
		t.Fatalf("Makefile without test must fall through: %+v", d)
	}
	// A tests target is not a test target.
	write(t, filepath.Join(root, "Makefile"), "tests:\n\techo no\n")
	if d := DetectVerifyCommand(root); d.Command != "go test ./..." {
		t.Fatalf("tests target must not match: %+v", d)
	}
	// Variable assignments are not targets, in either syntax.
	write(t, filepath.Join(root, "Makefile"), "test := dummy\nbuild:\n\tgo build\n")
	if d := DetectVerifyCommand(root); d.Command != "go test ./..." {
		t.Fatalf(":= assignment must not match: %+v", d)
	}
	write(t, filepath.Join(root, "Makefile"), "test ::= dummy\nbuild:\n\tgo build\n")
	if d := DetectVerifyCommand(root); d.Command != "go test ./..." {
		t.Fatalf("::= assignment must not match: %+v", d)
	}
}

func TestDetectPackageManager(t *testing.T) {
	root := t.TempDir()
	pkg := `{"scripts": {"test": "node --test"}}`
	write(t, filepath.Join(root, "package.json"), pkg)
	if d := DetectVerifyCommand(root); d.Command != "" {
		t.Fatalf("no manager evidence must stay empty: %+v", d)
	}
	write(t, filepath.Join(root, "package-lock.json"), "{}")
	if d := DetectVerifyCommand(root); d.Command != "npm test" || d.Source != "package.json test script via npm" {
		t.Fatalf("npm lockfile detection broken: %+v", d)
	}
	write(t, filepath.Join(root, "yarn.lock"), "")
	if d := DetectVerifyCommand(root); d.Command != "" {
		t.Fatalf("two lockfiles are ambiguous: %+v", d)
	}
	os.Remove(filepath.Join(root, "yarn.lock"))
	os.Remove(filepath.Join(root, "package-lock.json"))
	write(t, filepath.Join(root, "package.json"), `{"scripts": {"test": "vitest"}, "packageManager": "pnpm@9.1.0"}`)
	if d := DetectVerifyCommand(root); d.Command != "pnpm test" {
		t.Fatalf("packageManager declaration broken: %+v", d)
	}
	// A disagreeing lockfile makes the manager ambiguous.
	write(t, filepath.Join(root, "package-lock.json"), "{}")
	if d := DetectVerifyCommand(root); d.Command != "" {
		t.Fatalf("disagreeing manager evidence must stay empty: %+v", d)
	}
	// No test script means no node evidence at all.
	write(t, filepath.Join(root, "package.json"), `{"scripts": {"start": "node ."}, "packageManager": "npm@10"}`)
	os.Remove(filepath.Join(root, "package-lock.json"))
	if d := DetectVerifyCommand(root); d.Command != "" {
		t.Fatalf("package.json without a test script is not evidence: %+v", d)
	}
}

func TestDetectNestedMixedRootsStayEmpty(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "go.mod"), "module x\n")
	write(t, filepath.Join(root, "web", "package.json"), `{"scripts": {"test": "node --test"}}`)
	write(t, filepath.Join(root, "web", "package-lock.json"), "{}")
	if d := DetectVerifyCommand(root); d.Command != "" {
		t.Fatalf("a nested project root must leave the override empty: %+v", d)
	}
}

func TestDetectNestedSameMarker(t *testing.T) {
	for _, project := range []struct {
		marker, content, command string
	}{
		{"go.mod", "module example.com/project\n", "go test ./..."},
		{"package.json", `{"scripts":{"test":"node --test"},"packageManager":"npm@10"}`, "npm test"},
		{"Cargo.toml", "[package]\nname = \"project\"\n", "cargo test"},
	} {
		t.Run(project.marker, func(t *testing.T) {
			for _, child := range []string{"module", ".hidden", "node_modules", "vendor", "target", "tmp", "dist", "build"} {
				t.Run(child, func(t *testing.T) {
					root := t.TempDir()
					write(t, filepath.Join(root, project.marker), project.content)
					write(t, filepath.Join(root, child, project.marker), project.content)
					want := Detection{project.command, project.marker}
					if project.marker == "package.json" {
						want.Source = "package.json test script via npm"
					}
					if child == "module" {
						want = Detection{}
					}
					if got := DetectVerifyCommand(root); got != want {
						t.Fatalf("detection = %+v, want %+v", got, want)
					}
					// Explicit test targets retain precedence over nested markers.
					write(t, filepath.Join(root, "Makefile"), "test:\n\techo checks\n")
					if got := DetectVerifyCommand(root); got != (Detection{"make test", "Makefile test target"}) {
						t.Fatalf("explicit target detection = %+v", got)
					}
				})
			}
		})
	}
}

func TestDetectNestedGoModulePreservesPlannedCoverage(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "go.mod"), "module example.com/root\n")
	write(t, filepath.Join(root, "root_test.go"), "package root\nimport \"testing\"\nfunc TestRoot(t *testing.T) {}\n")
	write(t, filepath.Join(root, "module", "go.mod"), "module example.com/child\n")
	childTest := filepath.Join(root, "module", "child_test.go")
	write(t, childTest, "package child\nimport \"testing\"\nfunc TestChild(t *testing.T) { t.Fatal(\"nested module checked\") }\n")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	run := func(command string) (string, error) {
		t.Helper()
		cmd := exec.CommandContext(ctx, "sh", "-c", command)
		cmd.Dir = root
		cmd.Env = cleanEnv(map[string]string{"GOWORK": "off", "GOFLAGS": "", "GOTOOLCHAIN": "local"})
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	// The root command alone misses the failing test in the child module.
	if out, err := run("go test ./..."); err != nil {
		t.Fatalf("root test: %v\n%s", err, out)
	}
	detection := DetectVerifyCommand(root)
	plan := Plan{Verify: "go test ./... && (cd module && go test ./...)"}
	if err := plan.Validate(Backlog{}, detection.Command); err != nil {
		t.Fatal(err)
	}
	if out, err := run(plan.Verify); err == nil || !strings.Contains(out, "nested module checked") {
		t.Fatalf("verification must catch the child failure: %v\n%s", err, out)
	}
	write(t, childTest, "package child\nimport \"testing\"\nfunc TestChild(t *testing.T) {}\n")
	if out, err := run(plan.Verify); err != nil {
		t.Fatalf("verification after child repair: %v\n%s", err, out)
	}
	if err := (&Plan{}).Validate(Backlog{}, detection.Command); err == nil {
		t.Fatal("nested modules must still require executable verification")
	}
}

func TestDetectCargo(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "Cargo.toml"), "[package]\nname = \"x\"\n")
	if d := DetectVerifyCommand(root); d.Command != "cargo test" || d.Source != "Cargo.toml" {
		t.Fatalf("cargo detection broken: %+v", d)
	}
}

func TestDetectNestedProjectRoot(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "web", "package.json"), `{"scripts": {"test": "node --test"}, "packageManager": "npm@10"}`)
	write(t, filepath.Join(root, "web", "package-lock.json"), "{}")
	// Evidence below the repository root is a nested project; detection only
	// reads the root and leaves the command to planning.
	if d := DetectVerifyCommand(root); d.Command != "" {
		t.Fatalf("nested evidence must stay empty: %+v", d)
	}
}
