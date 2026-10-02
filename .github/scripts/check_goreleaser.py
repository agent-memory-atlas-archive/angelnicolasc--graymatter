"""Validate the pinned GoReleaser config, allowing only its known brews warning."""

import pathlib
import re
import subprocess
import tempfile


def main():
    result = subprocess.run(["goreleaser", "check"], capture_output=True, text=True)
    output = result.stdout + result.stderr
    print(output, end="")
    if result.returncode == 0:
        return 0
    plain = re.sub(r"\x1b\[[0-?]*[ -/]*[@-~]", "", output)
    if (plain.count("DEPRECATED:") != 1
            or not re.search(r"DEPRECATED:\s*brews\s*should not be used anymore", plain)
            or "configuration is valid, but uses deprecated properties" not in plain):
        return result.returncode
    # v2.17.1 treats the existing formula publisher's deprecation as an error.
    # Check everything else again, without changing the actual build config.
    source = pathlib.Path(".goreleaser.yml").read_text(encoding="utf-8")
    config, count = re.subn(r"(?ms)^brews:\n.*?(?=^scoops:)", "", source)
    if count != 1:
        raise SystemExit("Expected exactly one brews block before scoops")
    print("::warning file=.goreleaser.yml::Known brews deprecation; validating all remaining settings")
    with tempfile.TemporaryDirectory() as directory:
        path = pathlib.Path(directory) / "goreleaser.yml"
        path.write_text(config, encoding="utf-8")
        return subprocess.run(["goreleaser", "check", str(path)]).returncode


if __name__ == "__main__":
    raise SystemExit(main())
