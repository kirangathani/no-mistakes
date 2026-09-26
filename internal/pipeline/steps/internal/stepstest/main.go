package stepstest

import (
	"fmt"
	"os"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/shellenv"
)

// RunMain is the TestMain body every step-driving test package shares. Use it
// rather than restating it: on Windows a configured repository command is
// launched through a cooperative console helper that re-execs os.Executable(),
// which in a test is the TEST binary. A package that does not dispatch that
// helper first re-enters its own TestMain in the command's working directory
// (a temporary git repository, where findModuleRoot fails), so the step sees
// that process's exit code instead of the command's.
func RunMain(m *testing.M) {
	if handled, exitCode, err := shellenv.RunWindowsCooperativeCommandHelper(os.Args[1:]); handled {
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
		}
		os.Exit(exitCode)
	}

	// Agent harnesses inject git config (e.g. safe.bareRepository=explicit)
	// via GIT_CONFIG_COUNT/KEY_n/VALUE_n; tests that need it re-set it with
	// t.Setenv (issue #362).
	os.Unsetenv("GIT_CONFIG_COUNT")
	cleanup, err := initFakeCLI()
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
