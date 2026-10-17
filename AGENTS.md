# interbase-go

See `README.md` for prerequisites, build and test commands, and
`docs/production-hardening.md` for the execution contract and repeatable
checks.

## No sensitive information in commits (required)

This repository is public. Never commit sensitive information in code,
tests, fixtures, testdata, comments, documentation or commit messages:

- credentials, passwords, keys, tokens, connection strings or certificates;
- host, server, database or file-path names of real systems;
- customer, company or personal names;
- SQL, procedure/trigger source, schema, table, column, index or generator
  names copied from a real (non-fixture) database, and any data from one.

Tests and fixtures use synthetic, neutral names and values only. When a bug
is found on a real database, reproduce it with an equivalent synthetic
fixture instead of copying its SQL or names. Before every commit, review the
staged diff for such content; if anything slips into history, stop and report
it rather than pushing.

## Work tracking — Beads (required)

Tracked work lives in Beads (`bd`, issue prefix `ibg`); run `bd prime` for
the command reference. The database lives on the shared Dolt server at
`~/.beads/shared-server/`, not in the repo, so issue updates never create
commits. Issue history syncs to `refs/dolt/data` on `origin`: writes
auto-push (`dolt.auto-push`), and `bd dolt pull` fetches changes made
elsewhere.

- **History:** `docs/TODO.md` is historical; its last state is commit
  `a6151db` (`git show a6151db:docs/TODO.md`), and its items are deferred
  issues. Specs and plans in `docs/superpowers/` are reference/evidence
  documents, not live checklists. Do not recreate a TODO file.
- **Find work:** `bd ready`, `bd list` (filter by `--status`, `--label`),
  `bd search`, `bd show`. Respect dependencies.
- **Start:** `bd update <id> --claim` (sets `in_progress`) and record the plan
  with `--design` before writing code.
- **Complete:** only after the acceptance criteria are verified (tests, vet,
  native harnesses as applicable). Check each one (`[x]`) with
  `bd update <id> --acceptance`, then `bd close <id> --reason` with the final
  summary (what changed, commits, how it was verified).
- **Defer or put off:** `bd update <id> --status deferred --append-notes`
  with the reason, what was done so far, and what would unblock it. A
  deferred issue must never be left `in_progress`.
- **Partial work or new findings:** record progress with `--append-notes`;
  create new issues (`bd create`) for follow-ups or bugs discovered, linked
  with `--deps discovered-from:<id>` or `bd dep add`.
- Name the issue in the commit message of code done for it. Do not commit
  merely to synchronize tracking.

<!-- BEGIN BEADS INTEGRATION v:1 profile:minimal hash:46cd31e7 -->
## Beads Issue Tracker

This project uses **bd (beads)** for issue tracking. Run `bd prime` to see full workflow context and commands.

### Quick Reference

```bash
bd ready              # Find available work
bd show <id>          # View issue details
bd update <id> --claim  # Claim work
bd close <id>         # Complete work
```

### Rules

- Use `bd` for ALL task tracking — do NOT use TodoWrite, TaskCreate, or markdown TODO lists
- Run `bd prime` for detailed command reference and session close protocol
- Use `bd remember` for persistent knowledge — do NOT use MEMORY.md files

**Architecture in one line:** issues live in a local Dolt DB; sync uses `refs/dolt/data` on your git remote; `.beads/issues.jsonl` is a passive export. See https://github.com/gastownhall/beads/blob/main/docs/core-concepts/sync-concepts.md for details and anti-patterns.

## Agent Context Profiles

The managed Beads block is task-tracking guidance, not permission to override repository, user, or orchestrator instructions.

- **Conservative (default)**: Use `bd` for task tracking. Do not run git commits, git pushes, or Dolt remote sync unless explicitly asked. At handoff, report changed files, validation, and suggested next commands.
- **Minimal**: Keep tool instruction files as pointers to `bd prime`; use the same conservative git policy unless active instructions say otherwise.
- **Team-maintainer**: Only when the repository explicitly opts in, agents may close beads, run quality gates, commit, and push as part of session close. A current "do not commit" or "do not push" instruction still wins.

## Session Completion

This protocol applies when ending a Beads implementation workflow. It is subordinate to explicit user, repository, and orchestrator instructions.

1. **File issues for remaining work** - Create beads for anything that needs follow-up
2. **Run quality gates** (if code changed) - Tests, linters, builds
3. **Update issue status** - Close finished work, update in-progress items
4. **Handle git/sync by active profile**:
   ```bash
   # Conservative/minimal/default: report status and proposed commands; wait for approval.
   git status

   # Team-maintainer opt-in only, unless current instructions forbid it:
   git pull --rebase
   bd dolt push
   git push
   git status
   ```
5. **Hand off** - Summarize changes, validation, issue status, and any blocked sync/commit/push step

**Critical rules:**
- Explicit user or orchestrator instructions override this Beads block.
- Do not commit or push without clear authority from the active profile or the current user request.
- If a required sync or push is blocked, stop and report the exact command and error.
<!-- END BEADS INTEGRATION -->

<!-- BEGIN BEADS CODEX SETUP: generated by bd setup codex -->
## Beads Issue Tracker

Use Beads (`bd`) for durable task tracking in repositories that include it. Use the `beads` skill at `.agents/skills/beads/SKILL.md` (project install) or `~/.agents/skills/beads/SKILL.md` (global install) for Beads workflow guidance, then use the `bd` CLI for issue operations.

### Quick Reference

```bash
bd ready                # Find available work
bd show <id>            # View issue details
bd update <id> --claim  # Claim work
bd close <id>           # Complete work
bd prime                # Refresh Beads context
```

### Rules

- Use `bd` for all task tracking; do not create markdown TODO lists.
- Run `bd prime` when Beads context is missing or stale. Codex 0.129.0+ can load Beads context automatically through native hooks; use `/hooks` to inspect or toggle them.
- Keep persistent project memory in Beads via `bd remember`; do not create ad hoc memory files.

**Architecture in one line:** issues live in a local Dolt DB; sync uses `refs/dolt/data` on your git remote; `.beads/issues.jsonl` is a passive export. See https://github.com/gastownhall/beads/blob/main/docs/core-concepts/sync-concepts.md for details and anti-patterns.
<!-- END BEADS CODEX SETUP -->
