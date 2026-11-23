<#
.SYNOPSIS
    Prepares a self-contained review package (and a ZIP) for an independent code review of RelayForge.

.DESCRIPTION
    Collects the modified, staged and untracked source files of the working tree (or an explicit list), a complete
    patch, HEAD and Git status, optional test evidence, and a readable REVIEW.md, into review-handoff/<Name>/ plus
    review-handoff/<Name>.zip.

    Safety model (details: tools/ReviewHandoff-SECURITY.md):
      * Git is only read. Nothing is staged, committed, pushed, reset, checked out, cleaned or stashed, and no source
        file is edited. External diff drivers, text conversion, pagers and fsmonitor hooks are disabled.
      * Every file that could enter the package is checked: path containment and reparse points (symbolic links and
        junctions), reserved Windows names, precise generated-directory rules, sensitive file names, size and binary
        limits, and a bounded content scan for credentials (private keys, tokens, JSON/YAML/properties assignments,
        connection strings, Bearer headers, npm tokens, URLs with passwords, and base64, hex or concatenated forms).
      * The complete final patch is scanned too, so a secret that a change REMOVED, or the content of a DELETED
        credential file, never reaches changes.patch. A file or diff that fails is left out and reported by path, rule
        and line number, never by value.
      * Output of -RunChecks and supplied logs is scanned and redacted line by line.
      * Every file of the finished package is scanned once more before it is published.
      * The package is built in a hidden staging directory and published by renames, so an interrupted run never
        leaves a package that looks complete, and an existing package or ZIP is never overwritten.
      * The scan is a heuristic. It does not find every secret. A person must review the package before it is shared.

    Exit codes: 0 success; 2 package created but a -RunChecks check failed or timed out; 3 package withheld because
    the final scan found secret-like content; 1 any other error.

.PARAMETER Name
    Package name, for example "F1d-accessibility-followup". Letters, digits, dot, dash and underscore (at most 80).
    Windows reserved device names (CON, NUL, COM1 ...) and trailing dots are refused.

.PARAMETER Files
    Explicit repository-relative paths (files or directories). Only these are considered. Absolute paths, "..",
    reparse points and everything that fails a security check are rejected and reported; there is no override.

.PARAMETER BaseRef
    A Git revision to compare against instead of HEAD (for committed work). The patch covers BaseRef..working tree.

.PARAMETER Summary
.PARAMETER Scope
.PARAMETER Decisions
.PARAMETER Limitations
.PARAMETER RequestedMaterial
    Text for the matching sections of REVIEW.md (one or more lines each). Scanned like everything else.

.PARAMETER NotIncluded
    Material the reviewer asked for that could not be included, with the reason.

.PARAMETER TestLogs
    Existing test or verification output files, copied into tests.log after redaction.

.PARAMETER RunChecks
    Runs format:check, lint, typecheck, test and build in web/ with the dependencies that are already installed. Real
    exit codes are recorded; output is redacted. A failed or timed-out check makes the script exit with code 2.

.PARAMETER CheckTimeoutSeconds
    Per-check time limit for -RunChecks (default 600). A check that exceeds it is killed with its child processes.

.PARAMETER Evidence
    Additional files (logs, screenshots, traces) copied into evidence/. They must be inside the repository or inside
    a root named with -AllowedRoot. Text evidence is scanned (a file that fails is left out) and local paths are
    redacted. Images are only accepted with -AllowImages because they cannot be scanned.

.PARAMETER AllowedRoot
    Extra directories that -Evidence and -TestLogs may read from, in addition to the repository. Nothing outside the
    repository and these roots is ever read.

.PARAMETER AllowImages
    Allows PNG/JPEG/GIF/WebP evidence. They are copied unscanned and listed as such in REVIEW.md.

.PARAMETER IncludeArtifacts
    Also considers the review artifacts that are left out by default (archives, patches, loose root files). Security
    checks still apply.

.PARAMETER MaxFileKB
    Largest file that is scanned and copied (default 2048). Larger files are left out.

.PARAMETER MaxTotalMB
    Largest total size of the package contents (default 50).

.PARAMETER MaxFiles
    Largest number of files copied (default 2000).

.PARAMETER OutputRoot
    Where packages are created. Defaults to review-handoff/ in the repository root. When it is inside the repository,
    its untracked contents are never re-included in a later package.

.PARAMETER RepoRoot
    The repository to read. Defaults to the repository that contains this script.

.EXAMPLE
    .\tools\Prepare-ReviewHandoff.ps1 -Name F1d-accessibility-followup -Files web/src/features/operations `
        -Summary 'Focus moves to the feed heading after Show newest.'

.EXAMPLE
    .\tools\Prepare-ReviewHandoff.ps1 -Name F1c-delivery-detail -BaseRef 01bb106 -RunChecks
#>
[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)]
    [ValidatePattern('^[A-Za-z0-9][A-Za-z0-9._-]{0,79}$')]
    [string]$Name,
    [string[]]$Files = @(),
    [string]$BaseRef = 'HEAD',
    [string[]]$Summary = @(),
    [string[]]$Scope = @(),
    [string[]]$Decisions = @(),
    [string[]]$Limitations = @(),
    [string[]]$RequestedMaterial = @(),
    [string[]]$NotIncluded = @(),
    [string[]]$TestLogs = @(),
    [switch]$RunChecks,
    [ValidateRange(1, 86400)]
    [int]$CheckTimeoutSeconds = 600,
    [string[]]$Evidence = @(),
    [string[]]$AllowedRoot = @(),
    [switch]$AllowImages,
    [switch]$IncludeArtifacts,
    [ValidateRange(1, 102400)]
    [int]$MaxFileKB = 2048,
    [ValidateRange(1, 4096)]
    [int]$MaxTotalMB = 50,
    [ValidateRange(1, 100000)]
    [int]$MaxFiles = 2000,
    [string]$OutputRoot = '',
    [string]$RepoRoot = ''
)

Set-StrictMode -Version 2.0
$ErrorActionPreference = 'Stop'

$ScannerVersion = '2'
$MaxFileBytes = [int64]$MaxFileKB * 1KB
$MaxTotalBytes = [int64]$MaxTotalMB * 1MB
$Utf8NoBom = New-Object System.Text.UTF8Encoding($false)
$GitTimeoutMs = 120000

# ===============================================================================================================
# The content scanner. Compiled in memory with the C# compiler that ships with Windows PowerShell 5.1 (no module,
# no download). Every rule is bounded: lines are cut into windows of 4000 characters, quantifiers have upper limits,
# no pattern nests unbounded quantifiers, every regular expression has a 300 ms timeout and a timeout counts as a
# finding. Findings carry a rule name and line numbers, never the matched text.
# ===============================================================================================================
$scannerSource = @'
using System;
using System.Collections.Generic;
using System.IO;
using System.Security.Cryptography;
using System.Text;
using System.Text.RegularExpressions;

namespace RelayForge.HandoffV2
{
    public sealed class Finding
    {
        public int Line;
        public int EndLine;
        public string Rule;
    }

    internal sealed class Rule
    {
        public string Name;
        public int Kind;
        public string[] Triggers;
        public Regex Re;
    }

    public static class Scanner
    {
        private const int ChunkSize = 4000;
        private const int Overlap = 300;
        private const int MaxMatchesPerRule = 25;
        private static readonly TimeSpan Limit = TimeSpan.FromMilliseconds(300);
        private static readonly Regex JoinLiterals = new Regex("[\"']\\s{0,3}\\+\\s{0,3}[\"']", RegexOptions.CultureInvariant, Limit);
        private static readonly List<Rule> Rules = BuildRules();
        private static readonly string[] PlaceholderParts = new string[] {
            "${", "{{", "<%", "example", "sample", "dummy", "placeholder", "changeme", "change-me", "change_me",
            "redacted", "your-", "your_", "yourpassword", "xxxx", "****", "....", "process.env", "getenv",
            "os.environ", "secrets.", "env." };
        private static readonly string[] PlaceholderWords = new string[] {
            "password", "passwd", "pass", "secret", "token", "null", "nil", "none", "true", "false", "undefined",
            "empty", "string", "required", "optional", "unset", "user", "username", "admin", "root" };
        private static readonly string[] BenignKeyEnds = new string[] {
            "file", "path", "name", "type", "length", "label", "placeholder", "hint", "field", "header" };

        private static Regex Make(string pattern, bool ignoreCase)
        {
            RegexOptions o = RegexOptions.CultureInvariant;
            if (ignoreCase) o |= RegexOptions.IgnoreCase;
            return new Regex(pattern, o, Limit);
        }

        private static Rule Add(List<Rule> list, string name, int kind, string[] triggers, string pattern, bool ignoreCase)
        {
            Rule r = new Rule();
            r.Name = name;
            r.Kind = kind;
            r.Triggers = triggers;
            r.Re = Make(pattern, ignoreCase);
            list.Add(r);
            return r;
        }

        private static List<Rule> BuildRules()
        {
            List<Rule> l = new List<Rule>();
            Add(l, "known-token-format", 0,
                new string[] { "AKIA", "ASIA", "gh", "github_pat_", "xox", "sk_", "sk-", "AIza", "glpat-", "npm_", "eyJ", "hooks.slack.com", "SG.", "AccountKey" },
                @"AKIA[0-9A-Z]{16}|ASIA[0-9A-Z]{16}|\bgh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,}|\bxox[abprs]-[A-Za-z0-9-]{10,}|\bsk_(?:live|test)_[A-Za-z0-9]{16,}|\bsk-[A-Za-z0-9]{20,}|AIza[0-9A-Za-z_\-]{30,}|glpat-[A-Za-z0-9_\-]{15,}|\bnpm_[A-Za-z0-9]{30,}|eyJ[A-Za-z0-9_\-]{8,}\.eyJ[A-Za-z0-9_\-]{8,}\.[A-Za-z0-9_\-]{8,}|hooks\.slack\.com/services/[A-Za-z0-9/]{20,}|SG\.[A-Za-z0-9_\-]{16,}\.[A-Za-z0-9_\-]{16,}|AccountKey=[A-Za-z0-9+/=]{30,}",
                false);
            Add(l, "url-with-password", 2,
                new string[] { "://" },
                @"[a-z][a-z0-9+.\-]{1,20}://(?<user>[^/\s:@'""`]{0,64}):(?<pw>[^/\s@'""`]{1,128})@",
                true);
            Add(l, "authorization-header", 4,
                new string[] { "bearer", "authorization" },
                @"\bauthorization\b[^\r\n]{0,12}?(?:bearer|basic|token|digest)\s{1,3}(?<v>[A-Za-z0-9._~+/=\-]{8,512})",
                true);
            Add(l, "bearer-token", 4,
                new string[] { "bearer" },
                @"\bbearer\s{1,3}(?<v>[A-Za-z0-9._~+/=\-]{20,512})",
                true);
            Add(l, "npm-auth-setting", 5,
                new string[] { "_auth", "_password" },
                @"_(?:auth|authToken|password)\s{0,3}=\s{0,3}(?<v>[^\s]{8,200})",
                true);
            Add(l, "connection-string-password", 3,
                new string[] { "pwd", "passw" },
                @"(?:password|pwd)\s{0,3}=\s{0,3}(?<v>[^;\s""'`]{4,100})",
                true);
            Add(l, "secret-assignment", 1,
                new string[] { "passw", "pwd", "secret", "token", "key", "credential" },
                @"(?<key>[A-Za-z0-9_.\-\[\]]{0,50}(?:passw(?:or)?d|pwd|passphrase|secret|token|api[_\-.]?key|access[_\-.]?key|private[_\-.]?key|client[_\-.]?secret|auth[_\-.]?key|signing[_\-.]?key|credentials?)[A-Za-z0-9_.\-\[\]]{0,30})\\?[""'`]?\s{0,4}(?::=|=>|:|=)\s{0,4}(?<q>\\?[""'`])?(?<val>[^\s""'`\\,;)}\]<>]{1,200})",
                true);
            Add(l, "pass-assignment", 1,
                new string[] { "pass" },
                @"(?<![A-Za-z0-9])(?<key>(?:[A-Za-z0-9]{1,20}[_\-.])?pass)(?![A-Za-z])\s{0,3}[:=]\s{0,3}(?<q>[""'`])?(?<val>[^\s""'`,;]{1,200})",
                true);
            return l;
        }

        // ---------------------------------------------------------------------------------------------------
        // Public API
        // ---------------------------------------------------------------------------------------------------

        public static List<Finding> ScanText(string text)
        {
            return ScanTextDepth(text, 0);
        }

        /// <summary>Replaces every flagged line with a fixed marker. Line endings are kept.</summary>
        public static string Redact(string text, List<Finding> findings)
        {
            if (findings == null || findings.Count == 0) return text;
            bool[] hit = new bool[CountLines(text) + 2];
            foreach (Finding f in findings)
            {
                int end = Math.Min(f.EndLine, hit.Length - 1);
                for (int i = f.Line; i <= end; i++) hit[i] = true;
            }
            StringBuilder sb = new StringBuilder(text.Length);
            int line = 1;
            int pos = 0;
            while (pos <= text.Length)
            {
                int nl = text.IndexOf('\n', pos);
                int end = nl < 0 ? text.Length : nl;
                bool cr = end > pos && text[end - 1] == '\r';
                if (hit[line])
                {
                    sb.Append("[line withheld: secret-like content]");
                    if (cr) sb.Append('\r');
                }
                else
                {
                    sb.Append(text, pos, end - pos);
                }
                if (nl < 0) break;
                sb.Append('\n');
                pos = nl + 1;
                line++;
            }
            return sb.ToString();
        }

        public static int CountLines(string text)
        {
            int n = 1;
            for (int i = 0; i < text.Length; i++) if (text[i] == '\n') n++;
            return n;
        }

        /// <summary>Decodes UTF-8 or UTF-16 text. Returns false for binary or unsupported content.</summary>
        public static bool TryDecode(byte[] data, out string text, out string reason)
        {
            text = "";
            reason = "";
            if (data.Length == 0) return true;
            if (HasMagic(data))
            {
                reason = "unsupported binary format";
                return false;
            }
            if (data.Length >= 3 && data[0] == 0xEF && data[1] == 0xBB && data[2] == 0xBF)
            {
                text = Encoding.UTF8.GetString(data, 3, data.Length - 3);
                return true;
            }
            if (data.Length >= 2 && data[0] == 0xFF && data[1] == 0xFE)
            {
                text = Encoding.Unicode.GetString(data, 2, data.Length - 2);
                return true;
            }
            if (data.Length >= 2 && data[0] == 0xFE && data[1] == 0xFF)
            {
                text = Encoding.BigEndianUnicode.GetString(data, 2, data.Length - 2);
                return true;
            }
            int nul = 0;
            int nulEven = 0;
            int nulOdd = 0;
            for (int i = 0; i < data.Length; i++)
            {
                if (data[i] == 0)
                {
                    nul++;
                    if ((i & 1) == 0) nulEven++; else nulOdd++;
                }
            }
            if (nul > 0)
            {
                int pairs = data.Length / 2;
                if (pairs > 0 && nulOdd * 10 >= pairs * 3 && nulEven * 20 < pairs)
                {
                    text = Encoding.Unicode.GetString(data);
                    return true;
                }
                if (pairs > 0 && nulEven * 10 >= pairs * 3 && nulOdd * 20 < pairs)
                {
                    text = Encoding.BigEndianUnicode.GetString(data);
                    return true;
                }
                reason = "binary file";
                return false;
            }
            try
            {
                text = new UTF8Encoding(false, true).GetString(data);
            }
            catch (ArgumentException)
            {
                text = Encoding.GetEncoding(28591).GetString(data);
            }
            return true;
        }

        public static string Sha256Hex(string path)
        {
            using (SHA256 sha = SHA256.Create())
            using (FileStream fs = new FileStream(path, FileMode.Open, FileAccess.Read, FileShare.Read))
            {
                byte[] h = sha.ComputeHash(fs);
                StringBuilder sb = new StringBuilder(64);
                foreach (byte b in h) sb.Append(b.ToString("x2"));
                return sb.ToString();
            }
        }

        /// <summary>
        /// .NET Framework writes UTF-8 entry names but leaves the "UTF-8 name" flag (bit 11) unset, so other tools read
        /// the names in a legacy code page. This sets the flag on every entry whose name is not plain ASCII.
        /// </summary>
        public static void MarkUtf8Names(string path)
        {
            byte[] b = File.ReadAllBytes(path);
            int eocd = -1;
            for (int k = b.Length - 22; k >= 0 && k >= b.Length - 22 - 65535; k--)
            {
                if (b[k] == 0x50 && b[k + 1] == 0x4b && b[k + 2] == 5 && b[k + 3] == 6) { eocd = k; break; }
            }
            if (eocd < 0) throw new InvalidDataException("not a ZIP file");
            int entries = BitConverter.ToUInt16(b, eocd + 10);
            int p = (int)BitConverter.ToUInt32(b, eocd + 16);
            for (int e = 0; e < entries; e++)
            {
                if (!(b[p] == 0x50 && b[p + 1] == 0x4b && b[p + 2] == 1 && b[p + 3] == 2)) throw new InvalidDataException("bad central directory");
                int nlen = BitConverter.ToUInt16(b, p + 28);
                int elen = BitConverter.ToUInt16(b, p + 30);
                int clen = BitConverter.ToUInt16(b, p + 32);
                int localOff = (int)BitConverter.ToUInt32(b, p + 42);
                bool nonAscii = false;
                for (int q = 0; q < nlen; q++) { if (b[p + 46 + q] >= 0x80) { nonAscii = true; break; } }
                if (nonAscii)
                {
                    b[p + 9] |= 0x08;
                    b[localOff + 7] |= 0x08;
                }
                p += 46 + nlen + elen + clen;
            }
            File.WriteAllBytes(path, b);
        }

        /// <summary>Splits the output of "git diff" into one byte array per file ("diff --git " starts a section).</summary>
        public static List<byte[]> SplitPatch(byte[] data)
        {
            byte[] marker = Encoding.ASCII.GetBytes("diff --git ");
            List<int> starts = new List<int>();
            for (int i = 0; i + marker.Length <= data.Length; i++)
            {
                if (i != 0 && data[i - 1] != (byte)'\n') continue;
                bool ok = true;
                for (int j = 0; j < marker.Length; j++)
                {
                    if (data[i + j] != marker[j]) { ok = false; break; }
                }
                if (ok) starts.Add(i);
            }
            List<byte[]> parts = new List<byte[]>();
            for (int s = 0; s < starts.Count; s++)
            {
                int from = starts[s];
                int to = s + 1 < starts.Count ? starts[s + 1] : data.Length;
                byte[] part = new byte[to - from];
                Buffer.BlockCopy(data, from, part, 0, part.Length);
                parts.Add(part);
            }
            return parts;
        }

        /// <summary>The "git diff" section of a file that does not exist in the base (text files only).</summary>
        public static byte[] NewFilePatch(string relPath, byte[] content)
        {
            using (MemoryStream ms = new MemoryStream())
            {
                byte[] head = Encoding.UTF8.GetBytes("diff --git a/" + relPath + " b/" + relPath + "\nnew file mode 100644\n");
                ms.Write(head, 0, head.Length);
                if (content.Length > 0)
                {
                    string tab = relPath.IndexOf(' ') >= 0 ? "\t" : "";
                    byte[] names = Encoding.UTF8.GetBytes("--- /dev/null\n+++ b/" + relPath + tab + "\n");
                    ms.Write(names, 0, names.Length);
                    int lines = 0;
                    for (int i = 0; i < content.Length; i++) if (content[i] == (byte)'\n') lines++;
                    bool endsWithNewline = content[content.Length - 1] == (byte)'\n';
                    if (!endsWithNewline) lines++;
                    byte[] hunk = Encoding.ASCII.GetBytes("@@ -0,0 +1" + (lines == 1 ? "" : "," + lines.ToString()) + " @@\n");
                    ms.Write(hunk, 0, hunk.Length);
                    int pos = 0;
                    while (pos < content.Length)
                    {
                        int nl = Array.IndexOf(content, (byte)'\n', pos);
                        int end = nl < 0 ? content.Length : nl;
                        ms.WriteByte((byte)'+');
                        ms.Write(content, pos, end - pos);
                        ms.WriteByte((byte)'\n');
                        pos = end + 1;
                    }
                    if (!endsWithNewline)
                    {
                        byte[] note = Encoding.ASCII.GetBytes("\\ No newline at end of file\n");
                        ms.Write(note, 0, note.Length);
                    }
                }
                return ms.ToArray();
            }
        }

        // ---------------------------------------------------------------------------------------------------
        // Scanning
        // ---------------------------------------------------------------------------------------------------

        private static bool HasMagic(byte[] d)
        {
            if (d.Length < 4) return false;
            if (d[0] == 0x89 && d[1] == 0x50 && d[2] == 0x4E && d[3] == 0x47) return true;
            if (d[0] == 0xFF && d[1] == 0xD8 && d[2] == 0xFF) return true;
            if (d[0] == (byte)'G' && d[1] == (byte)'I' && d[2] == (byte)'F' && d[3] == (byte)'8') return true;
            if (d[0] == (byte)'P' && d[1] == (byte)'K' && d[2] == 3 && d[3] == 4) return true;
            if (d[0] == (byte)'%' && d[1] == (byte)'P' && d[2] == (byte)'D' && d[3] == (byte)'F') return true;
            if (d[0] == 0x1F && d[1] == 0x8B) return true;
            if (d[0] == 0x7F && d[1] == (byte)'E' && d[2] == (byte)'L' && d[3] == (byte)'F') return true;
            if (d[0] == (byte)'M' && d[1] == (byte)'Z') return true;
            if (d[0] == (byte)'R' && d[1] == (byte)'I' && d[2] == (byte)'F' && d[3] == (byte)'F') return true;
            return false;
        }

        private static List<Finding> ScanTextDepth(string text, int depth)
        {
            List<Finding> result = new List<Finding>();
            if (text == null || text.Length == 0) return result;
            List<int[]> blocks = FindKeyBlocks(text, result);
            int line = 1;
            int pos = 0;
            int n = text.Length;
            while (pos <= n)
            {
                int nl = text.IndexOf('\n', pos);
                int end = nl < 0 ? n : nl;
                int len = end - pos;
                if (len > 0 && text[pos + len - 1] == '\r') len--;
                if (len > 0 && !InBlock(blocks, line))
                {
                    string rule = ScanLine(text, pos, len, depth);
                    if (rule == null && nl >= 0 && len <= ChunkSize && EndsWithAssign(text, pos, len))
                    {
                        int nextStart = nl + 1;
                        int nextEnd = text.IndexOf('\n', nextStart);
                        if (nextEnd < 0) nextEnd = n;
                        int nextLen = Math.Min(nextEnd - nextStart, 400);
                        string joined = text.Substring(pos, len) + " " + text.Substring(nextStart, nextLen).TrimStart();
                        rule = ScanChunk(joined.Length > ChunkSize ? joined.Substring(0, ChunkSize) : joined, depth);
                    }
                    if (rule != null)
                    {
                        Finding f = new Finding();
                        f.Line = line;
                        f.EndLine = line;
                        f.Rule = rule;
                        result.Add(f);
                    }
                }
                if (nl < 0) break;
                pos = nl + 1;
                line++;
            }
            result.Sort(delegate (Finding a, Finding b) { return a.Line.CompareTo(b.Line); });
            return result;
        }

        private static bool EndsWithAssign(string text, int pos, int len)
        {
            for (int i = pos + len - 1; i >= pos; i--)
            {
                char c = text[i];
                if (c == ' ' || c == '\t') continue;
                return c == '=' || c == ':';
            }
            return false;
        }

        private static bool InBlock(List<int[]> blocks, int line)
        {
            for (int i = 0; i < blocks.Count; i++)
            {
                if (line >= blocks[i][0] && line <= blocks[i][1]) return true;
            }
            return false;
        }

        // Private keys and certificates: the whole BEGIN..END block is one finding.
        private static List<int[]> FindKeyBlocks(string text, List<Finding> result)
        {
            List<int[]> blocks = new List<int[]>();
            int search = 0;
            int countedTo = 0;
            int countedLine = 1;
            while (blocks.Count < 1000)
            {
                int idx = text.IndexOf("-----BEGIN ", search, StringComparison.Ordinal);
                if (idx < 0) break;
                int hdrEnd = text.IndexOf("-----", idx + 11, StringComparison.Ordinal);
                search = idx + 11;
                if (hdrEnd < 0 || hdrEnd - idx > 90) continue;
                string label = text.Substring(idx + 11, hdrEnd - (idx + 11)).ToUpperInvariant();
                if (label.IndexOf("PRIVATE KEY", StringComparison.Ordinal) < 0 && label.IndexOf("CERTIFICATE", StringComparison.Ordinal) < 0) continue;
                for (; countedTo < idx; countedTo++) if (text[countedTo] == '\n') countedLine++;
                int startLine = countedLine;
                int endIdx = text.IndexOf("-----END ", hdrEnd, StringComparison.Ordinal);
                int stop = endIdx < 0 ? Math.Min(text.Length, hdrEnd + 20000) : endIdx;
                int endLine = startLine;
                for (int i = idx; i < stop; i++) if (text[i] == '\n') endLine++;
                Finding f = new Finding();
                f.Line = startLine;
                f.EndLine = endLine;
                f.Rule = "private-key-or-certificate-block";
                result.Add(f);
                blocks.Add(new int[] { startLine, endLine });
                search = Math.Max(search, stop);
            }
            return blocks;
        }

        private static string ScanLine(string text, int start, int len, int depth)
        {
            if (len <= ChunkSize) return ScanChunk(text.Substring(start, len), depth);
            int step = ChunkSize - Overlap;
            for (int off = 0; off < len; off += step)
            {
                int take = Math.Min(ChunkSize, len - off);
                string rule = ScanChunk(text.Substring(start + off, take), depth);
                if (rule != null) return rule;
                if (off + take >= len) break;
            }
            return null;
        }

        private static string ScanChunk(string chunk, int depth)
        {
            try
            {
                string s = chunk;
                if (s.IndexOf('+') >= 0) s = JoinLiterals.Replace(s, "");
                for (int ri = 0; ri < Rules.Count; ri++)
                {
                    Rule r = Rules[ri];
                    if (!HasTrigger(s, r.Triggers)) continue;
                    Match m = r.Re.Match(s);
                    int count = 0;
                    while (m.Success && count < MaxMatchesPerRule)
                    {
                        if (Evaluate(r, m, s)) return r.Name;
                        m = m.NextMatch();
                        count++;
                    }
                }
                if (s.Length >= 24)
                {
                    string enc = ScanEncoded(s, depth);
                    if (enc != null) return enc;
                }
                return null;
            }
            catch (RegexMatchTimeoutException)
            {
                return "scan-timeout";
            }
        }

        private static bool HasTrigger(string s, string[] triggers)
        {
            if (triggers == null) return true;
            for (int i = 0; i < triggers.Length; i++)
            {
                if (s.IndexOf(triggers[i], StringComparison.OrdinalIgnoreCase) >= 0) return true;
            }
            return false;
        }

        private static bool Evaluate(Rule r, Match m, string s)
        {
            switch (r.Kind)
            {
                case 0:
                    return true;
                case 1:
                    {
                        string key = m.Groups["key"].Value.ToLowerInvariant();
                        for (int i = 0; i < BenignKeyEnds.Length; i++)
                        {
                            if (key.EndsWith(BenignKeyEnds[i], StringComparison.Ordinal)) return false;
                        }
                        Group val = m.Groups["val"];
                        if (IsPlaceholder(val.Value)) return false;
                        Group q = m.Groups["q"];
                        if (q.Success && q.Length > 0)
                        {
                            int endIdx = val.Index + val.Length;
                            char c = endIdx < s.Length ? s[endIdx] : '\0';
                            if (c != '"' && c != '\'' && c != '`' && c != '\\') return false;
                            return LooksSecret(val.Value, true);
                        }
                        return LooksSecret(val.Value, false);
                    }
                case 2:
                    {
                        string user = m.Groups["user"].Value;
                        string pw = m.Groups["pw"].Value;
                        if (IsPlaceholder(pw)) return false;
                        if (string.Equals(user, pw, StringComparison.OrdinalIgnoreCase)) return false;
                        return pw.Length >= 3;
                    }
                case 3:
                    {
                        if (s.IndexOf(';') < 0) return false;
                        string v = m.Groups["v"].Value;
                        return !IsPlaceholder(v) && v.Length >= 6;
                    }
                case 4:
                    {
                        string v = m.Groups["v"].Value;
                        if (IsPlaceholder(v)) return false;
                        bool letter = false;
                        bool digit = false;
                        foreach (char c in v)
                        {
                            if (char.IsLetter(c)) letter = true;
                            if (char.IsDigit(c)) digit = true;
                        }
                        return (letter && digit) || v.Length >= 20;
                    }
                case 5:
                    return !IsPlaceholder(m.Groups["v"].Value);
            }
            return false;
        }

        private static bool IsPlaceholder(string val)
        {
            if (val == null || val.Length == 0) return true;
            string v = val.ToLowerInvariant();
            char c0 = v[0];
            if (c0 == '$' || c0 == '%' || c0 == '{' || c0 == '@' || c0 == '[' || c0 == '<' || c0 == '*' || c0 == '.' || c0 == '#') return true;
            for (int i = 0; i < PlaceholderParts.Length; i++)
            {
                if (v.IndexOf(PlaceholderParts[i], StringComparison.Ordinal) >= 0) return true;
            }
            for (int i = 0; i < PlaceholderWords.Length; i++)
            {
                if (v == PlaceholderWords[i]) return true;
            }
            if (v.EndsWith("()", StringComparison.Ordinal)) return true;
            return false;
        }

        private static bool LooksSecret(string val, bool quoted)
        {
            int len = val.Length;
            if (len < (quoted ? 8 : 12)) return false;
            bool letter = false, digit = false, upper = false, lower = false, symbol = false;
            HashSet<char> distinct = new HashSet<char>();
            foreach (char c in val)
            {
                distinct.Add(c);
                if (char.IsLetter(c)) { letter = true; if (char.IsUpper(c)) upper = true; else lower = true; }
                else if (char.IsDigit(c)) digit = true;
                else symbol = true;
            }
            if (distinct.Count <= 3) return false;
            if (quoted)
            {
                return (letter && digit) || (upper && lower && len >= 12) || (symbol && len >= 10) || len >= 24;
            }
            return (letter && digit) || (upper && lower && len >= 16);
        }

        private static bool IsB64(char c)
        {
            return (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '+' || c == '/' || c == '-' || c == '_';
        }

        private static bool IsHex(char c)
        {
            return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F');
        }

        // Long random-looking tokens, and base64 or hex tokens that decode to text containing a secret.
        private static string ScanEncoded(string s, int depth)
        {
            int i = 0;
            int tokens = 0;
            while (i < s.Length && tokens < 60)
            {
                if (!IsB64(s[i])) { i++; continue; }
                int j = i;
                while (j < s.Length && IsB64(s[j])) j++;
                int k = j;
                while (k < s.Length && k - j < 2 && s[k] == '=') k++;
                int len = j - i;
                if (len >= 24)
                {
                    tokens++;
                    if (len >= 120 && Entropy(s, i, len) >= 4.5) return "high-entropy-blob";
                    if (depth == 0 && len <= 4000)
                    {
                        string decoded = TryBase64(s.Substring(i, k - i));
                        if (decoded != null && ScanTextDepth(decoded, 1).Count > 0) return "encoded-secret-base64";
                        if (len >= 32 && len <= 2000 && (len % 2) == 0 && AllHex(s, i, len))
                        {
                            string hex = TryHex(s, i, len);
                            if (hex != null && ScanTextDepth(hex, 1).Count > 0) return "encoded-secret-hex";
                        }
                    }
                }
                i = k > i ? k : i + 1;
            }
            return null;
        }

        private static bool AllHex(string s, int from, int len)
        {
            for (int i = from; i < from + len; i++) if (!IsHex(s[i])) return false;
            return true;
        }

        private static string TryHex(string s, int from, int len)
        {
            byte[] b = new byte[len / 2];
            for (int i = 0; i < b.Length; i++) b[i] = Convert.ToByte(s.Substring(from + 2 * i, 2), 16);
            return PrintableText(b);
        }

        private static string TryBase64(string token)
        {
            string t = token.Replace('-', '+').Replace('_', '/').TrimEnd('=');
            int pad = (4 - (t.Length % 4)) % 4;
            if (pad == 3) return null;
            t = t + new string('=', pad);
            byte[] bytes;
            try { bytes = Convert.FromBase64String(t); }
            catch (FormatException) { return null; }
            return PrintableText(bytes);
        }

        private static string PrintableText(byte[] bytes)
        {
            if (bytes.Length < 8) return null;
            int printable = 0;
            for (int i = 0; i < bytes.Length; i++)
            {
                byte b = bytes[i];
                if ((b >= 32 && b <= 126) || b == 9 || b == 10 || b == 13) printable++;
            }
            if (printable * 10 < bytes.Length * 9) return null;
            return Encoding.ASCII.GetString(bytes);
        }

        private static double Entropy(string s, int from, int len)
        {
            int[] counts = new int[128];
            for (int i = from; i < from + len; i++) counts[s[i] & 127]++;
            double h = 0;
            for (int i = 0; i < counts.Length; i++)
            {
                if (counts[i] == 0) continue;
                double p = (double)counts[i] / len;
                h -= p * Math.Log(p, 2);
            }
            return h;
        }
    }
}
'@
if (-not ('RelayForge.HandoffV2.Scanner' -as [type])) { Add-Type -TypeDefinition $scannerSource -Language CSharp }

# ===============================================================================================================
# Helpers
# ===============================================================================================================

$script:Root = ''
$script:LocalPathRules = @()

function Write-Utf8([string]$Path, [string]$Text) { [System.IO.File]::WriteAllText($Path, $Text, $Utf8NoBom) }

function Initialize-LocalPathRules {
    # Local user names and temporary locations are replaced in everything that is written to the package.
    $rules = New-Object System.Collections.Generic.List[object]
    $temp = [System.IO.Path]::GetTempPath().TrimEnd('\')
    $profile = $env:USERPROFILE
    foreach ($pair in @(@($temp, '<TEMP>'), @($profile, '<USERPROFILE>'), @($script:Root, '<repo>'))) {
        if ($pair[0]) {
            foreach ($variant in @($pair[0], $pair[0].Replace('\', '/'))) {
                $rules.Add(@([regex]::new([regex]::Escape($variant), 'IgnoreCase'), $pair[1]))
            }
        }
    }
    $rules.Add(@([regex]::new('(?i)([A-Za-z]:[\\/]+Users[\\/]+)[^\\/\s''"<>|]+'), '${1}<user>'))
    $rules.Add(@([regex]::new('(?i)(/[a-z]/Users/)[^/\s''"<>|]+'), '${1}<user>'))
    if ($env:USERNAME -and $env:USERNAME.Length -ge 3) {
        $rules.Add(@([regex]::new('(?i)(?<![A-Za-z0-9])' + [regex]::Escape($env:USERNAME) + '(?![A-Za-z0-9])'), '<user>'))
    }
    $script:LocalPathRules = $rules.ToArray()
}

function Hide-LocalPaths([string]$Text) {
    foreach ($rule in $script:LocalPathRules) { $Text = $rule[0].Replace($Text, $rule[1]) }
    return $Text
}

function Test-ReservedName([string]$Component) {
    if ($Component -eq '') { return $false }
    if ($Component.EndsWith('.') -or $Component.EndsWith(' ')) { return $true }
    $stem = $Component.Split('.')[0].Trim()
    return ($stem -match '^(?i:CON|PRN|AUX|NUL|CLOCK\$|COM[0-9]|LPT[0-9])$')
}

function Invoke-GitProcess {
    param([string[]]$GitArgs, [byte[]]$StdinBytes = $null, [int[]]$OkExit = @(0))
    $psi = New-Object System.Diagnostics.ProcessStartInfo
    $psi.FileName = 'git'
    # Read-only, no pager, no external diff, no text conversion helpers, no fsmonitor hook, literal pathspecs.
    $fixed = @('--no-optional-locks', '--no-pager', '--literal-pathspecs', '-c', 'core.quotePath=false', '-c', 'core.fsmonitor=false',
        '-c', 'core.pager=cat', '-c', 'diff.external=', '-c', 'diff.renames=false', '-C', $script:Root)
    $all = $fixed + $GitArgs
    $psi.Arguments = ($all | ForEach-Object { if ($_ -match '[\s"]' -or $_ -eq '') { '"' + ($_ -replace '"', '\"') + '"' } else { $_ } }) -join ' '
    $psi.UseShellExecute = $false
    $psi.RedirectStandardInput = ($null -ne $StdinBytes)
    $psi.RedirectStandardOutput = $true
    $psi.RedirectStandardError = $true
    $psi.CreateNoWindow = $true
    foreach ($var in 'GIT_EXTERNAL_DIFF', 'GIT_PAGER', 'GIT_DIFF_OPTS', 'GIT_TRACE', 'GIT_TRACE_PACKET') {
        if ($psi.EnvironmentVariables.ContainsKey($var)) { $psi.EnvironmentVariables.Remove($var) }
    }
    $proc = [System.Diagnostics.Process]::Start($psi)
    $ms = New-Object System.IO.MemoryStream
    $outTask = $proc.StandardOutput.BaseStream.CopyToAsync($ms)
    $errTask = $proc.StandardError.ReadToEndAsync()
    if ($null -ne $StdinBytes) {
        $proc.StandardInput.BaseStream.Write($StdinBytes, 0, $StdinBytes.Length)
        $proc.StandardInput.Close()
    }
    if (-not $proc.WaitForExit($GitTimeoutMs)) {
        try { $proc.Kill() } catch { }
        throw "git $($GitArgs[0]) did not finish within $($GitTimeoutMs / 1000) seconds"
    }
    $outTask.Wait()
    if ($OkExit -notcontains $proc.ExitCode) {
        throw "git $($GitArgs[0]) failed with exit code $($proc.ExitCode): $($errTask.Result.Trim())"
    }
    return [pscustomobject]@{ Bytes = $ms.ToArray(); ExitCode = $proc.ExitCode }
}

function Get-GitText([string[]]$GitArgs, [int[]]$OkExit = @(0)) {
    return $Utf8NoBom.GetString((Invoke-GitProcess -GitArgs $GitArgs -OkExit $OkExit).Bytes)
}

function Split-Nul([string]$Text) {
    # The leading comma keeps an empty result an (empty) array instead of $null under strict mode.
    return , @($Text.Split([char]0) | Where-Object { $_ -ne '' })
}

function Test-ReparseChain([string]$Full, [string]$Boundary) {
    # True when the item or any parent directory below $Boundary is a symbolic link or a junction.
    $cur = $Full
    $limit = $Boundary.TrimEnd('\')
    while ($cur.Length -gt $limit.Length) {
        if (Test-Path -LiteralPath $cur) {
            $attr = [System.IO.File]::GetAttributes($cur)
            if (($attr -band [System.IO.FileAttributes]::ReparsePoint) -ne 0) { return $true }
        }
        $parent = [System.IO.Path]::GetDirectoryName($cur)
        if ($null -eq $parent -or $parent -eq $cur) { break }
        $cur = $parent
    }
    return $false
}

function Test-AllowedRootUnsafe([string]$Root) {
    # An -AllowedRoot is only trusted when the directory itself and every parent up to the drive root are ordinary
    # directories. A junction or symbolic link anywhere on that path would make "inside the root" mean "somewhere else".
    # (The repository root is not checked here: it is the working directory that Git resolved.)
    $cur = $Root.TrimEnd('\')
    while ($cur.Length -gt 0) {
        if (Test-Path -LiteralPath $cur) {
            if (([System.IO.File]::GetAttributes($cur) -band [System.IO.FileAttributes]::ReparsePoint) -ne 0) { return $true }
        }
        $parent = [System.IO.Path]::GetDirectoryName($cur)
        if ($null -eq $parent -or $parent -eq $cur) { break }
        $cur = $parent
    }
    return $false
}

function Test-UnderRoot([string]$Full, [string]$RootDir) {
    $r = $RootDir.TrimEnd('\') + '\'
    return $Full.StartsWith($r, [System.StringComparison]::OrdinalIgnoreCase)
}

# ===============================================================================================================
# Repository, limits, rules
# ===============================================================================================================

if ($RepoRoot -eq '') { $RepoRoot = Split-Path -Parent (Split-Path -Parent $MyInvocation.MyCommand.Path) }
$script:Root = (Resolve-Path -LiteralPath $RepoRoot).Path
$top = (Get-GitText -GitArgs @('rev-parse', '--show-toplevel')).Trim()
if ($top -eq '') { throw "Not a Git repository: $script:Root" }
$script:Root = (Resolve-Path -LiteralPath $top).Path
Initialize-LocalPathRules

if (Test-ReservedName $Name) { throw "The package name '$Name' is a reserved Windows name or ends with a dot." }

$headFull = (Get-GitText -GitArgs @('rev-parse', 'HEAD')).Trim()
$headShort = (Get-GitText -GitArgs @('rev-parse', '--short', 'HEAD')).Trim()
$branch = (Get-GitText -GitArgs @('branch', '--show-current')).Trim()
if ($branch -eq '') { $branch = '(detached HEAD)' }
$headSubject = (Get-GitText -GitArgs @('log', '-1', '--format=%s', 'HEAD')).Trim()
$baseFull = (Get-GitText -GitArgs @('rev-parse', '--verify', "$BaseRef^{commit}")).Trim()
$baseShort = (Get-GitText -GitArgs @('rev-parse', '--short', $baseFull)).Trim()

if ($OutputRoot -eq '') { $OutputRoot = Join-Path $script:Root 'review-handoff' }
if (-not [System.IO.Path]::IsPathRooted($OutputRoot)) { $OutputRoot = Join-Path (Get-Location).Path $OutputRoot }
$OutputRoot = [System.IO.Path]::GetFullPath($OutputRoot)
$outputRel = ''
if (Test-UnderRoot ($OutputRoot + '\x') $script:Root) { $outputRel = $OutputRoot.Substring($script:Root.TrimEnd('\').Length + 1).Replace('\', '/') }

# Exclusion categories (see tools/ReviewHandoff-SECURITY.md). Every rule is anchored; a file is never hidden merely
# because a word such as "build" or "credentials" appears in its path.
$GeneratedDirPattern = '(^|/)(node_modules|\.git)(/|$)|^web/(dist|coverage)(/|$)|^web/tsconfig\.tsbuildinfo$|^bin/|^(\.vite|\.idea|\.vscode|\.claude)(/|$)|(^|/)\.incomplete-[^/]*(/|$)'
$ArtifactExtPattern = '(?i)\.(zip|patch|7z|gz|tar|tgz|rar)$'
$RootSourcePattern = '(?i)\.(md|ps1|go|mod|sum|ts|tsx|js|jsx|mjs|cjs|json|ya?ml|toml|css|html|sql|editorconfig|gitattributes|gitignore)$'
$CodeExt = '(?i)\.(go|ts|tsx|js|jsx|mjs|cjs|py|cs|java|rs|rb|php|c|h|cpp|hpp|ps1|sh|md|txt)$'
$DataExt = '(json|ya?ml|xml|ini|txt|csv|toml|conf|cfg|properties|env)'

function Get-NameReason([string]$Rel) {
    $leaf = ($Rel -split '/')[-1]
    if ($leaf -match '(?i)^\.env\.(example|sample|template|dist|defaults)$') { return '' }
    if ($leaf -match '(?i)^\.env(\..+)?$' -or $leaf -match '(?i)\.env$' -or $leaf -match '(?i)^\.envrc$') { return 'environment file' }
    if ($leaf -match '(?i)\.(pem|key|pfx|p12|jks|keystore|ppk|crt|cer|der|p7b|asc|gpg|kdbx)$') { return 'key, certificate or keystore file' }
    if ($leaf -match '(?i)^id_(rsa|dsa|ecdsa|ed25519)') { return 'SSH private key file' }
    if ($leaf -match "(?i)^credentials?(\.$DataExt)?$") { return 'credentials data file' }
    if ($leaf -match "(?i)^secrets?\.$DataExt$" -or $leaf -match "(?i)\.secrets?\.$DataExt$") { return 'secrets data file' }
    if ($leaf -match '(?i)^service[-_]?account.*\.json$') { return 'service account file' }
    if ($leaf -match '(?i)^(\.npmrc|\.pypirc|\.netrc|_netrc|\.pgpass|pgpass\.conf|\.git-credentials|\.dockercfg|kubeconfig)$' -or $leaf -match '(?i)\.kubeconfig$') { return 'tool credential file' }
    if ($leaf -match '(?i)\.(tfstate|tfstate\.backup|tfvars)$') { return 'infrastructure state or variable file' }
    if ($leaf -match '(?i)\.(sqlite3?|db|mdb|dump|bak)$') { return 'database or backup file' }
    return ''
}

# ===============================================================================================================
# Candidate files
# ===============================================================================================================

$leftOut = New-Object System.Collections.Generic.List[object]      # Path, Reason
$notes = New-Object System.Collections.Generic.List[string]
function Add-LeftOut([string]$Path, [string]$Reason) { $leftOut.Add([pscustomobject]@{ Path = $Path; Reason = $Reason }) | Out-Null }

$candidates = @{}     # rel -> @{ Status; Tracked; Explicit }
$deleted = New-Object System.Collections.Generic.List[string]
$explicitSet = New-Object System.Collections.Generic.List[string]

foreach ($f in $Files) {
    $norm = ($f.Replace('\', '/') -replace '^(\./)+', '')
    if ([System.IO.Path]::IsPathRooted($f) -or $norm.StartsWith('/') -or $norm -match '^[A-Za-z]:') {
        Add-LeftOut $f 'rejected: absolute paths are not allowed in -Files (use a repository-relative path)'; continue
    }
    $segments = $norm.Trim('/').Split('/')
    if ($segments -contains '..') { Add-LeftOut $f 'rejected: path traversal ("..") is not allowed'; continue }
    if ($norm -match '[\x00-\x1F"*?<>|]') { Add-LeftOut $f 'rejected: the path contains characters that are not valid in a file name'; continue }
    $explicitSet.Add($norm.Trim('/')) | Out-Null
}
$explicitMode = ($Files.Count -gt 0)

function Test-InExplicitSet([string]$Rel) {
    if (-not $explicitMode) { return $true }
    foreach ($e in $explicitSet) { if ($Rel -eq $e -or $Rel.StartsWith($e + '/')) { return $true } }
    return $false
}

function Add-Candidate([string]$Rel, [string]$Status, [bool]$Tracked) {
    if (-not $candidates.ContainsKey($Rel)) { $candidates[$Rel] = @{ Status = $Status; Tracked = $Tracked; Explicit = $explicitMode } }
}

# Tracked changes (staged and unstaged together), NUL separated so that every file name is read exactly.
$nameStatus = Split-Nul (Get-GitText -GitArgs @('diff', '-z', '--name-status', '--no-renames', '--no-ext-diff', '--no-textconv', $baseFull, '--'))
for ($i = 0; $i + 1 -lt $nameStatus.Count; $i += 2) {
    $st = $nameStatus[$i].Substring(0, 1)
    $rel = $nameStatus[$i + 1]
    if (-not (Test-InExplicitSet $rel)) { continue }
    if ($st -eq 'D') { $deleted.Add($rel) | Out-Null } else { Add-Candidate $rel $st $true }
}
# Untracked files (one entry per file).
$porcelain = Split-Nul (Get-GitText -GitArgs @('status', '--porcelain=v1', '-z', '-uall', '--no-renames'))
foreach ($entry in $porcelain) {
    if ($entry.Length -lt 4 -or $entry.Substring(0, 2) -ne '??') { continue }
    $rel = $entry.Substring(3).TrimEnd('/')
    if (-not (Test-InExplicitSet $rel)) { continue }
    Add-Candidate $rel '?' $false
}
# Explicit paths: unchanged tracked files are still honored; directories are expanded with ls-files.
foreach ($e in $explicitSet) {
    $full = [System.IO.Path]::GetFullPath((Join-Path $script:Root $e.Replace('/', '\')))
    if (-not (Test-UnderRoot $full $script:Root)) { Add-LeftOut $e 'rejected: the path resolves outside the repository'; continue }
    if (Test-Path -LiteralPath $full -PathType Leaf) {
        $tracked = ((Get-GitText -GitArgs @('ls-files', '-z', '--', $e)).Length -gt 0)
        Add-Candidate $e 'N' $tracked
    } elseif (Test-Path -LiteralPath $full -PathType Container) {
        foreach ($rel in (Split-Nul (Get-GitText -GitArgs @('ls-files', '-z', '--', $e)))) { Add-Candidate $rel 'N' $true }
    } else {
        Add-LeftOut $e 'not found on disk'
    }
}

# ===============================================================================================================
# Checks per candidate
# ===============================================================================================================

$included = New-Object System.Collections.Generic.List[object]   # Path, Status, Tracked, Bytes
$fileFindings = @{}                                               # for the report: path -> findings text
$totalBytes = [int64]0

function Get-PathReason([string]$Rel, [bool]$Tracked, [bool]$Explicit) {
    foreach ($comp in $Rel.Split('/')) { if (Test-ReservedName $comp) { return 'reserved Windows file name' } }
    if ($Rel -match '[\x00-\x1F"\\]') { return 'file name contains characters that cannot be represented safely in a patch' }
    if ($Rel -match $GeneratedDirPattern) { return 'generated or tooling directory (node_modules, .git, web/dist, web/coverage, bin, editor and handoff staging directories)' }
    if ($outputRel -ne '' -and -not $Tracked -and ($Rel -eq $outputRel -or $Rel.StartsWith($outputRel + '/')) -and $Rel -ne "$outputRel/README.md") { return 'untracked content of the handoff output directory (the directory README is source and is not excluded)' }
    $nr = Get-NameReason $Rel
    if ($nr -ne '') { return "sensitive file name ($nr)" }
    if (-not $IncludeArtifacts -and -not $Explicit -and -not $Tracked) {
        if ($Rel -match $ArtifactExtPattern) { return 'untracked archive or patch (review artifact); not included by default' }
        if ($Rel -match '^repository(/|$)') { return 'untracked repository/ review copy; not included by default' }
        if ($Rel -notmatch '/' -and $Rel -notmatch $RootSourcePattern) { return 'untracked loose file of an unknown kind at the repository root (review artifact); not included by default' }
    }
    return ''
}

function Read-Checked([string]$Full, [string]$Rel) {
    # Returns @{ Reason; Bytes; Text }. Reason is '' when the file may be used.
    $out = @{ Reason = ''; Bytes = $null; Text = '' }
    if (-not (Test-Path -LiteralPath $Full -PathType Leaf)) { $out.Reason = 'not found on disk'; return $out }
    if (-not (Test-UnderRoot $Full $script:Root)) { $out.Reason = 'resolves outside the repository'; return $out }
    if (Test-ReparseChain $Full $script:Root) { $out.Reason = 'symbolic link or junction (reparse point); never followed'; return $out }
    $len = (Get-Item -LiteralPath $Full -Force).Length
    if ($len -gt $MaxFileBytes) { $out.Reason = "larger than $MaxFileKB KB"; return $out }
    if ($Rel -match '(?i)\.(png|jpe?g|gif|webp|ico|pdf|zip|gz|tgz|7z|rar|exe|dll|so|dylib|bin|class|jar|woff2?|ttf|otf|eot|mp3|mp4|mov|wasm|pyc|o|a|lib|iso)$') {
        $out.Reason = 'unsupported binary format'; return $out
    }
    try { $bytes = [System.IO.File]::ReadAllBytes($Full) } catch { $out.Reason = 'could not be read'; return $out }
    $text = ''; $why = ''
    if (-not [RelayForge.HandoffV2.Scanner]::TryDecode($bytes, [ref]$text, [ref]$why)) { $out.Reason = $why; return $out }
    $found = [RelayForge.HandoffV2.Scanner]::ScanText($text)
    if ($found.Count -gt 0) {
        $f = $found[0]
        $more = ''
        if ($found.Count -gt 1) { $more = " (+$($found.Count - 1) more)" }
        $out.Reason = "secret-like content ($($f.Rule), line $($f.Line)$more)"
        return $out
    }
    $out.Bytes = $bytes; $out.Text = $text
    return $out
}

foreach ($rel in ($candidates.Keys | Sort-Object)) {
    $c = $candidates[$rel]
    $reason = Get-PathReason $rel $c.Tracked $c.Explicit
    $data = $null
    if ($reason -eq '') {
        $full = [System.IO.Path]::GetFullPath((Join-Path $script:Root $rel.Replace('/', '\')))
        $data = Read-Checked $full $rel
        $reason = $data.Reason
    }
    if ($reason -eq '' -and $included.Count -ge $MaxFiles) { $reason = "package limit of $MaxFiles files reached" }
    if ($reason -eq '' -and ($totalBytes + $data.Bytes.Length) -gt $MaxTotalBytes) { $reason = "package size limit of $MaxTotalMB MB reached" }
    if ($reason -ne '') { Add-LeftOut $rel $reason; continue }
    $totalBytes += $data.Bytes.Length
    $included.Add([pscustomobject]@{ Path = $rel; Status = $c.Status; Tracked = $c.Tracked; Bytes = $data.Bytes }) | Out-Null
}

# Deleted files: the name rules apply, and the diff itself is scanned below.
$deletedKept = New-Object System.Collections.Generic.List[string]
foreach ($d in $deleted) {
    $reason = Get-PathReason $d $true $explicitMode
    if ($reason -ne '') { Add-LeftOut $d "deleted file: diff omitted ($reason)"; continue }
    $deletedKept.Add($d) | Out-Null
}

# ===============================================================================================================
# Staging directory (published by rename at the very end)
# ===============================================================================================================

New-Item -ItemType Directory -Force -Path $OutputRoot | Out-Null
$pkgName = $Name
$n = 2
while ((Test-Path -LiteralPath (Join-Path $OutputRoot $pkgName)) -or (Test-Path -LiteralPath (Join-Path $OutputRoot "$pkgName.zip"))) {
    $pkgName = "$Name-$n"
    $n++
}
$stamp = [guid]::NewGuid().ToString('N').Substring(0, 8)
$stage = Join-Path $OutputRoot ".incomplete-$pkgName-$stamp"
$zipTemp = Join-Path $OutputRoot ".incomplete-$pkgName-$stamp.zip"
New-Item -ItemType Directory -Path $stage | Out-Null
New-Item -ItemType Directory -Path (Join-Path $stage 'files') | Out-Null

$exitCode = 0
$published = $false
try {

# ---------------------------------------------------------------------------------------------------------------
# files/ and changes.patch
# ---------------------------------------------------------------------------------------------------------------

foreach ($f in $included) {
    $dest = Join-Path (Join-Path $stage 'files') $f.Path.Replace('/', '\')
    New-Item -ItemType Directory -Force -Path (Split-Path -Parent $dest) | Out-Null
    [System.IO.File]::WriteAllBytes($dest, $f.Bytes)
}

$patchStream = New-Object System.IO.MemoryStream
$patchOmitted = New-Object System.Collections.Generic.List[string]
$trackedPaths = New-Object System.Collections.Generic.List[string]
foreach ($f in $included) { if ($f.Tracked) { $trackedPaths.Add($f.Path) | Out-Null } }
foreach ($d in $deletedKept) { $trackedPaths.Add($d) | Out-Null }
if ($trackedPaths.Count -gt 0) {
    # One diff of the whole tree (two processes in total); the sections of files that were not selected, or that
    # were left out by a check, are dropped here and never written.
    $wanted = New-Object 'System.Collections.Generic.HashSet[string]' ([System.StringComparer]::Ordinal)
    foreach ($p in $trackedPaths) { [void]$wanted.Add($p) }
    $order = Split-Nul $Utf8NoBom.GetString((Invoke-GitProcess -GitArgs @('diff', '--name-only', '-z', '--no-renames', '--no-ext-diff', '--no-textconv', $baseFull, '--')).Bytes)
    $raw = (Invoke-GitProcess -GitArgs @('diff', '--binary', '--no-renames', '--no-ext-diff', '--no-textconv', $baseFull, '--')).Bytes
    $sections = [RelayForge.HandoffV2.Scanner]::SplitPatch($raw)
    if ($sections.Count -ne $order.Count) {
        $patchOmitted.Add('all tracked changes: the patch could not be split safely') | Out-Null
        $notes.Add('The diff of the tracked files was left out because its sections could not be matched to file names.')
    } else {
        for ($i = 0; $i -lt $sections.Count; $i++) {
            if (-not $wanted.Contains($order[$i])) { continue }
            $text = ''; $why = ''
            $section = $sections[$i]
            if (-not [RelayForge.HandoffV2.Scanner]::TryDecode($section, [ref]$text, [ref]$why)) {
                $patchOmitted.Add("$($order[$i]): diff omitted ($why)") | Out-Null; continue
            }
            $found = [RelayForge.HandoffV2.Scanner]::ScanText($text)
            if ($found.Count -gt 0) {
                $patchOmitted.Add("$($order[$i]): diff omitted (secret-like content in added, removed or context lines: $($found[0].Rule), line $($found[0].Line) of the diff)") | Out-Null
                continue
            }
            $patchStream.Write($section, 0, $section.Length)
        }
    }
}
foreach ($f in @($included | Where-Object { -not $_.Tracked } | Sort-Object Path)) {
    $section = [RelayForge.HandoffV2.Scanner]::NewFilePatch($f.Path, $f.Bytes)
    $patchStream.Write($section, 0, $section.Length)
}
foreach ($o in $patchOmitted) { Add-LeftOut ($o.Split(':')[0]) ($o.Substring($o.IndexOf(':') + 2)) }
[System.IO.File]::WriteAllBytes((Join-Path $stage 'changes.patch'), $patchStream.ToArray())

# ---------------------------------------------------------------------------------------------------------------
# git-status.txt
# ---------------------------------------------------------------------------------------------------------------

$sb = New-Object System.Text.StringBuilder
[void]$sb.AppendLine("Repository   : <repo>")
[void]$sb.AppendLine("Branch       : $branch")
[void]$sb.AppendLine("HEAD         : $headFull")
[void]$sb.AppendLine("HEAD subject : $headSubject")
[void]$sb.AppendLine("Diff base    : $BaseRef ($baseFull)")
[void]$sb.AppendLine("Prepared     : $([DateTime]::UtcNow.ToString('yyyy-MM-dd HH:mm:ss')) UTC")
[void]$sb.AppendLine('')
[void]$sb.AppendLine('--- git status --short --branch (untracked directories are summarized) ---')
[void]$sb.Append((Get-GitText -GitArgs @('status', '--short', '--branch', '--untracked-files=normal')))
[void]$sb.AppendLine('')
[void]$sb.AppendLine('--- git log --oneline -8 ---')
[void]$sb.Append((Get-GitText -GitArgs @('log', '--oneline', '-8')))
Write-Utf8 (Join-Path $stage 'git-status.txt') (Hide-LocalPaths $sb.ToString())

# ---------------------------------------------------------------------------------------------------------------
# tests.log: -RunChecks and -TestLogs. Output is redacted line by line; exit codes and failures stay visible.
# ---------------------------------------------------------------------------------------------------------------

$checkResults = New-Object System.Collections.Generic.List[object]
$testLog = New-Object System.Text.StringBuilder
$redactedLines = 0
$checksFailed = $false

function Protect-Output([string]$Text) {
    $Text = $Text -replace '\x1B\[[0-9;]*[A-Za-z]', ''
    $found = [RelayForge.HandoffV2.Scanner]::ScanText($Text)
    if ($found.Count -gt 0) {
        $lines = 0
        foreach ($f in $found) { $lines += ($f.EndLine - $f.Line + 1) }
        $script:redactedLines += $lines
        $Text = [RelayForge.HandoffV2.Scanner]::Redact($Text, $found)
    }
    return (Hide-LocalPaths $Text)
}

if ($RunChecks) {
    $web = Join-Path $script:Root 'web'
    if (-not (Test-Path -LiteralPath (Join-Path $web 'node_modules'))) {
        [void]$testLog.AppendLine('RunChecks was requested but web/node_modules does not exist. Nothing was run, and nothing was installed.')
        $checkResults.Add([pscustomobject]@{ Name = 'all checks'; Exit = 'not run'; Failed = $true; Note = 'web/node_modules missing' }) | Out-Null
        $checksFailed = $true
    } else {
        foreach ($npmScript in @('format:check', 'lint', 'typecheck', 'test', 'build')) {
            [void]$testLog.AppendLine("===== npm run $npmScript =====")
            $tmp = [System.IO.Path]::GetTempFileName()
            $proc = Start-Process -FilePath 'cmd.exe' -ArgumentList '/d', '/c', "npm run $npmScript --silent > `"$tmp`" 2>&1" -WorkingDirectory $web -WindowStyle Hidden -PassThru
            $null = $proc.Handle
            $note = ''
            if (-not $proc.WaitForExit($CheckTimeoutSeconds * 1000)) {
                & taskkill.exe /PID $proc.Id /T /F 2>&1 | Out-Null
                $null = $proc.WaitForExit(10000)
                $exitText = 'timeout'; $note = "killed after $CheckTimeoutSeconds s"; $failed = $true
            } else {
                $exitText = "$($proc.ExitCode)"; $failed = ($proc.ExitCode -ne 0)
            }
            $out = ''
            try { $out = [System.IO.File]::ReadAllText($tmp) } catch { }
            Remove-Item -LiteralPath $tmp -Force -ErrorAction SilentlyContinue
            [void]$testLog.AppendLine((Protect-Output $out).TrimEnd())
            [void]$testLog.AppendLine("----- exit code: $exitText$(if ($note) { " ($note)" })")
            [void]$testLog.AppendLine('')
            if ($failed) { $checksFailed = $true }
            $checkResults.Add([pscustomobject]@{ Name = "npm run $npmScript"; Exit = $exitText; Failed = $failed; Note = $note }) | Out-Null
        }
    }
}

function Resolve-AllowedFile([string]$Path) {
    # Evidence and test logs may only be read from the repository or from a root named with -AllowedRoot.
    $full = $Path
    if (-not [System.IO.Path]::IsPathRooted($full)) { $full = Join-Path $script:Root $full }
    $full = [System.IO.Path]::GetFullPath($full)
    $roots = @($script:Root) + @($AllowedRoot | ForEach-Object { [System.IO.Path]::GetFullPath($_) })
    foreach ($r in $roots) {
        if (Test-UnderRoot $full $r) {
            # Every root except the repository itself must be a plain directory path (see Test-AllowedRootUnsafe).
            if ($r -ne $script:Root -and (Test-AllowedRootUnsafe $r)) { return @{ Full = $null; Reason = 'the allowed root is, or lies behind, a symbolic link or junction (reparse point); never followed' } }
            if (Test-ReparseChain $full $r) { return @{ Full = $null; Reason = 'symbolic link or junction (reparse point); never followed' } }
            if (-not (Test-Path -LiteralPath $full -PathType Leaf)) { return @{ Full = $null; Reason = 'not found' } }
            return @{ Full = $full; Reason = '' }
        }
    }
    return @{ Full = $null; Reason = 'outside the repository and every -AllowedRoot; not read' }
}

foreach ($log in $TestLogs) {
    $res = Resolve-AllowedFile $log
    if ($res.Reason -ne '') {
        [void]$testLog.AppendLine("===== $(Hide-LocalPaths $log) : not included ($($res.Reason)) =====")
        Add-LeftOut (Hide-LocalPaths $log) "test log: $($res.Reason)"; continue
    }
    if ((Get-Item -LiteralPath $res.Full).Length -gt $MaxFileBytes) {
        Add-LeftOut (Hide-LocalPaths $log) "test log: larger than $MaxFileKB KB"; continue
    }
    $bytes = [System.IO.File]::ReadAllBytes($res.Full)
    $text = ''; $why = ''
    if (-not [RelayForge.HandoffV2.Scanner]::TryDecode($bytes, [ref]$text, [ref]$why)) {
        Add-LeftOut (Hide-LocalPaths $log) "test log: $why"; continue
    }
    [void]$testLog.AppendLine("===== supplied log: $(Split-Path -Leaf $res.Full) =====")
    [void]$testLog.AppendLine((Protect-Output $text).TrimEnd())
    [void]$testLog.AppendLine('')
    $checkResults.Add([pscustomobject]@{ Name = "supplied log $(Split-Path -Leaf $res.Full)"; Exit = 'n/a'; Failed = $false; Note = 'copied as given, after redaction' }) | Out-Null
}
if ($testLog.Length -eq 0) {
    [void]$testLog.AppendLine('No test or verification output was supplied or run for this package.')
    [void]$testLog.AppendLine('Use -RunChecks to run the frontend checks, or -TestLogs to include existing output.')
}
Write-Utf8 (Join-Path $stage 'tests.log') $testLog.ToString()

# ---------------------------------------------------------------------------------------------------------------
# evidence/
# ---------------------------------------------------------------------------------------------------------------

$evidenceIncluded = New-Object System.Collections.Generic.List[string]
$unscanned = New-Object System.Collections.Generic.List[string]
foreach ($item in $Evidence) {
    $shown = Hide-LocalPaths $item
    $res = Resolve-AllowedFile $item
    if ($res.Reason -ne '') { Add-LeftOut $shown "evidence: $($res.Reason)"; continue }
    $leaf = Split-Path -Leaf $res.Full
    if (Test-ReservedName $leaf) { Add-LeftOut $shown 'evidence: reserved Windows file name'; continue }
    $nr = Get-NameReason $leaf
    if ($nr -ne '') { Add-LeftOut $shown "evidence: sensitive file name ($nr)"; continue }
    if ((Get-Item -LiteralPath $res.Full).Length -gt $MaxFileBytes) { Add-LeftOut $shown "evidence: larger than $MaxFileKB KB"; continue }
    if (($totalBytes + (Get-Item -LiteralPath $res.Full).Length) -gt $MaxTotalBytes) { Add-LeftOut $shown "evidence: package size limit of $MaxTotalMB MB reached"; continue }
    $bytes = [System.IO.File]::ReadAllBytes($res.Full)
    $target = Join-Path (Join-Path $stage 'evidence') $leaf
    $k = 2
    while (Test-Path -LiteralPath $target) { $target = Join-Path (Join-Path $stage 'evidence') ("$k-" + $leaf); $k++ }
    $isImage = ($leaf -match '(?i)\.(png|jpe?g|gif|webp)$')
    if ($isImage) {
        if (-not $AllowImages) { Add-LeftOut $shown 'evidence: images cannot be scanned for secrets; pass -AllowImages to include screenshots'; continue }
        New-Item -ItemType Directory -Force -Path (Join-Path $stage 'evidence') | Out-Null
        [System.IO.File]::WriteAllBytes($target, $bytes)
        $unscanned.Add("evidence/$(Split-Path -Leaf $target)") | Out-Null
    } else {
        $text = ''; $why = ''
        if (-not [RelayForge.HandoffV2.Scanner]::TryDecode($bytes, [ref]$text, [ref]$why)) { Add-LeftOut $shown "evidence: $why"; continue }
        $found = [RelayForge.HandoffV2.Scanner]::ScanText($text)
        if ($found.Count -gt 0) { Add-LeftOut $shown "evidence: secret-like content ($($found[0].Rule), line $($found[0].Line))"; continue }
        New-Item -ItemType Directory -Force -Path (Join-Path $stage 'evidence') | Out-Null
        Write-Utf8 $target (Hide-LocalPaths $text)
    }
    $totalBytes += $bytes.Length
    $evidenceIncluded.Add($shown) | Out-Null
}

# ---------------------------------------------------------------------------------------------------------------
# REVIEW.md
# ---------------------------------------------------------------------------------------------------------------

$numstat = @{}
$ns = Split-Nul (Get-GitText -GitArgs @('diff', '-z', '--numstat', '--no-renames', '--no-ext-diff', '--no-textconv', $baseFull, '--'))
foreach ($entry in $ns) {
    $p = $entry.Split("`t", 3)
    if ($p.Count -eq 3) { $numstat[$p[2]] = "+$($p[0]) / -$($p[1])" }
}

function Add-Section($sb, [string]$Title, [string[]]$Lines, [string]$Empty) {
    [void]$sb.AppendLine("## $Title")
    [void]$sb.AppendLine('')
    if ($Lines.Count -eq 0) { [void]$sb.AppendLine($Empty) } else { foreach ($l in $Lines) { [void]$sb.AppendLine((Hide-LocalPaths $l)) } }
    [void]$sb.AppendLine('')
}

function Get-LeftOutLines {
    $lines = New-Object System.Collections.Generic.List[string]
    $groups = @{}
    foreach ($x in $leftOut) {
        $top = ''
        if ($x.Path -match '^([^/]+)/') { $top = $Matches[1] + '/' }
        $key = "$top|$($x.Reason)"
        if (-not $groups.ContainsKey($key)) { $groups[$key] = New-Object System.Collections.Generic.List[object] }
        $groups[$key].Add($x) | Out-Null
    }
    foreach ($key in ($groups.Keys | Sort-Object)) {
        $items = $groups[$key]
        $top = $key.Split('|')[0]
        if ($top -ne '' -and $items.Count -gt 5) { $lines.Add("$top ($($items.Count) files): $($items[0].Reason)") | Out-Null }
        else { foreach ($i in $items) { $lines.Add("$($i.Path): $($i.Reason)") | Out-Null } }
    }
    return $lines.ToArray()
}

$md = New-Object System.Text.StringBuilder
[void]$md.AppendLine("# Review package: $pkgName")
[void]$md.AppendLine('')
if ($checksFailed) {
    $bad = @($checkResults | Where-Object { $_.Failed } | ForEach-Object { "$($_.Name) (exit $($_.Exit))" })
    [void]$md.AppendLine("> **VERIFICATION FAILED:** $($bad -join '; '). The package was still created; see tests.log.")
    [void]$md.AppendLine('')
} elseif ($checkResults.Count -gt 0 -and $RunChecks) {
    [void]$md.AppendLine('> Verification: every check run for this package exited with code 0.')
    [void]$md.AppendLine('')
}
[void]$md.AppendLine("- Prepared: $([DateTime]::UtcNow.ToString('yyyy-MM-dd HH:mm:ss')) UTC")
[void]$md.AppendLine("- Branch: ``$branch``")
[void]$md.AppendLine("- HEAD: ``$headShort`` $headSubject")
[void]$md.AppendLine("- Diff base: ``$baseShort`` ($BaseRef). The patch covers that revision up to the working tree.")
[void]$md.AppendLine('- Nothing in this package was committed, pushed or staged by the tool that prepared it.')
[void]$md.AppendLine('')
[void]$md.AppendLine('## Security scan')
[void]$md.AppendLine('')
[void]$md.AppendLine("- Scanner version $ScannerVersion (bounded heuristics). Files included: $($included.Count). Left out: $($leftOut.Count). Lines withheld from logs: $redactedLines.")
[void]$md.AppendLine('- Every file, the complete patch, the logs and this report were scanned. The scan is a heuristic: it does not find every secret. A person must review this package before it is shared with an external service.')
if ($unscanned.Count -gt 0) { [void]$md.AppendLine("- Not scanned (images): $($unscanned -join ', ')") }
foreach ($note in $notes) { [void]$md.AppendLine("- $note") }
[void]$md.AppendLine('')

Add-Section $md 'Summary' $Summary '_No summary was supplied. Pass -Summary when preparing the package._'
Add-Section $md 'Scope' $Scope '_No scope statement was supplied. Pass -Scope._'
if ($RequestedMaterial.Count -gt 0) { Add-Section $md 'Material requested for this review' $RequestedMaterial '' }

[void]$md.AppendLine('## Changed files')
[void]$md.AppendLine('')
if ($included.Count -eq 0 -and $deletedKept.Count -eq 0) {
    [void]$md.AppendLine('_No changed files were included._')
} else {
    [void]$md.AppendLine('| Status | File | Lines |')
    [void]$md.AppendLine('|---|---|---|')
    foreach ($f in $included) {
        $label = switch ($f.Status) { '?' { 'new (untracked)' } 'A' { 'added' } 'M' { 'modified' } 'N' { 'unchanged (requested)' } default { $f.Status } }
        $lines = ''
        if ($numstat.ContainsKey($f.Path)) { $lines = $numstat[$f.Path] } elseif ($f.Status -eq '?') { $lines = 'new file' }
        [void]$md.AppendLine("| $label | ``$($f.Path)`` | $lines |")
    }
    foreach ($d in $deletedKept) { [void]$md.AppendLine("| deleted | ``$d`` | in changes.patch only |") }
}
[void]$md.AppendLine('')

Add-Section $md 'Implementation decisions' $Decisions '_None supplied. Pass -Decisions._'

[void]$md.AppendLine('## Test and verification results')
[void]$md.AppendLine('')
if ($checkResults.Count -eq 0) {
    [void]$md.AppendLine('No test or verification output is part of this package. Do not read the absence as a passing result.')
} else {
    [void]$md.AppendLine('The output is in `tests.log` (secret-like lines withheld, local paths replaced).')
    [void]$md.AppendLine('')
    [void]$md.AppendLine('| Command | Exit code | Result | Note |')
    [void]$md.AppendLine('|---|---|---|---|')
    foreach ($r in $checkResults) {
        $verdict = 'ok'
        if ($r.Failed) { $verdict = 'FAILED' } elseif ($r.Exit -eq 'n/a') { $verdict = 'supplied' }
        [void]$md.AppendLine("| $($r.Name) | $($r.Exit) | $verdict | $($r.Note) |")
    }
}
[void]$md.AppendLine('')

Add-Section $md 'Known limitations' $Limitations '_None supplied. Pass -Limitations._'
if ($evidenceIncluded.Count -gt 0) {
    $evLines = @($evidenceIncluded | ForEach-Object { "- ``evidence/$(Split-Path -Leaf $_)`` (from ``$_``)" })
    Add-Section $md 'Additional evidence' $evLines ''
}

[void]$md.AppendLine('## Left out of this package')
[void]$md.AppendLine('')
$leftLines = @(Get-LeftOutLines)
if ($leftLines.Count -eq 0 -and $NotIncluded.Count -eq 0) {
    [void]$md.AppendLine('Nothing that was selected was left out.')
} else {
    foreach ($x in $leftLines) { [void]$md.AppendLine("- $(Hide-LocalPaths $x)") }
    foreach ($x in $NotIncluded) { [void]$md.AppendLine("- Requested but not included: $(Hide-LocalPaths $x)") }
}
[void]$md.AppendLine('')
[void]$md.AppendLine('Reasons name a rule and a line number, never the matched text. Categories: see `tools/ReviewHandoff-SECURITY.md`.')
[void]$md.AppendLine('')
[void]$md.AppendLine('## Contents')
[void]$md.AppendLine('')
[void]$md.AppendLine('- `REVIEW.md`: this file')
[void]$md.AppendLine('- `changes.patch`: complete diff of the included files (apply with `git apply`; new files are additions)')
[void]$md.AppendLine('- `files/`: the included files under their repository paths')
[void]$md.AppendLine('- `tests.log`: verification output')
[void]$md.AppendLine('- `git-status.txt`: HEAD, branch, status and recent history')
[void]$md.AppendLine('- `MANIFEST.txt`: SHA-256 of every other file in the package')
Write-Utf8 (Join-Path $stage 'REVIEW.md') $md.ToString()

# ---------------------------------------------------------------------------------------------------------------
# Final scan of everything that will be published, then MANIFEST.txt
# ---------------------------------------------------------------------------------------------------------------

$final = New-Object System.Collections.Generic.List[string]
foreach ($file in @(Get-ChildItem -LiteralPath $stage -Recurse -File)) {
    $rel = $file.FullName.Substring($stage.Length + 1).Replace('\', '/')
    if ($rel -match '(?i)\.(png|jpe?g|gif|webp)$') { continue }
    $bytes = [System.IO.File]::ReadAllBytes($file.FullName)
    $text = ''; $why = ''
    if (-not [RelayForge.HandoffV2.Scanner]::TryDecode($bytes, [ref]$text, [ref]$why)) {
        $final.Add("${rel}: $why") | Out-Null; continue
    }
    $found = [RelayForge.HandoffV2.Scanner]::ScanText($text)
    if ($found.Count -gt 0) { $final.Add("${rel}: secret-like content ($($found[0].Rule), line $($found[0].Line))") | Out-Null }
}
if ($final.Count -gt 0) {
    Write-Host ''
    Write-Host 'PACKAGE WITHHELD: the final scan found content that must not be shared (values are not printed):'
    foreach ($x in $final) { Write-Host "  $x" }
    $exitCode = 3
    throw [System.OperationCanceledException]::new('final scan failed')
}

$manifest = New-Object System.Text.StringBuilder
foreach ($file in @(Get-ChildItem -LiteralPath $stage -Recurse -File | Sort-Object FullName)) {
    $rel = $file.FullName.Substring($stage.Length + 1).Replace('\', '/')
    [void]$manifest.AppendLine("$([RelayForge.HandoffV2.Scanner]::Sha256Hex($file.FullName))  $rel  ($($file.Length) bytes)")
}
Write-Utf8 (Join-Path $stage 'MANIFEST.txt') $manifest.ToString()

# ---------------------------------------------------------------------------------------------------------------
# ZIP (UTF-8 entry names, forward slashes) under a temporary name, then publish by renames.
# ---------------------------------------------------------------------------------------------------------------

Add-Type -AssemblyName System.IO.Compression
$zipStream = [System.IO.File]::Open($zipTemp, [System.IO.FileMode]::CreateNew)
try {
    $zip = New-Object System.IO.Compression.ZipArchive($zipStream, [System.IO.Compression.ZipArchiveMode]::Create, $false, $Utf8NoBom)
    try {
        foreach ($file in @(Get-ChildItem -LiteralPath $stage -Recurse -File | Sort-Object FullName)) {
            $rel = $file.FullName.Substring($stage.Length + 1).Replace('\', '/')
            $entry = $zip.CreateEntry("$pkgName/$rel", [System.IO.Compression.CompressionLevel]::Optimal)
            $in = [System.IO.File]::OpenRead($file.FullName)
            $out = $entry.Open()
            try { $in.CopyTo($out) } finally { $out.Dispose(); $in.Dispose() }
        }
    } finally { $zip.Dispose() }
} finally { $zipStream.Dispose() }
[RelayForge.HandoffV2.Scanner]::MarkUtf8Names($zipTemp)

$pkg = Join-Path $OutputRoot $pkgName
$zipPath = Join-Path $OutputRoot "$pkgName.zip"
# Neither Move call replaces an existing item: a collision makes this run fail instead of overwriting.
[System.IO.File]::Move($zipTemp, $zipPath)
try { [System.IO.Directory]::Move($stage, $pkg) }
catch {
    # Keep the ZIP consistent with the directory: the ZIP is ours, so withdraw it if the directory cannot be published.
    Remove-Item -LiteralPath $zipPath -Force -ErrorAction SilentlyContinue
    throw
}
$published = $true

} catch [System.OperationCanceledException] {
    # Final scan failure: nothing is published. $exitCode is already 3.
} finally {
    if (-not $published) {
        # Only the staging artifacts of THIS run are removed; they are hidden, uniquely named and never complete.
        if (Test-Path -LiteralPath $stage) { Remove-Item -LiteralPath $stage -Recurse -Force -ErrorAction SilentlyContinue }
        if (Test-Path -LiteralPath $zipTemp) { Remove-Item -LiteralPath $zipTemp -Force -ErrorAction SilentlyContinue }
    }
}

if (-not $published) {
    if ($exitCode -eq 0) { $exitCode = 1 }
    exit $exitCode
}

# ===============================================================================================================
# Result
# ===============================================================================================================

if ($checksFailed) { $exitCode = 2 }
Write-Host ''
Write-Host "Review package : $pkg"
Write-Host "ZIP archive    : $zipPath ($([Math]::Round((Get-Item -LiteralPath $zipPath).Length / 1KB, 1)) KB)"
Write-Host "Included files : $($included.Count)   Left out: $($leftOut.Count)   Lines withheld from logs: $redactedLines"
foreach ($x in @(Get-LeftOutLines)) { Write-Host "  left out: $x" }
if ($checksFailed) { Write-Warning 'VERIFICATION FAILED: at least one check failed or timed out. See tests.log. Exit code 2.' }
[pscustomobject]@{
    PackagePath  = $pkg
    ZipPath      = $zipPath
    Included     = @($included | ForEach-Object { $_.Path })
    LeftOut      = @(Get-LeftOutLines)
    ChecksFailed = $checksFailed
    ExitCode     = $exitCode
}
exit $exitCode
