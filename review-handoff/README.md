# review-handoff

Generated review packages for independent code reviews. Everything in this directory except this file is ignored by
Git and must never be committed.

Create a package from the repository root (details and options: `Get-Help .\tools\Prepare-ReviewHandoff.ps1 -Full`):

```powershell
.\tools\Prepare-ReviewHandoff.ps1 -Name F1d-accessibility-followup -Files web/src/features/operations `
    -Summary 'What changed and why.'
```

Each package `<name>/` has `REVIEW.md`, `changes.patch`, `files/`, `tests.log`, `git-status.txt` and `MANIFEST.txt`
(plus `evidence/` when requested), and `<name>.zip` is created next to it.

What the tool guarantees, and what it does not:

- It only reads Git. It never stages, commits, pushes, resets or edits source files.
- Every file, the complete patch, test output and the finished package are scanned for credentials. A file or diff that
  fails is left out and reported by path, rule and line number, never by value. There is no override.
- Packages are built in hidden `.incomplete-*` staging items and published by renames. An interrupted run never leaves a
  package that looks complete, and an existing package or ZIP is never overwritten (a repeated name gets a numeric
  suffix).
- Local user names and temporary paths are replaced in everything that is written.
- The scan is a heuristic. It does not find every secret. **Review each package before sharing it with an external
  service.**

Exit codes: `0` success; `2` package created but a `-RunChecks` check failed or timed out; `3` package withheld because
the final scan found secret-like content; `1` any other error.

The security model, exclusion categories, limits and benchmarks are in `tools/ReviewHandoff-SECURITY.md`. Regression
tests: `tools/review-handoff-tests/Test-ReviewHandoff.ps1`.
