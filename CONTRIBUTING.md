# Contributing to CloudBurrow

Thank you for improving CloudBurrow. Contributions come as pull requests to
[cloudburrow/cloudburrow](https://github.com/cloudburrow/cloudburrow).

## Before you start

- Work is tracked by issue on the [project board](https://github.com/orgs/cloudburrow/projects/1),
  one issue per pull request. Open or pick an issue first.
- Read [AGENTS.md](AGENTS.md): it covers scope, testing expectations, and the project's rule
  against claiming compatibility you have not verified with an official Google SDK.
- To report a vulnerability, follow [SECURITY.md](SECURITY.md), not a public issue or pull request.

## Licence of contributions (inbound = outbound)

CloudBurrow is licensed under the [MIT License](LICENSE). Releases v0.1.0 to v0.5.0 were first
published under the Apache License 2.0, and their CloudBurrow code is now available under MIT too. By opening a pull request you agree that your contribution is
licensed under the same MIT License as the rest of CloudBurrow's own code. No separate contributor
licence agreement is needed.

Third-party code keeps its own licence. If your change copies, vendors or links third-party code,
record it in [NOTICE](NOTICE), [dependencies.json](dependencies.json) and the matching
`PROVENANCE.md`, with its licence files. GPL, LGPL and AGPL code is never accepted into the
repository or linked into the CLI; `go test ./test/repo/` refuses it.

## Developer Certificate of Origin

Every commit must be signed off under the
[Developer Certificate of Origin 1.1](https://developercertificate.org/): by adding a
`Signed-off-by:` line you certify that you wrote the change, or otherwise have the right to submit
it under the project's licence. Sign off with:

```sh
git commit -s
```

which adds `Signed-off-by: Your Name <you@example.com>` with the name and email of your Git
configuration. Use your real name. To sign off commits you already made on your branch, run
`git commit --amend -s` for the last one, or `git rebase --signoff main` for all of them, and
push the branch again.

## Pull requests

- Keep each pull request to its issue, and say which (`Closes #N`).
- Run what your change touches before you push: `go build ./...`, `go vet ./...`, the unit tests
  of the packages you changed, `go test ./test/repo/...`, and `make docs-check`.
- Never mark an operation Verified in [docs/compatibility.md](docs/compatibility.md) without an
  official-SDK compat test that passes.
- Add an entry under **Unreleased** in [CHANGELOG.md](CHANGELOG.md) for each user-visible change:
  what a user can now do or no longer hits, naming the API, flag or console page and the issue.
  CI plumbing and test-only fixes need none.

## Landings

Work reaches `main` through landings: one `land/N` branch that merges the reviewed branches with
merge commits, and one CI run for all of them. **Every landing adds the CHANGELOG.md entries under
Unreleased for the user-visible changes it lands**, in the Keep a Changelog headings the file uses
(Added, Changed, Fixed), unless each merged branch already brought its own. Write them for users,
one per feature or fix, and claim only what docs/compatibility.md supports. A scheduled release
commits nothing to `CHANGELOG.md`: `scripts/release-notes.sh` builds its **What's new** from the
entries added under Unreleased since the previous tag, and falls back to the titles of the issues
the landings closed only for changes that have no entry. A landing with no entries makes a
release whose notes are a list of issue titles. "Fix main CI" pull requests need an entry only for a
product bug they fix, and the notes keep them in the collapsed **All landings** list.
