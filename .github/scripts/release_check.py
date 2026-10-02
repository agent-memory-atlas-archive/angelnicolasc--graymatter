#!/usr/bin/env python3
"""Check release metadata without resolving modules or modifying the checkout."""

import argparse
import datetime
import json
from pathlib import Path
import re
import subprocess
import sys


MODULE = "github.com/angelnicolasc/graymatter"
VERSION = r"(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)(?:-[0-9A-Za-z.-]+)?"


class CheckError(Exception):
    """An actionable release precondition failed."""


def read(root, path):
    try:
        return (root / path).read_text(encoding="utf-8-sig")
    except (OSError, UnicodeError) as exc:
        raise CheckError(f"{path}: {exc}") from exc


def check_changelog(source, tag=None):
    headings = list(re.finditer(r"^##[ \t]+([^\n]+)$", source, re.MULTILINE))
    if tag is None:
        for heading in headings:
            label = re.match(r"\[([^\]]+)\]", heading.group(1))
            if label and label.group(1).lower() != "unreleased":
                tag = "v" + label.group(1)
                break
        if tag is None:
            raise CheckError("CHANGELOG.md: no release heading found")
    if re.fullmatch("v" + VERSION, tag) is None:
        raise CheckError(f"invalid release tag: {tag!r}; expected vMAJOR.MINOR.PATCH")
    version = tag[1:]
    matches = [
        (index, heading) for index, heading in enumerate(headings)
        if re.match(r"\[" + re.escape(version) + r"\](?:\s|$)", heading.group(1))
    ]
    if len(matches) != 1:
        raise CheckError(f"CHANGELOG.md: expected exactly one [{version}] section")
    index, heading = matches[0]
    dated = re.fullmatch(r"\[" + re.escape(version) + r"\] - (\d{4}-\d{2}-\d{2})[ \t]*", heading.group(1))
    if dated is None:
        raise CheckError(f"CHANGELOG.md: [{version}] needs an exact YYYY-MM-DD release date")
    try:
        datetime.date.fromisoformat(dated.group(1))
    except ValueError as exc:
        raise CheckError(f"CHANGELOG.md: invalid release date {dated.group(1)!r}") from exc
    end = headings[index + 1].start() if index + 1 < len(headings) else len(source)
    section = re.sub(r"<!--.*?-->", "", source[heading.end():end], flags=re.DOTALL)
    content = [line.strip() for line in section.splitlines()]
    if not any(line and not line.startswith("#") and not re.fullmatch(r"[-*_ ]+", line) for line in content):
        raise CheckError(f"CHANGELOG.md: [{version}] has no release notes")
    return tag


def check_readme(source, tag):
    prefix = "https://" + MODULE + "/releases/download/"
    downloads = re.findall(re.escape(prefix) + r"([^\s<>\)\]\"']+)", source)
    if not downloads:
        raise CheckError("README.md: no versioned release download URLs found")
    for download in downloads:
        parts = download.split("/", 1)
        if len(parts) != 2 or parts[0] != tag:
            raise CheckError(f"README.md: stale or invalid download URL {prefix + download}")
        if not parts[1].startswith("graymatter_" + tag[1:] + "_"):
            raise CheckError(f"README.md: download asset version does not match {tag}: {parts[1]}")
    footers = re.findall(r"^\*GrayMatter[^\n]*\*[ \t]*$", source, re.MULTILINE)
    if len(footers) != 1 or re.search(r"(?<![\w.])" + re.escape(tag) + r"(?![\w.+-])", footers[0]) is None:
        raise CheckError(f"README.md: expected one GrayMatter footer advertising {tag}")
    versions = re.findall(r"\bv" + VERSION, footers[0])
    if versions != [tag]:
        raise CheckError(f"README.md: ambiguous footer release version: {versions}")


def go_tokens(source):
    # Preserve quoted strings while stripping line comments. This handles the
    # single-line and block forms of Go module directives without invoking Go.
    pattern = r'"(?:\\.|[^"\\])*"|`[^`]*`|//[^\n]*|[()]|[^\s()"`]+'
    for line in source.splitlines():
        tokens = []
        for match in re.finditer(pattern, line):
            token = match.group(0)
            if token.startswith("//"):
                break
            if token.startswith('"'):
                try:
                    token = json.loads(token)
                except ValueError as exc:
                    raise CheckError("cmd/graymatter/go.mod: invalid quoted token") from exc
            elif token.startswith("`"):
                token = token[1:-1]
            tokens.append(token)
        if tokens:
            yield tokens


def check_module(source, tag):
    requirements = []
    in_require = False
    for tokens in go_tokens(source):
        if tokens[0] == "replace":
            raise CheckError("cmd/graymatter/go.mod: durable replace directives break versioned go install")
        if tokens[0] == "require":
            tokens = tokens[1:]
            if tokens == ["("]:
                in_require = True
                continue
        elif not in_require:
            continue
        if tokens == [")"]:
            in_require = False
            continue
        if tokens and tokens[0] == MODULE:
            requirements.append(tokens[1] if len(tokens) == 2 else "<invalid requirement>")
    if requirements != [tag]:
        raise CheckError(f"cmd/graymatter/go.mod: root require must be exactly {MODULE} {tag}; found {requirements}")


def check_server(source, tag):
    try:
        manifest = json.loads(source)
    except ValueError as exc:
        raise CheckError(f"server.json: invalid JSON: {exc}") from exc
    version = manifest.get("version") if isinstance(manifest, dict) else None
    if version != tag[1:]:
        raise CheckError(f"server.json: version must be {tag[1:]!r}; found {version!r}")


def git(root, *args, allow_missing=False):
    try:
        result = subprocess.run(["git", "-C", str(root), *args], capture_output=True, text=True, timeout=30)
    except (OSError, subprocess.TimeoutExpired) as exc:
        raise CheckError(f"git {' '.join(args)}: {exc}") from exc
    if result.returncode and not (allow_missing and result.returncode == 1):
        detail = result.stderr.strip() or result.stdout.strip() or f"exit {result.returncode}"
        raise CheckError(f"git {' '.join(args)}: {detail}")
    return result.stdout.strip() if result.returncode == 0 else None


def check_tags(root, tag):
    head = git(root, "rev-parse", "--verify", "HEAD^{commit}")
    root_ref = "refs/tags/" + tag
    root_commit = git(root, "rev-parse", "--verify", root_ref + "^{commit}")
    if root_commit != head:
        raise CheckError(f"root tag {tag} points to {root_commit}, not HEAD {head}")
    cli_ref = "refs/tags/cmd/graymatter/" + tag
    if git(root, "show-ref", "--verify", "--quiet", cli_ref, allow_missing=True) is not None:
        cli_commit = git(root, "rev-parse", "--verify", cli_ref + "^{commit}")
        if cli_commit != head:
            raise CheckError(f"local CLI tag cmd/graymatter/{tag} points to {cli_commit}, not HEAD {head}")
    # Query both exact refs so annotated tags are compared by their peeled
    # commit, rather than by the distinct annotated tag object. No fetch/write.
    remote = git(root, "ls-remote", "--tags", "origin", cli_ref, cli_ref + "^{}")
    refs = {}
    for line in remote.splitlines():
        fields = line.split()
        if len(fields) != 2 or fields[1] not in (cli_ref, cli_ref + "^{}"):
            raise CheckError("origin returned an invalid CLI tag advertisement")
        refs[fields[1]] = fields[0]
    cli_commit = refs.get(cli_ref + "^{}", refs.get(cli_ref))
    if cli_commit is not None and cli_commit != head:
        raise CheckError(f"remote CLI tag cmd/graymatter/{tag} points to {cli_commit}, not HEAD {head}")


def check_release(root, tag=None, check_git_tags=False):
    tag = check_changelog(read(root, "CHANGELOG.md"), tag)
    check_readme(read(root, "README.md"), tag)
    check_module(read(root, "cmd/graymatter/go.mod"), tag)
    check_server(read(root, "server.json"), tag)
    if check_git_tags:
        check_tags(root, tag)
    return tag


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--tag", help="release tag; defaults to the first versioned changelog heading")
    parser.add_argument("--root", type=Path, default=Path(__file__).resolve().parents[2], help="repository checkout")
    parser.add_argument("--check-tags", action="store_true", help="require root tag at HEAD and reject divergent local/remote CLI tags")
    args = parser.parse_args(argv)
    try:
        tag = check_release(args.root, args.tag, args.check_tags)
    except CheckError as exc:
        print(f"Release preflight failed: {exc}", file=sys.stderr)
        return 1
    print(f"Release preflight passed: {tag}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
