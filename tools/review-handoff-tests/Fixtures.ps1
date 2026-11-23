# Synthetic fixtures for the review-handoff regression tests. Dot-source this file.
#
# Every credential-looking value is assembled from fragments at run time, so that this file contains no secret-like
# literal and passes the handoff scanner itself. All values are fake. Nothing here touches the real repository.

# Fragments are joined with -join, not with "+": the scanner deliberately re-joins adjacent literals that are added with "+",
# so a "+" here would make this file look like it contains the secret.
$script:KW = 'pass', 'word' -join ''               # the word "password"
$script:VAL = 'abcdEFGH', '12345678' -join ''      # 16 characters, letters and digits
$script:AWS = 'AKIA', 'ABCDEFGHIJKLMNOP' -join ''
$script:GH = 'ghp', '_', ('a1B2c3D4e5' * 4) -join ''
$script:SLACK = 'xox', 'b-', '123456789012-abcdefghijkl' -join ''
$script:PEM = '-----BEGIN ', 'PRIVATE', ' KEY-----' -join ''
$script:PEMEND = '-----END ', 'PRIVATE', ' KEY-----' -join ''
$script:CERT = '-----BEGIN ', 'CERTIF', 'ICATE-----' -join ''
$script:CERTEND = '-----END ', 'CERTIF', 'ICATE-----' -join ''
$script:JWT = 'eyJhbGciOiJIUzI1NiJ9', '.eyJzdWIiOiIxMjM0NTY3ODkwIn0', '.abcdEFGH1234abcdEFGH' -join ''
$script:NPMTOKEN = 'npm', '_', ('aB3dE5gH7j' * 4) -join ''

function New-TestRepo([string]$Path) {
    New-Item -ItemType Directory -Force $Path | Out-Null
    & git -C $Path init -q 2>&1 | Out-Null
    & git -C $Path config user.email tester@example.com
    & git -C $Path config user.name tester
    & git -C $Path config core.autocrlf false
    & git -C $Path config core.safecrlf false
    return $Path
}

function Put-File([string]$Root, [string]$Rel, [string]$Text) {
    $p = Join-Path $Root $Rel.Replace('/', '\')
    New-Item -ItemType Directory -Force (Split-Path -Parent $p) | Out-Null
    [System.IO.File]::WriteAllText($p, $Text, (New-Object System.Text.UTF8Encoding($false)))
}

function Save-Commit([string]$Root, [string]$Message) {
    & git -C $Root add -A 2>&1 | Out-Null
    & git -C $Root commit -q -m $Message 2>&1 | Out-Null
}

function ConvertTo-Hex([string]$Text) { return (($Text.ToCharArray() | ForEach-Object { '{0:x2}' -f [int]$_ }) -join '') }

# Files that MUST be left out of a package. Each one is a credential format that the first version of the scanner missed
# (marked "v1 gap") or caught (marked "v1 ok"). Value = file content.
function Get-BlockedCases {
    $q = '"'
    $c = [ordered]@{}
    # sensitive file names
    $c['.env'] = "APP_SECRET=$($script:VAL)"
    $c['web/.env.local'] = "X=1"
    $c['prod.env'] = "DB_PASS=$($script:VAL)"                                  # v1 gap: only dot-prefixed names were blocked
    $c['.env.production'] = "A=1"
    $c['keys/id_rsa'] = "x"
    $c['keys/server.pem'] = "x"
    $c['keys/server.crt'] = "x"
    $c['aws/credentials'] = "x"
    $c['credentials.json'] = "{}"
    $c['config/secrets.json'] = "{}"
    $c['.npmrc'] = "registry=https://registry.npmjs.org/"                     # v1 gap
    $c['service-account.json'] = "{}"
    $c['terraform.tfvars'] = "x = 1"
    $c['kube/kubeconfig'] = "x"
    $c['data/app.db'] = "x"
    # content formats
    $c['src/quoted.ts'] = "const $($script:KW) = '$($script:VAL)'"            # v1 ok
    $c['src/multiline.ts'] = "const $($script:KW) =`n  '$($script:VAL)'"      # v1 ok
    $c['src/json-password.json'] = "{ $q$($script:KW)$q`: $q$($script:VAL)$q }"          # v1 gap
    $c['src/json-secret.json'] = "{ ${q}client_secret$q`: $q$($script:VAL)$q }"          # v1 gap
    $c['src/json-apikey.json'] = "{ ${q}api_key$q`: $q$($script:VAL)$q }"                # v1 gap
    $c['src/json-token.json'] = "{ ${q}accessToken$q`: $q$($script:VAL)$q }"             # v1 gap
    $c['src/settings.yaml'] = "$($script:KW): $($script:VAL)"                  # v1 gap: unquoted YAML
    $c['src/app.properties'] = "db.$($script:KW)=$($script:VAL)"               # v1 gap
    $c['src/Conn.cs'] = "var c = ${q}Server=x;User Id=sa;$($script:KW)=$($script:VAL);$q;"   # v1 gap
    $c['src/conn-short.txt'] = ("Data Source=a;Pw", "d=hunter22;" -join '')                  # v1 gap
    $c['src/db-url.ts'] = ("const u = 'postgres://app:", 'hunter', "2xy@db.local/x'" -join '')   # v1 ok
    $c['src/mongo.txt'] = ('mongodb+srv://admin:', 'Zx9fQw', 'ErTy@cluster0.example.net/db' -join '')
    $c['src/http.txt'] = "Authorization: Bearer $($script:JWT)"               # v1 gap
    $c['src/bearer.txt'] = "curl -H ${q}Authorization: Bearer $($script:VAL)$($script:VAL)$q"  # v1 gap
    $c['src/npm-token.txt'] = "//registry.npmjs.org/:_authToken=$($script:NPMTOKEN)"          # v1 gap
    $c['src/concat.ts'] = ("const $($script:KW) = 'abcdEFGH", "' + '12345678'" -join '')      # v1 gap (the scanner re-joins the added literals)
    $c['src/b64.txt'] = [Convert]::ToBase64String([Text.Encoding]::UTF8.GetBytes("$($script:KW)=$($script:VAL)"))   # v1 gap
    $c['src/hex.txt'] = ConvertTo-Hex "const $($script:KW) = '$($script:VAL)'"                 # v1 gap
    $c['src/escaped.json'] = "{ \${q}$($script:KW)\$q`: \$q$($script:VAL)\$q }"                # v1 gap
    $c['src/aws.txt'] = "id = $($script:AWS)"                                  # v1 ok
    $c['src/github.txt'] = "t = $($script:GH)"
    $c['src/slack.txt'] = "t = $($script:SLACK)"
    $c['src/privkey.txt'] = "$($script:PEM)`nMIIEvQIBADANBgkqhkiG9w0BAQEFAASC`n$($script:PEMEND)"        # v1 ok
    $c['src/cert.txt'] = "$($script:CERT)`nMIIDdzCCAl+gAwIBAgIEAgAAuTANBgkqhkiG9w0BAQUFADBaMQswCQYDVQQGEwJJ`n$($script:CERTEND)"
    $c['src/pass-key.txt'] = "DB_PASS=$($script:VAL)"
    $c['src/blob.txt'] = (1..6 | ForEach-Object { 'Zm9vYmFyYmF6' + 'QUJDREVGR0hJSktMTU5PUFFSU1RVVldYWVo' + [string]$_ + 'q8Xk2LmN4pR7sT9vW1yB3dF5hJ' }) -join ''
    return $c
}

# Content that looks like a credential but is not one. These must NOT be left out (no hiding of relevant source).
function Get-AllowedCases {
    $q = '"'
    $c = [ordered]@{}
    $c['deploy/.env.example'] = "TOKEN="
    $c['src/plain.ts'] = "export const a = 1"
    $c['internal/auth/credentials.go'] = "package auth // credential types"      # v1 gap: name rule hid source
    $c['internal/build/builder.go'] = "package build"                           # v1 gap: path word "build"
    $c['cmd/coverage/main.go'] = "package main"                                 # v1 gap: path word "coverage"
    $c['web/src/dist/format.ts'] = "export const f = 1"                         # v1 gap: path word "dist"
    $c['docs/review-handoff/guide.md'] = "# guide"                              # v1 gap
    $c['src/env-ref.ts'] = "const $($script:KW) = process.env.APP_KEY"
    $c['src/go-cfg.go'] = "Password: cfg.Password,"
    $c['src/label.tsx'] = "label: 'Password confirmation',"
    $c['docs/placeholders.md'] = "$($script:KW): $q<your-password>$q`ntoken: `${NPM_TOKEN}`nAuthorization: Bearer <token>"
    $c['src/length.ts'] = "const passwordLength = 12"
    $c['src/digest.ts'] = "const digest = 'e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855'"
    $c['src/lock.json'] = "{ ${q}integrity$q`: ${q}sha512-" + ('Ab1' * 28) + "==$q }"
    return $c
}

# Known limitations: documented, not detected. Reported as INFO, never as PASS.
function Get-KnownGaps {
    $c = [ordered]@{}
    $c['src/shifted.ts'] = "const x = '" + (("const $($script:KW) = '$($script:VAL)'".ToCharArray() | ForEach-Object { [char](([int]$_) + 1) }) -join '') + "'"
    $chars = ("$($script:KW)=$($script:VAL)").ToCharArray()
    [array]::Reverse($chars)
    $c['src/reversed.txt'] = -join $chars
    return $c
}
