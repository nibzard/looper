package looper

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// This probe runs in a supervised child so it checks the actual environment.
func TestPrivateTMPDIRProbe(t *testing.T) {
	mode := os.Getenv("LOOPER_TEST_TMPDIR_PROBE")
	if mode == "" {
		t.Skip("child process only")
	}
	tmp := os.Getenv("TMPDIR")
	info, err := os.Stat(tmp)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0700 {
		t.Fatalf("temporary directory mode: %o", info.Mode().Perm())
	}
	socket, err := net.Listen("unix", filepath.Join(tmp, strings.Repeat("s", 50)+".sock"))
	if err != nil {
		t.Fatal(err)
	}
	socket.Close()
	if err := os.WriteFile(filepath.Join(tmp, "scratch"), []byte("temporary"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(os.Getenv("LOOPER_ARTIFACT_DIR"), "probe"), []byte(tmp), 0600); err != nil {
		t.Fatal(err)
	}
	switch mode {
	case "failure":
		os.Exit(7)
	case "cancel":
		time.Sleep(time.Minute)
	}
}

func TestProcessPrivateTMPDIR(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, strings.Repeat("deep-runtime-", 15))
	t.Setenv("TMPDIR", dir)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, mode := range []string{"success", "failure", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			artifact := filepath.Join(dir, mode)
			extra := resourceEnv(artifact)
			extra["LOOPER_TEST_TMPDIR_PROBE"] = mode
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() {
				done <- runProcess(ctx, root, filepath.Join(artifact, "events.log"),
					[]string{exe, "-test.run=^TestPrivateTMPDIRProbe$"}, nil, extra)
			}()
			if mode == "cancel" {
				deadline := time.Now().Add(10 * time.Second)
				for {
					if _, err := os.Stat(filepath.Join(artifact, "probe")); err == nil {
						break
					}
					if time.Now().After(deadline) {
						t.Fatal("child did not create its probe")
					}
					time.Sleep(10 * time.Millisecond)
				}
				cancel()
			}
			select {
			case err = <-done:
			case <-time.After(10 * time.Second):
				t.Fatal("child did not exit")
			}
			if mode == "success" && err != nil {
				log, _ := os.ReadFile(filepath.Join(artifact, "events.log"))
				t.Fatalf("process failed: %v\n%s", err, log)
			}
			if mode == "failure" && err == nil {
				t.Fatal("child failure was not returned")
			}
			if mode == "cancel" && !errors.Is(err, context.Canceled) {
				t.Fatalf("cancellation error: %v", err)
			}
			data, err := os.ReadFile(filepath.Join(artifact, "probe"))
			if err != nil {
				t.Fatal(err)
			}
			tmp := string(data)
			if seen[tmp] {
				t.Fatal("child processes share a temporary directory")
			}
			seen[tmp] = true
			if _, err := os.Stat(tmp); !os.IsNotExist(err) {
				t.Fatalf("temporary directory remains: %s (%v)", tmp, err)
			}
			if _, ok := extra["TMPDIR"]; ok {
				t.Fatal("process changed the caller's environment")
			}
		})
	}
}
