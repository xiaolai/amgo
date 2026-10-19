# Publishing checklist

The public tree is the set of files Git tracks. `.gitignore` keeps local
material out of normal commits, but it is not a security boundary: `git add -f`
bypasses it, and a secret that reaches one commit stays in history after it is
deleted. The checks below operate on the publication set and on history.

## Before the first push

1. **Review what would be published.**

   ```sh
   git status --short --ignored
   git add -A
   git diff --cached --name-only
   ```

   The staged list must contain only source, tests, docs and build metadata.
   It must not contain `.env`, `auth.json`, key files, `dev-docs/`,
   `.cc-suite/`, agent state or editor state. Never use `git add -f` on an
   ignored path.

2. **Run the secret scan.**

   ```sh
   make secrets
   ```

   This copies the publication set (tracked plus untracked non-ignored files)
   into a private temporary directory and runs gitleaks with the project rules
   in `.gitleaks.toml`:

   - credential formats from the gitleaks default set;
   - private, link-local and CGNAT IPv4 addresses;
   - absolute personal home paths;
   - internal-only hostname suffixes.

   A developer may keep project-specific host or account names in
   `.gitleaks.local.toml`. That file is ignored, merged only for local scans,
   and must never be committed.

   It then scans the full Git history. A finding blocks publication. The scan
   redacts matches; do not paste a matched value into an issue or chat.

3. **Read the staged diff.** The scan cannot judge context. Look for internal
   architecture notes, customer names, private URLs, tunnel or overlay
   addresses, and comments that name a host or account.

4. **Check runtime credential handling.** Tests may stage credentials into a
   mode-0600 temporary home at runtime, but no test may write a credential into
   a tracked file, a fixture snapshot, a log or a failure message. The native
   tests in `tests/native/` delete their temporary homes on completion.

5. **Commit and push.** Branch protection should require the `secret-scan` and
   `test` jobs before merge.

## GitHub repository settings

These are manual settings; the workflow cannot enable them:

- **Secret scanning** and **push protection** on, for the repository and for
  all organization members.
- **Dependabot alerts** and security updates on.
- **Branch protection** for the default branch: require pull requests, require
  the `secret-scan` and `test` checks, and restrict force pushes.
- **Actions permissions**: allow only selected actions, or at minimum keep the
  default `GITHUB_TOKEN` read-only. The workflow already declares
  `permissions: contents: read`.
- **Environment and deployment secrets**: never expose them to pull requests
  from forks; use OIDC instead of long-lived cloud keys.

Pinned action revisions in `.github/workflows/ci.yml` should be refreshed
deliberately, not by floating tags.

## If a secret is found

1. **Revoke or rotate it first.** Removing the commit does not un-leak a value
   that was pushed; assume it is compromised.
2. Remove it from the working tree and from history with `git filter-repo` or
   an equivalent history rewrite, then force-push and ask collaborators to
   re-clone.
3. Check CI logs, release artifacts, caches and forks. GitHub may retain
   cached views of the commit.
4. Record the incident and the rotation in the private operations notes, not in
   this repository.

## What is intentionally excluded

| Path | Reason |
|---|---|
| `.env`, `.env.*` | local credentials and endpoints; `.env.example` and `.env.sample` are allowed templates |
| `dev-docs/` | local plans, research evidence and audit material |
| `.cc-suite/` | agent and audit runner state |
| `.claude/`, `.codex/`, `.agents/` | local agent configuration and caches |
| `dist/`, `work/`, `tmp/` | build and scratch output |
| `*.key`, `*.pem`, `auth.json`, `credentials.json` | credential material; rejected by `scripts/secret-scan.sh` if forced into the index |
