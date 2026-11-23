# Instructions for Claude Code in this repository

## Independent review handoff

Prepare a review package only when the user relays an actual request for an independent review or for specific review
artifacts (source files, diffs, logs, test output, screenshots or similar). Do not trigger on a generic request for
"evidence", on a question about the code, or when a summary or a diff shown in the conversation is enough. If it is not
clear what the reviewer asked for, ask which files and evidence are needed before preparing anything. Do not repeat a
package when nothing changed since the last one: reuse it.

1. **Identify exactly what is needed.** List the files, diffs, logs and evidence the request names.
2. **Choose the selection.** The default selection is every modified, staged and untracked source file of the working
   tree plus tracked deletions, which is usually more than a narrow review needs. Prefer `-Files <paths>` for a
   specific request and `-BaseRef <revision>` for work that is already committed.
3. **Run the script** from the repository root:

   ```powershell
   .\tools\Prepare-ReviewHandoff.ps1 -Name <Phase-short-topic> -Files <paths> `
       -Summary '<what changed and why>' -Scope '<what is in and out of scope>' `
       -Decisions '<key decisions>' -Limitations '<known limitations>'
   ```

4. **Evidence.** Use `-RunChecks` only when the reviewer asked for verification evidence and the sources changed since
   the last verification: it runs the whole frontend suite (minutes) and rewrites ignored build output. Otherwise pass
   the latest real output with `-TestLogs`. Set `-CheckTimeoutSeconds` for a slow check. Never describe a check as
   passing unless `tests.log` shows exit code 0 for it. Exit code 2 means a check failed or timed out; exit code 3 means
   the package was withheld because the final scan found secret-like content.
5. **Review the package before reporting it.** Open `REVIEW.md` and read "Security scan" and "Left out of this
   package"; list `files/`; confirm that nothing sensitive and no local path is there. The scanner is a heuristic and
   does not find every secret.
6. **Report** the package path and the ZIP path (both under `review-handoff/`), what the package contains, anything that
   was requested but could not be included or was left out (path and reason, never a secret value), and remind the user
   to review the package before sharing it with an external service.

Rules that always apply:

- Git is only read. Do not stage, commit, push, reset, clean, stash or switch branches to prepare a package, and do not
  edit source files for it.
- Never work around the scanner: a file that is left out stays out. Ask the user instead of copying it by hand.
- `review-handoff/` is generated output and is ignored by Git, except its README. Never commit packages. Existing
  packages and the untracked review artifacts in the repository root are never modified or deleted.
- Transferring the package to ChatGPT or any other service is manual: Claude Code cannot upload it. Give the user the
  ZIP path.
- The security model and limits are in `tools/ReviewHandoff-SECURITY.md`; the regression tests are in
  `tools/review-handoff-tests/`.
- Keep all repository content and documentation in English.
