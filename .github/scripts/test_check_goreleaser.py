import pathlib
import subprocess
import unittest
from unittest.mock import patch

import check_goreleaser


class ConfigurationCheckTests(unittest.TestCase):
    def result(self, code, output=""):
        return subprocess.CompletedProcess([], code, output, "")

    @patch("check_goreleaser.subprocess.run")
    def test_invalid_config_stops_without_fallback(self, run):
        run.return_value = self.result(1, "field gomod not found in type config.Build")
        self.assertEqual(check_goreleaser.main(), 1)
        self.assertEqual(run.call_count, 1)

    @patch("check_goreleaser.subprocess.run")
    def test_only_known_deprecation_rechecks_remaining_config(self, run):
        output = ("\x1b[33mDEPRECATED: brews should not be used anymore\x1b[0m\n"
                  "configuration is valid, but uses deprecated properties")
        run.side_effect = [self.result(1, output), self.result(0)]
        with patch.object(pathlib.Path, "read_text", return_value=
                          "version: 2\nbrews:\n  - repository: {}\nscoops:\n  - repository: {}\n"):
            self.assertEqual(check_goreleaser.main(), 0)
        self.assertEqual(run.call_count, 2)

    @patch("check_goreleaser.subprocess.run")
    def test_unknown_deprecation_is_not_ignored(self, run):
        run.return_value = self.result(1, "DEPRECATED: something else\n"
                                       "configuration is valid, but uses deprecated properties")
        self.assertEqual(check_goreleaser.main(), 1)
        self.assertEqual(run.call_count, 1)


if __name__ == "__main__":
    unittest.main()
