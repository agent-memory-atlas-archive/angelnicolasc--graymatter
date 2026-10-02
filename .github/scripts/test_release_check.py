"""Offline regression checks for the release preflight."""

import contextlib
import io
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest
from unittest import mock

import release_check as check


TAG = "v0.20.0"
CHANGELOG = "## [Unreleased]\n\n## [0.20.0] - 2026-10-02\n\n### Added\n\n- New workbench.\n\n## [0.19.1] - 2026-09-01\n\n- Previous release.\n"
README = "Download https://github.com/angelnicolasc/graymatter/releases/download/v0.20.0/graymatter_0.20.0_windows_amd64.zip\n\n*GrayMatter — v0.20.0 — October 2026*\n"
MODULE = "module github.com/angelnicolasc/graymatter/cmd/graymatter\n\ngo 1.25.5\n\nrequire (\n\tgithub.com/angelnicolasc/graymatter v0.20.0\n)\n"


class ReleaseFixture(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name) / "checkout with spaces"
        (self.root / "cmd/graymatter").mkdir(parents=True)
        self.write("CHANGELOG.md", CHANGELOG)
        self.write("README.md", README)
        self.write("cmd/graymatter/go.mod", MODULE)
        self.write("server.json", '{"version": "0.20.0"}\n')

    def write(self, path, text):
        (self.root / path).write_text(text, encoding="utf-8")


class ReleaseCheckTests(ReleaseFixture):
    def test_explicit_and_inferred_version_without_git(self):
        with mock.patch.object(check, "git", side_effect=AssertionError("Git must be opt-in")):
            self.assertEqual(check.check_release(self.root), TAG)
            self.assertEqual(check.check_release(self.root, TAG), TAG)

    def test_module_requirement_mismatch(self):
        for source in [MODULE.replace(TAG, "v0.19.1"), MODULE.replace("\tgithub.com/angelnicolasc/graymatter v0.20.0\n", "")]:
            with self.subTest(source=source):
                self.write("cmd/graymatter/go.mod", source)
                with self.assertRaisesRegex(check.CheckError, "root require must be exactly"):
                    check.check_release(self.root, TAG)

    def test_single_line_quoted_requirement_and_comments(self):
        source = 'require "github.com/angelnicolasc/graymatter" "v0.20.0" // indirect\n// replace ignored comment\n'
        check.check_module(source, TAG)

    def test_durable_replace_rejected(self):
        for replacement in ["replace github.com/angelnicolasc/graymatter => ../..", "replace (\n example.com/other => ../other\n)"]:
            with self.subTest(replacement=replacement):
                self.write("cmd/graymatter/go.mod", MODULE + replacement)
                with self.assertRaisesRegex(check.CheckError, "durable replace"):
                    check.check_release(self.root, TAG)

    def test_missing_and_stale_readme_downloads(self):
        for source in ["*GrayMatter — v0.20.0 — October 2026*\n", README.replace("download/v0.20.0", "download/v0.19.1"), README.replace("graymatter_0.20.0", "graymatter_0.19.1")]:
            with self.subTest(source=source):
                self.write("README.md", source)
                with self.assertRaisesRegex(check.CheckError, "README.md"):
                    check.check_release(self.root, TAG)

    def test_missing_stale_and_prefix_footer(self):
        for source in [README.split("\n\n")[0], README.replace("— v0.20.0", "— v0.19.1"), README.replace("— v0.20.0", "— v0.20.01")]:
            with self.subTest(source=source):
                self.write("README.md", source)
                with self.assertRaisesRegex(check.CheckError, "footer"):
                    check.check_release(self.root, TAG)

    def test_exact_nonempty_dated_changelog_section(self):
        invalid = [
            CHANGELOG.replace("[0.20.0]", "[0.20.01]"),
            CHANGELOG.replace("[0.20.0]", "[0.19.0]"),
            CHANGELOG.replace(" - 2026-10-02", ""),
            CHANGELOG.replace("2026-10-02", "2026-02-30"),
            CHANGELOG.replace("- New workbench.", "<!-- Release notes go here. -->\n---"),
            CHANGELOG + "\n## [0.20.0] - 2026-10-02\n- Duplicate.\n",
        ]
        for source in invalid:
            with self.subTest(source=source):
                self.write("CHANGELOG.md", source)
                with self.assertRaisesRegex(check.CheckError, "CHANGELOG.md"):
                    check.check_release(self.root, TAG)

    def test_inferred_bad_first_release_does_not_fall_back(self):
        self.write("CHANGELOG.md", CHANGELOG.replace("2026-10-02", "TBD"))
        with self.assertRaisesRegex(check.CheckError, "release date"):
            check.check_release(self.root)

    def test_cli_root_and_error_exit(self):
        with contextlib.redirect_stdout(io.StringIO()) as output:
            self.assertEqual(check.main(["--root", str(self.root), "--tag", TAG]), 0)
        self.assertIn(TAG, output.getvalue())
        self.write("cmd/graymatter/go.mod", MODULE.replace(TAG, "v0.19.1"))
        with contextlib.redirect_stderr(io.StringIO()) as errors:
            self.assertEqual(check.main(["--root", str(self.root)]), 1)
        self.assertIn("root require", errors.getvalue())

    def test_server_manifest_version_and_shape(self):
        for source in ['{"version":"0.19.1"}', '{}', '[]', '{']:
            with self.subTest(source=source):
                self.write("server.json", source)
                with self.assertRaisesRegex(check.CheckError, "server.json"):
                    check.check_release(self.root, TAG)


@unittest.skipUnless(shutil.which("git"), "Git unavailable")
class ReleaseTagTests(ReleaseFixture):
    def setUp(self):
        super().setUp()
        self.git("init", "-q")
        self.git("config", "user.name", "Release fixture")
        self.git("config", "user.email", "fixture@example.invalid")
        self.git("config", "commit.gpgsign", "false")
        self.git("config", "tag.gpgsign", "false")
        self.git("add", ".")
        self.git("commit", "-qm", "fixture")
        self.git("tag", "-a", TAG, "-m", "root release")
        remote = Path(self.temporary.name) / "remote.git"
        subprocess.run(["git", "init", "--bare", "-q", str(remote)], check=True, capture_output=True)
        self.git("remote", "add", "origin", str(remote))

    def git(self, *args):
        return subprocess.run(["git", "-C", str(self.root), *args], check=True, capture_output=True, text=True).stdout.strip()

    def advance(self):
        self.write("next.txt", "next commit\n")
        self.git("add", "next.txt")
        self.git("commit", "-qm", "next fixture")
        self.git("tag", "-f", TAG)

    def test_absent_cli_tag_is_allowed(self):
        self.assertEqual(check.check_release(self.root, TAG, True), TAG)

    def test_matching_annotated_local_and_remote_cli_tag(self):
        cli_tag = "cmd/graymatter/" + TAG
        self.git("tag", "-a", cli_tag, "-m", "CLI release")
        self.git("push", "origin", "refs/tags/" + cli_tag)
        self.assertEqual(check.check_release(self.root, TAG, True), TAG)

    def test_wrong_root_tag(self):
        self.git("commit", "--allow-empty", "-qm", "new head")
        with self.assertRaisesRegex(check.CheckError, "root tag .* not HEAD"):
            check.check_release(self.root, TAG, True)

    def test_wrong_local_cli_tag(self):
        self.git("tag", "cmd/graymatter/" + TAG)
        self.advance()
        with self.assertRaisesRegex(check.CheckError, "local CLI tag .* not HEAD"):
            check.check_release(self.root, TAG, True)

    def test_wrong_remote_cli_tag_even_when_local_missing(self):
        cli_tag = "cmd/graymatter/" + TAG
        self.git("tag", "-a", cli_tag, "-m", "previous CLI release")
        self.git("push", "origin", "refs/tags/" + cli_tag)
        self.git("tag", "-d", cli_tag)
        self.advance()
        with self.assertRaisesRegex(check.CheckError, "remote CLI tag .* not HEAD"):
            check.check_release(self.root, TAG, True)

    def test_remote_failure_is_not_missing_tag(self):
        self.git("remote", "set-url", "origin", str(self.root / "missing.git"))
        with self.assertRaisesRegex(check.CheckError, "ls-remote"):
            check.check_release(self.root, TAG, True)


if __name__ == "__main__":
    unittest.main()
