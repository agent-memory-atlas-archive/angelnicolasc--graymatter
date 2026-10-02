# Publishing a release

GrayMatter publishes two Go modules from the same commit: the library tag
`vX.Y.Z` and the CLI tag `cmd/graymatter/vX.Y.Z`. The CLI's library requirement
must name that same version. Published tags are immutable: never delete,
move, or recreate one to repair a release.

## Prepare and validate

1. Move the completed changelog entries into a dated version section. Preserve
   compatibility notices and leave future default changes conditional.
2. Update the library requirement in `cmd/graymatter/go.mod`, README downloads
   and footer, `server.json`, and the website's download command. Keep the
   published module free of `replace` directives.
3. Run `python .github/scripts/release_check.py --tag vX.Y.Z` and
   `python -m unittest discover -s .github/scripts -p 'test_*.py'`.
4. Open the release preparation PR. CI and Maintenance smoke must pass,
   including the five GoReleaser snapshot targets. Complete any real-client
   gates required by the features being released.

Before the new library tag exists, commands that load the workspace graph may
try to fetch it even though `go.work` selects local source for compilation.
Every CI job that loads that graph therefore applies this temporary replacement
in its disposable checkout before its first Go build, test, or analysis:

```sh
cd cmd/graymatter
GOWORK=off go mod edit -replace github.com/angelnicolasc/graymatter=../..
```

Do not commit that replacement. For local release checks, use a disposable
checkout too. Release builds use the same patched Go toolchain as the current
CI matrix and the CLI's `toolchain` directive.

## Publish and verify

Merge the validated preparation and create signed annotated tags for both
modules at that exact commit. Push the main update and both tags atomically:

```sh
git tag -s vX.Y.Z -m "GrayMatter vX.Y.Z" HEAD
git tag -s cmd/graymatter/vX.Y.Z -m "GrayMatter CLI vX.Y.Z" HEAD
git push --atomic origin HEAD:refs/heads/main refs/tags/vX.Y.Z refs/tags/cmd/graymatter/vX.Y.Z
```

The release workflow checks metadata, matching tag commits and GoReleaser
configuration before publishing. An existing CLI tag is accepted only when it
identifies the same commit; a mismatch fails without changing either tag.

The build's temporary replacement avoids depending on a newly pushed tag's
availability in the public Go proxy. The subsequent install check warms both
modules and the checksum database, then runs the normal `go install` command
and verifies the CLI and embedded library versions without replacements.

Confirm all five archives and `checksums.txt` exist, both release jobs pass,
and Homebrew, Scoop and Nix reference the new release. Retry a failed workflow
against its existing immutable tags when appropriate. If the published source
needs a correction, prepare a new patch version instead.
