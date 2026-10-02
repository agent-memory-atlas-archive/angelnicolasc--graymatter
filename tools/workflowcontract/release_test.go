package workflowcontract

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

func stepBefore(t *testing.T, job, first, second string) {
	t.Helper()
	a := strings.Index(job, "      - name: "+first+"\n")
	b := strings.Index(job, "      - name: "+second+"\n")
	if a < 0 || b < 0 || a >= b {
		t.Errorf("%q must run before %q", first, second)
	}
}

func TestReleasePreflightBeforePublication(t *testing.T) {
	job := ciJob(read(t, "release"), "release")
	preflight := ciStep(job, "Validate release metadata and tag identity")
	contains(t, preflight, `TAG: ${{ github.ref_name }}`, `python3 .github/scripts/release_check.py --tag "$TAG" --check-tags`)
	absent(t, preflight, "continue-on-error:", "if:", "|| true")
	stepBefore(t, job, "Validate release metadata and tag identity", "Publish the CLI submodule tag")
	stepBefore(t, job, "Check GoReleaser configuration before publishing", "Publish the CLI submodule tag")
	contains(t, ciStep(job, "Check GoReleaser configuration before publishing"), "python3 .github/scripts/check_goreleaser.py")
	// An existing remote CLI tag is checked again rather than being accepted
	// merely because its name exists.
	contains(t, ciStep(job, "Publish the CLI submodule tag"), `python3 .github/scripts/release_check.py --tag "$TAG" --check-tags`)
	snapshot := ciJob(read(t, "maintenance-smoke"), "snapshot")
	metadata := ciStep(snapshot, "Validate release metadata")
	contains(t, metadata, "python3 -m unittest discover -s .github/scripts -p 'test_*.py'", "python3 .github/scripts/release_check.py")
	absent(t, metadata, "--check-tags", "continue-on-error:", "if:", "|| true")
	stepBefore(t, snapshot, "Validate release metadata", "Build against the checked-out library")
}

func TestUnpublishedVersionWorkspacePreparation(t *testing.T) {
	cases := []struct {
		workflow, job, prepare, firstWork string
	}{
		{"ci", "coverage-union", "Source-replace the library into the CLI module", "Check workflow contracts"},
		{"fuzz", "fuzz", "Build against the checked-out library", "Fuzz Tokenize (60s)"},
		{"mutation", "mutate", "Build against the checked-out library", "Install gremlins v0.5.0"},
		{"maintenance-smoke", "artifacts-mutation", "Build against the checked-out library", "Install gremlins v0.5.0"},
	}
	for _, tc := range cases {
		t.Run(tc.workflow+"/"+tc.job, func(t *testing.T) {
			job := ciJob(read(t, tc.workflow), tc.job)
			step := ciStep(job, tc.prepare)
			contains(t, step, "working-directory: cmd/graymatter", `GOWORK: "off"`, "run: go mod edit -replace github.com/angelnicolasc/graymatter=../..")
			absent(t, step, "continue-on-error:", "if:", "|| true")
			stepBefore(t, job, tc.prepare, tc.firstWork)
		})
	}
}

func TestReleaseToolchainMatchesCLIModule(t *testing.T) {
	module, err := os.ReadFile("../../cmd/graymatter/go.mod")
	if err != nil {
		t.Fatal(err)
	}
	match := regexp.MustCompile(`(?m)^toolchain go([0-9.]+)\r?$`).FindStringSubmatch(string(module))
	if len(match) != 2 {
		t.Fatal("CLI must declare one release toolchain")
	}
	for _, tc := range []struct{ workflow, job string }{
		{"release", "release"}, {"release", "install-smoke"}, {"maintenance-smoke", "snapshot"},
	} {
		t.Run(tc.workflow+"/"+tc.job, func(t *testing.T) {
			job := ciJob(read(t, tc.workflow), tc.job)
			versions := regexp.MustCompile(`(?m)^          go-version: "([0-9.]+)"$`).FindAllStringSubmatch(job, -1)
			if len(versions) != 1 || versions[0][1] != match[1] {
				t.Errorf("release toolchain must match CLI %s; found %v", match[1], versions)
			}
		})
	}
}

func TestInstallSmokeValidatesPublishedLibrary(t *testing.T) {
	job := ciJob(read(t, "release"), "install-smoke")
	contains(t, job, `MOD="github.com/angelnicolasc/graymatter/cmd/graymatter@${TAG}"`, `go install "${MOD}"`, `"${INSTALLED}" --version`, `go version -m "$INSTALLED"`,
		`$2 == "github.com/angelnicolasc/graymatter"`, `[ "$library" != "$TAG" ]`, `grep -q '=>'`)
	absent(t, job, "go mod edit", "-replace", "actions/checkout@")
}
