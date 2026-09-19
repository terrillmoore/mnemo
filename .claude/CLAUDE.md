# mnemo — notes for Claude Code sessions

## Where this repository lives

`mcci/tools/claude/mnemo` on gitlab-x is authoritative. Work happens here:
branches, merge requests, pipelines, and the package registry the archive
host installs from. `github.com/terrillmoore/mnemo` is the public copy, kept
current by the `mirror:github` job on every push to `main` and on tags, and it
is the route for anything bound upstream to `Pilan-AI/mnemo`. Never commit to
the GitHub copy: the mirror push is fast-forward only and a commit there stops
the job rather than being merged quietly.

Remotes on a working clone: `origin` gitlab-x, `github` the public copy,
`upstream` Pilan-AI.

## Shipping a build

`mnemo` is not installed from a release or a tap. The archive host installs
what the pipeline publishes, so publishing is the act that changes a machine.

1. Merge to `main`. `vet`, `test` and `build:linux-amd64` run on every
   pipeline; nothing is published by merging.
2. Run the `publish` job by hand. It uploads the linux/amd64 binary and its
   sha256 to the project's generic package registry, named for what
   `git describe` said: a tag if the pipeline ran on one, otherwise the
   commit.
3. Put that version into `claude_archive_mnemo_version` in
   `mcci/sysadmin/infrastructure`, in an MR that says why, and run the play.
   The role downloads from the registry with a read-only deploy token and
   checks the sha256. Nothing in CI reaches the archive.

Upstream's Homebrew tap and its GoReleaser workflow belong to
`Pilan-AI/mnemo` and are not part of this path. `RELEASING.md` and
`.github/workflows/release.yml` describe that flow, not ours.

## Multi-machine archive

This fork exists mainly to support one searchable history across several
machines: the `host` column, `--host` at index time, `MNEMO_DB`, `mnemo serve`
opening the database read-only under an ssh forced command, and `mnemo
migrate host`. README.md, "Several machines, one history", describes the shape.

Nothing here configures a deployment, and no skill in this repo can be
installed unmodified. Each person builds their own: databases, keys, transfer
scripts, and a skill that tells an agent what the machine it is running on can
reach. Terry's is installed from his private `personal-claude-context` repo,
which carries the workstation half: `mnemo-push`, `mnemo-pull`, the systemd
timers, and the skill.

The archive host is built by `mcci/sysadmin/infrastructure`:
`ansible/roles/claude-archive` for the account, the encrypted volume, the
indexer and its timer, `terraform/archive.tf` for the VM, and
`docs/claude-archive.md` for what to do by hand and why. Its "Keys" section
defines the three forced commands, and `claude_archive_sources` in
`ansible/inventory/group_vars/archive_hosts/main.yml` lists each machine and
the keys it holds. Setting one up for another person is a new Terraform module
block and their own account and volume, named for them throughout, not another
directory under Terry's.

If you are an agent on one of Terry's machines and need history from another
machine, read the installed skill at `~/.claude/skills/mnemo/SKILL.md` first.
Its opening section says whether this machine can reach the archive, and how.
Do not assume it can: a machine with a push key only can write to the archive
and cannot read it back, by design, and no amount of local searching will find
another machine's sessions.

## Other context

- Module path: `github.com/0xRaghu/mnemo` (note the `0xRaghu`, not `Pilan-AI`).
  The `go build -ldflags -X ...cmd.Version=` injection in the formula depends
  on this exact path.
- Homebrew tap: `Pilan-AI/homebrew-tap`, formula at `Formula/mnemo.rb`.
- License is AGPL-3.0-or-later (see `LICENSE`), not MIT — `CONTRIBUTING.md` has
  a stale MIT mention that should get fixed opportunistically.
