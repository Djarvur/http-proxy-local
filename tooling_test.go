// tooling_test.go enforces the project's engineering conventions at the
// toolchain level. Per the engineering-conventions spec, `golangci-lint run`
// MUST exit 0 with zero findings on the whole codebase. This test makes that
// contract executable so a regression (missing config, new finding) fails CI
// and local `go test` rather than slipping through.
package tooling_test

import (
	"os/exec"
	"strings"
	"testing"
)

func TestGolangCILintPasses(t *testing.T) {
	if _, err := exec.LookPath("golangci-lint"); err != nil {
		t.Skip("golangci-lint not installed; skipping (CI installs it via mise)")
	}

	cmd := exec.Command("golangci-lint", "run", "./...")
	out, err := cmd.CombinedOutput()
	if err != nil {
		// Non-zero exit + a findings block is the real failure signal.
		// golangci-lint prints "0 issues." to stdout on success, so we
		// discriminate on exit code and report stdout only when it failed.
		t.Fatalf("golangci-lint run ./... exited non-zero (must exit 0):\n%s", string(out))
	}
	// Exit 0 means the strict config passed with no findings.
	_ = strings.TrimSpace(string(out))
}
