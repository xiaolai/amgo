# Release scripts

These scripts are public so the build and packaging steps can be audited and
reproduced. **No script stores a credential.** The Developer ID, notarytool
profile, `.env`, `gh` login and tap push access live outside the repository.

| Script | Credentials needed | Purpose |
|---|---|---|
| `secret-scan.sh` | no | Scan the publication set and Git history with gitleaks; used by CI and `make secrets` |
| `build-release.sh` | no | Cross-compile the four release binaries with the injected version |
| `checksums.sh` | no | Write `SHA256SUMS` for the release binaries |
| `package-release.sh` | no | Build deterministic `.tar.gz` archives with `LICENSE` and `README.md` |
| `homebrew-formula.sh` | no | Render `Formula/amgo.rb` from the archive checksums |
| `tap-version-guard.sh` | no | Refuse a tap update that is equal to or older than the current formula |
| `license-check.sh` | no | Fail when a used module is missing from `THIRD-PARTY-NOTICES.md`, has no license file, or is listed but unused |
| `perf-record.sh` | no | Record startup, send cycle, contention, peak RSS and residual process count |
| `release-check.sh` | yes | Verify binary provenance, run govulncheck, and run the native delivery gates; spends model tokens |
| `sign-notarize.sh` | yes | Developer ID signing and Apple notarisation of the two darwin binaries |
| `publish-release.sh` | yes | Create the GitHub release, verify anonymous downloads, update the Homebrew tap, and dispatch the clean-Mac check |

The order and the local setup are in `dev-docs/releasing.md`, which is
maintainer material and not tracked. The public security policy is
[`SECURITY.md`](../SECURITY.md); report vulnerabilities through private
advisories, not a public issue.

`publish-release.sh` refuses before any network write unless the local and
remote tag both point at `HEAD`, the tree is clean, the release-check report
matches every binary's sha256 and commit, and both darwin binaries carry a
Developer ID signature with the hardened runtime and an Accepted notarisation
of their current bytes.
