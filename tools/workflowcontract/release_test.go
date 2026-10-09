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
	tags := ciStep(job, "Publish the CLI submodule tag")
	contains(t, tags, "Publish the signed annotated cmd/graymatter/${TAG} tag", "exit 1")
	absent(t, tags, "git tag ", "git push ")
	snapshot := ciJob(read(t, "maintenance-smoke"), "snapshot")
	metadata := ciStep(snapshot, "Validate release metadata")
	contains(t, metadata, "python3 -m unittest discover -s .github/scripts -p 'test_*.py'", "python3 .github/scripts/release_check.py")
	absent(t, metadata, "--check-tags", "continue-on-error:", "if:", "|| true")
	stepBefore(t, snapshot, "Validate release metadata", "Build against the checked-out library")
}

func TestPackageManagerManifestsRequireSignedPublication(t *testing.T) {
	config := readWorkflow(t, "../../.goreleaser.yml")
	for _, name := range []string{"brews", "scoops", "nix"} {
		block := regexp.MustCompile(`(?ms)^` + name + `:\n(.*?)(?:\n[a-z_]+:|\z)`).FindStringSubmatch(config)
		if len(block) != 2 {
			t.Fatalf("missing package-manager configuration: %s", name)
		}
		contains(t, block[1], "skip_upload: true", "name: angelnicolasc", "email: 108889887+angelnicolasc@users.noreply.github.com")
		absent(t, block[1], "token:", "skip_upload: auto", "skip_upload: false")
	}
	for _, tc := range []struct{ workflow, job, build string }{
		{"release", "release", "Run GoReleaser"},
		{"maintenance-smoke", "snapshot", "Build snapshot without publishing"},
	} {
		t.Run(tc.workflow, func(t *testing.T) {
			job := ciJob(read(t, tc.workflow), tc.job)
			install := ciStep(job, "Install and verify the Nix hash tool")
			contains(t, install, "sudo apt-get install --no-install-recommends -y nix-bin", "nix-hash --type sha256 --flat --base32 /dev/null")
			absent(t, install, "continue-on-error:", "if:", "|| true")
			stepBefore(t, job, "Install and verify the Nix hash tool", tc.build)
			verify := ciStep(job, "Verify package-manager manifests")
			contains(t, verify,
				"test -s dist/homebrew/Formula/graymatter.rb",
				"test -s dist/scoop/graymatter.json",
				"test -s dist/nix/pkgs/graymatter/default.nix",
				"python3 -m json.tool dist/scoop/graymatter.json",
				"nix-instantiate --parse dist/nix/pkgs/graymatter/default.nix")
			absent(t, verify, "continue-on-error:", "if:", "|| true")
			stepBefore(t, job, tc.build, "Verify package-manager manifests")
			absent(t, job, "TAP_GITHUB_TOKEN", "PRIVATE_KEY", "SSH_PRIVATE_KEY")
		})
	}
	release := ciJob(read(t, "release"), "release")
	upload := ciStep(release, "Preserve package-manager manifests for signed publication")
	contains(t, upload,
		"name: package-manager-manifests-${{ github.ref_name }}",
		"dist/homebrew/Formula/graymatter.rb", "dist/scoop/graymatter.json", "dist/nix/pkgs/graymatter/default.nix",
		"dist/checksums.txt", "dist/metadata.json", "if-no-files-found: error", "archive: true")
	absent(t, upload, "continue-on-error:", "if:")
	stepBefore(t, release, "Verify package-manager manifests", "Preserve package-manager manifests for signed publication")
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
