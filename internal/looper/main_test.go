package looper

import (
	"os"
	"testing"
	"time"
)

// TestMain dispatches helper invocations that re-execute the current
// executable. runProcess spawns os.Executable() with the __exec argument as a
// process-group supervisor; under go test that executable is this test binary,
// whose generated main would otherwise run the whole suite again.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "__exec" {
		if os.Getenv("LOOPER_TEST_LAUNCH_REPORT") == "missing" {
			os.Exit(0)
		}
		if os.Getenv("LOOPER_TEST_LAUNCH_REPORT") == "hang" {
			time.Sleep(time.Minute)
			os.Exit(0)
		}
		os.Exit(RunChild(os.Args[2:]))
	}
	os.Exit(m.Run())
}
