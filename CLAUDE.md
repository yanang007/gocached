# gocached

Self-hosted Go build cache for intranet CI: a `GOCACHEPROG` client (`gocached prog`) and an HTTP cache server (`gocached serve`) that shares the server host's own `GOCACHE`. Module: `github.com/yanang007/gocached`.

## Conventions

- All documents committed to the repo (specs, tickets, README, ADRs, code comments) are written in English. The one exception is `README.zh-CN.md`, a Chinese translation of `README.md`: keep the two in sync whenever the README changes.
- One commit per ticket. Tickets are GitHub issues; reference the issue as `(#N)` at the end of the subject, e.g. `✨ feat: local-disk GOCACHEPROG client (#2)`, and add a `Closes #N` trailer so pushing to `master` closes it. Commits that span several issues (e.g. review fixes) reference each one with `Refs #N` in the body.
- Keep history linear: rebase or fast-forward, never merge commits. Branch work (e.g. agent worktrees) is rebased onto the target branch before landing. Before delivery (push/PR), clean the branch up: linearize any merges, and squash follow-up test/docs/fix commits into the ticket commit they belong to, so each ticket is exactly one commit.
- Commit messages follow [gitmoji](https://gitmoji.dev): `<emoji> <type>: <summary>`, e.g. `✨` new feature, `🐛` bug fix, `✅` tests, `📝` docs, `♻️` refactor, `🔧` config, `👷` CI, `📦` build/release, `🎉` initial commit.

## Agent skills

### Issue tracker

GitHub issues on `yanang007/gocached`, via the `gh` CLI. A spec is a parent issue; its tickets are sub-issues with native blocked-by links. See `docs/agents/issue-tracker.md`.

### Triage labels

Default five canonical roles (`needs-triage`, `needs-info`, `ready-for-agent`, `ready-for-human`, `wontfix`). See `docs/agents/triage-labels.md`.

### Domain docs

Single-context: `GLOSSARY.md` + `docs/adr/` at the repo root, created lazily. See `docs/agents/domain.md`.
