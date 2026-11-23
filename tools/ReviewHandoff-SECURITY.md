# Review-handoff security model

`tools/Prepare-ReviewHandoff.ps1` prepares source code and evidence for an independent review by an external party. The
package leaves the machine, so the tool is built to leak as little as possible and to fail closed. This document
describes what it checks, what it leaves out, and where it cannot help.

## Principles

1. **Read-only.** Git is only read (`status`, `diff`, `ls-files`, `rev-parse`, `log`). Nothing is staged, committed,
   pushed, reset, checked out, cleaned or stashed, and no source file is edited. External diff drivers, text conversion,
   pagers and fsmonitor hooks are disabled for every Git call, and `GIT_EXTERNAL_DIFF` is removed from the environment.
2. **Every byte that enters the package is inspected**: copied files, the generated patch, test output, evidence,
   `REVIEW.md`, `git-status.txt` and the manifest. Nothing is trusted because of its extension.
3. **Fail closed.** A file or a diff that fails any check is left out and reported by path, rule and line number, never
   by value. There is no flag that overrides a security check, and explicit `-Files` selections go through the same
   checks as the default selection.
4. **Atomic publication.** The package is built in a hidden `.incomplete-<name>-<id>` directory and ZIP; the ZIP and then
   the directory are published by renames that never replace an existing item.
5. **A person decides.** The scan is a heuristic. The final report tells the user to review the package before sharing.

## Pipeline

1. Collect candidates: tracked changes (`git diff -z --name-status`), untracked files (`git status -z`), or the explicit
   `-Files` list. All paths are read NUL-separated, so file names are exact (no console code page is involved).
2. Path checks per candidate: containment inside the repository, reparse points (symbolic links and junctions) on the file
   and every parent directory, reserved Windows names, characters that cannot be written safely into a patch, the
   exclusion rules below.
3. Content checks: size limit, binary detection, then the bounded content scan.
4. Patch: one `git diff --binary` of the tree is split into per-file sections. Sections of files that were not selected
   or were left out are dropped. Each remaining section is scanned in full (added, removed and context lines). Sections of
   new, untracked files are generated from the bytes that were already scanned.
5. Logs: `-RunChecks` output and `-TestLogs` are scanned line by line. Flagged lines are replaced by a marker; the rest of
   the output and the exit codes are kept.
6. Evidence and logs: must be inside the repository or inside a root named with `-AllowedRoot`. An allowed root is only
   trusted when it is an ordinary directory: the root itself and every parent up to the drive root are checked for
   junctions and symbolic links, and so is every directory between the root and the file. A root that is, or lies behind,
   a reparse point is rejected, because "inside the root" would then mean "somewhere else". (The repository root is the
   directory Git resolved and is not subject to the parent check.) Text is scanned; images are only accepted with
   `-AllowImages` because they cannot be scanned.
7. Local paths: the Windows user name, the profile path and the temporary directory are replaced by `<user>`,
   `<USERPROFILE>` and `<TEMP>`; the repository root is shown as `<repo>`.
8. Final scan: every file of the staging directory is scanned once more. If anything is flagged, nothing is published
   (exit code 3).
9. ZIP (UTF-8 entry names, forward slashes) under a temporary name, then renames.

## Exclusion categories

Every exclusion is reported in `REVIEW.md` with its reason. Rules are anchored; a word such as `build`, `dist`,
`coverage` or `credentials` in a path never hides a file by itself.

| Category | What is excluded | Why |
|---|---|---|
| Sensitive file names | `.env`, `.env.*` (except `.env.example`, `.sample`, `.template`, `.dist`, `.defaults`), `*.env`, `.envrc`; `*.pem`, `*.key`, `*.pfx`, `*.p12`, `*.jks`, `*.keystore`, `*.ppk`, `*.crt`, `*.cer`, `*.der`, `*.p7b`, `*.asc`, `*.gpg`, `*.kdbx`; `id_rsa`, `id_dsa`, `id_ecdsa`, `id_ed25519`; `credentials` and `credentials.<data extension>`; `secrets.<data extension>`; `service-account*.json`; `.npmrc`, `.pypirc`, `.netrc`, `.pgpass`, `.git-credentials`, `.dockercfg`, `kubeconfig`; `*.tfstate`, `*.tfvars`; `*.sqlite`, `*.db`, `*.dump`, `*.bak` | Files that normally hold credentials or private data. `credentials.go` and other code files are not affected. |
| Generated or tooling directories | `node_modules/` and `.git/` at any depth; `web/dist/`, `web/coverage/`, `web/tsconfig.tsbuildinfo`, `bin/`, `.vite/`, `.idea/`, `.vscode/`, `.claude/`; hidden `.incomplete-*` staging items | Build output and tooling, not source. A tracked file elsewhere is never excluded by its directory name. |
| Handoff output | Untracked content of the output directory (default `review-handoff/`, or a custom `-OutputRoot` inside the repository). The tracked `review-handoff/README.md` is source and is included. | A package must not contain earlier packages. |
| Review artifacts (default only) | Untracked archives and patches (`.zip`, `.patch`, `.7z`, `.gz`, `.tar`, `.tgz`, `.rar`), an untracked root `repository/` copy, untracked loose root files of an unknown kind. `-Files` or `-IncludeArtifacts` lifts this category only. | Unrelated artifacts of earlier work. |
| Unsafe paths | Reparse points (symbolic links, junctions); paths resolving outside the repository; `..` segments and absolute paths in `-Files`; reserved Windows names (`CON`, `NUL`, `COM1`...), trailing dots; names with control characters, quotes or backslashes | Containment and Windows safety. |
| Binary and unsupported formats | Anything with NUL bytes that is not UTF-16 text, known archive, image, executable and document formats | They cannot be scanned reliably. |
| Limits | Files above `-MaxFileKB` (2048), more than `-MaxFiles` (2000) files, more than `-MaxTotalMB` (50) in total | Bounded resources. |
| Secret-like content | Any file, diff section, log line or evidence file the scanner flags | See below. |

## Secret detection

The scanner is compiled in memory from C# that ships with Windows PowerShell 5.1 (no module, no download). It works line
by line; lines longer than 4,000 characters are scanned in overlapping 4,000-character windows. Every pattern has upper
bounds, no pattern nests unbounded quantifiers, every regular expression has a 300 ms timeout and a timeout counts as a
finding.

Detected (by rule name in reports): private keys and certificates (whole PEM blocks); well-known token formats (AWS, GitHub,
Slack, Stripe, Google API, GitLab, npm, SendGrid, JWT, Azure account keys, Slack webhooks); URLs with a user and a password;
`Authorization` headers and Bearer tokens; npm `_auth`, `_authToken` and `_password` settings; `Password=` and `Pwd=` in
connection strings; JSON, YAML, `.properties`, code and environment-style assignments to names containing password,
passwd, pwd, passphrase, secret, token, api key, access key, private key, client secret, auth key, signing key or
credential (quoted, unquoted, with escaped quotes, joined by `+`, or split over two lines after `=` or `:`), and `PASS=`
style names; base64, base64url and hex tokens that decode to text containing any of the above; long random-looking tokens
(entropy of 4.5 bits per character or more over 120 or more characters).

Values that are clearly placeholders or references (`${VAR}`, `<token>`, `process.env...`, `example`, `changeme`, all the
same character, words such as `password`) are not flagged, so ordinary source is not hidden.

### Limitations (read this)

The scanner does not claim to find every secret. Known gaps:

- Obfuscation: shifted or reversed characters, custom encodings, encryption, compressed or archived content.
- Secrets that are only recognisable from context or knowledge (a short human-chosen password in prose, a key without any
  keyword or format).
- Assignments split over more than two lines, or built at run time from several variables.
- Images and other binary content (screenshots can show secrets). Binary files are left out; images are only accepted
  with `-AllowImages` and are listed as unscanned.
- Placeholders are not flagged by design, so a real secret that looks like one (for example one containing the word
  `example`) is missed.
- It also has false positives: a test fixture with a realistic fake password is left out and must be reviewed by hand.

Always read `REVIEW.md` ("Security scan" and "Left out of this package") and the contents of the package before sharing it.

## Atomic publication and existing packages

Staging items are named `.incomplete-<name>-<id>` (directory) and `.incomplete-<name>-<id>.zip`. They are only renamed to
their final names after the final scan and the ZIP are complete. `File.Move` and `Directory.Move` never replace an existing
item, so a collision (for example two concurrent runs with one name) fails one run instead of overwriting. An interrupted
run leaves only hidden `.incomplete-*` items. The run's own staging items are removed when it fails normally; after a
hard kill they stay for the user to inspect or delete.

## Exit codes

| Code | Meaning |
|---|---|
| 0 | Package created; every check that was run passed |
| 1 | Error (nothing published) |
| 2 | Package created, but a `-RunChecks` check failed or timed out. `REVIEW.md` starts with a "VERIFICATION FAILED" banner. |
| 3 | Package withheld: the final scan found secret-like content (paths and rules are printed, never values) |

## Tests

`tools/review-handoff-tests/Test-ReviewHandoff.ps1` runs the script against isolated temporary repositories with synthetic
fixtures (`Fixtures.ps1`; secret-like values are built from fragments so the test files themselves pass the scanner). Test
ids map to the findings of the independent review: H1 (secret formats), H2 (secrets in patches), M1 (performance), M2/L6
(links and paths), M3/L2 (Unicode), M4 (exclusions), M5/L5/L8 (check output, exit codes, timeouts, reserved names), L1
(atomic packages), L3 (diff drivers), L4 (output directories), L7 (CLAUDE.md). Privilege-dependent tests report `NOT-RUN`.
`Measure-ReviewHandoff.ps1` reproduces the benchmark cases of the review.

## Benchmarks

Reproduce with `tools/review-handoff-tests/Measure-ReviewHandoff.ps1 -OldScript <copy of the first version>`. Each row is
a whole run of the script in a separate process (about 2 s of every run is PowerShell start-up and compiling the scanner),
on synthetic content, Windows PowerShell 5.1, one machine; the first version was stopped after 120 s.

| Case | First version | Current version |
|---|---|---|
| one line of 5,000 letters | 2.3 s | 2.1 s |
| one line of 10,000 letters | 4.0 s | 2.0 s |
| one line of 20,000 letters | 9.7 s | 2.2 s |
| one line of 40,000 letters | 32.9 s | 2.2 s |
| one minified-style line of 40,000 characters | 2.1 s | 2.1 s |
| 4,000 ordinary short lines | 1.9 s | 2.1 s |
| 600 ordinary files | 61.0 s | 5.2 s |
| 400 files with one 60,000-letter line each | not finished in 120 s (the review measured more than 9 minutes) | 13.0 s |

The first version grew quadratically with the length of a line; the current scanner is linear (windows of 4,000
characters) and starts new-file patches in memory instead of one `git` process per file. A 200,000-character line is
covered by a regression test (`M1-02`).

## Limits

| Limit | Default | Parameter |
|---|---|---|
| Largest file scanned and copied | 2,048 KB | `-MaxFileKB` |
| Largest number of files copied | 2,000 | `-MaxFiles` |
| Largest package content | 50 MB | `-MaxTotalMB` |
| Time limit per `-RunChecks` check | 600 s | `-CheckTimeoutSeconds` |
| Time limit per Git call | 120 s | fixed |
| Window of the line scanner | 4,000 characters (300 overlap) | fixed |
| Timeout of one regular expression | 300 ms (a timeout is a finding) | fixed |

Binary and unsupported formats are left out. Large repositories with thousands of changed files should be reviewed in
several packages with `-Files`.
