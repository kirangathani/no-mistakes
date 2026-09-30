package handoff

import (
	"fmt"
	"os"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/pipeline/steps/internal/stepstest"
	"github.com/kunchenguid/no-mistakes/internal/shellenv"
)

func TestMain(m *testing.M) {
	// On Windows a configured repository command runs through a cooperative
	// helper that re-executes os.Executable() - this test binary - so the
	// binary must serve that helper before anything else, exactly as the
	// steps package's TestMain does. Without it the command never runs and
	// its exit status is the test binary's.
	if handled, exitCode, err := shellenv.RunWindowsCooperativeCommandHelper(os.Args[1:]); handled {
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
		}
		os.Exit(exitCode)
	}
	os.Unsetenv("GIT_CONFIG_COUNT")
	cleanup, err := stepstest.Init()
	if err != nil {
		fmt.Fprintf(os.Stderr, "init fake CLI helper: %v\n", err)
		os.Exit(1)
	}
	code := m.Run()
	if err := cleanup(); err != nil {
		fmt.Fprintf(os.Stderr, "cleanup fake CLI helper: %v\n", err)
		code = 1
	}
	os.Exit(code)
}
