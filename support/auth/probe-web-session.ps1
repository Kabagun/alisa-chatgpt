[CmdletBinding()]
param(
    [switch]$SelfCheck,
    [switch]$RequestSession,
    [switch]$IncludeProvidedClearance,
    [string]$InputCurlPath
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

# Unofficial, fixed ChatGPT web-session endpoint. This is a bounded diagnostic,
# not proof that unattended renewal will continue to work in the future.
$script:SessionUri = [Uri]'https://chatgpt.com/api/auth/session'
$script:CookiePrefix = '__Secure-next-auth.session-token'
$script:MaximumBytes = 1MB

function New-ProbeResult {
    param([string]$Status = 'input_invalid')
    return [ordered]@{
        status = $Status
        httpStatus = $null
        challengeDetected = $false
        contentTypeCategory = $null
        providedClearanceIncluded = $false
        tokenPresent = $false
        identityMatch = $null
        expiryMinutes = $null
        cookieRotation = $false
        encryptedPackFilename = $null
    }
}

function Split-LiteralCurl {
    param([string]$Text)
    if ([string]::IsNullOrWhiteSpace($Text) -or $Text.Length -gt $script:MaximumBytes) { throw 'input_invalid' }
    $tokens = [Collections.Generic.List[string]]::new()
    $current = [Text.StringBuilder]::new()
    $quote = [char]0
    $started = $false
    for ($i = 0; $i -lt $Text.Length; $i++) {
        $ch = $Text[$i]
        if ($quote -ne [char]0) {
            if ($ch -eq $quote) { $quote = [char]0; continue }
            if ($quote -eq '"' -and $ch -in @([char]92, [char]96) -and $i + 1 -lt $Text.Length -and $Text[$i + 1] -in @([char]34, [char]92)) {
                $i++; [void]$current.Append($Text[$i]); continue
            }
            [void]$current.Append($ch)
            continue
        }
        if ($ch -in @([char]92, [char]96, [char]94) -and $i + 1 -lt $Text.Length -and $Text[$i + 1] -in @([char]10, [char]13)) {
            $i++
            if ($Text[$i] -eq [char]13 -and $i + 1 -lt $Text.Length -and $Text[$i + 1] -eq [char]10) { $i++ }
            continue
        }
        if ([char]::IsWhiteSpace($ch)) {
            if ($started) { $tokens.Add($current.ToString()); [void]$current.Clear(); $started = $false }
            continue
        }
        if ($ch -in @([char]34, [char]39)) { $quote = $ch; $started = $true; continue }
        # Bash's escaped apostrophe between adjacent quoted fragments is literal.
        if ($ch -eq [char]92 -and $i + 1 -lt $Text.Length -and $Text[$i + 1] -eq [char]39) {
            $i++; [void]$current.Append($Text[$i]); $started = $true; continue
        }
        [void]$current.Append($ch); $started = $true
    }
    if ($quote -ne [char]0) { throw 'input_invalid' }
    if ($started) { $tokens.Add($current.ToString()) }
    if ($tokens.Count -eq 0 -or [IO.Path]::GetFileName($tokens[0]) -notin @('curl', 'curl.exe')) { throw 'input_invalid' }
    return ,$tokens.ToArray()
}

function Assert-SessionCookieSet {
    param([Collections.Generic.Dictionary[string, string]]$Cookies)
    $sessionNames = @($Cookies.Keys | Where-Object { $_.StartsWith($script:CookiePrefix, [StringComparison]::Ordinal) })
    if ($sessionNames.Count -lt 1 -or $sessionNames.Count -gt 32) { throw 'input_invalid' }
    $hasWhole = $Cookies.ContainsKey($script:CookiePrefix)
    if ($hasWhole) {
        if ($sessionNames.Count -ne 1) { throw 'input_invalid' }
        return
    }
    for ($index = 0; $index -lt $sessionNames.Count; $index++) {
        if (-not $Cookies.ContainsKey("$($script:CookiePrefix).$index")) { throw 'input_invalid' }
    }
}

function Get-SessionCookieName {
    param([string]$Name)
    if (-not $Name.StartsWith($script:CookiePrefix, [StringComparison]::Ordinal)) { return $null }
    if ($Name -notmatch '^__Secure-next-auth\.session-token(?:\.(?:0|[1-9][0-9]?))?$') { throw 'input_invalid' }
    return $Name
}

function Assert-CookieValue {
    param([string]$Value)
    if ([string]::IsNullOrEmpty($Value) -or $Value.Length -gt 65536 -or $Value -notmatch '^[A-Za-z0-9._~-]+$') { throw 'input_invalid' }
}

function Assert-ProvidedClearanceValue {
    param([string]$Value)
    # RFC cookie-octet characters only: no controls, whitespace, quotes,
    # comma, semicolon, or backslash. Values are used exactly as supplied.
    if ([string]::IsNullOrEmpty($Value) -or $Value.Length -gt 65536 -or $Value -notmatch '^[\x21\x23-\x2B\x2D-\x3A\x3C-\x5B\x5D-\x7E]+$') { throw 'input_invalid' }
}

function Read-JwtIdentity {
    param([string]$Token)
    if ([string]::IsNullOrWhiteSpace($Token) -or $Token.Length -gt 65536) { throw 'input_invalid' }
    $parts = $Token.Split('.')
    if ($parts.Count -ne 3) { throw 'input_invalid' }
    foreach ($part in $parts) { if ($part -notmatch '^[A-Za-z0-9_-]+$') { throw 'input_invalid' } }
    $payload = $parts[1].Replace('-', '+').Replace('_', '/')
    if ($payload.Length % 4 -eq 1) { throw 'input_invalid' }
    $payload = $payload.PadRight($payload.Length + ((4 - $payload.Length % 4) % 4), '=')
    $bytes = [Convert]::FromBase64String($payload)
    if ($bytes.Length -gt 16384) { throw 'input_invalid' }
    $document = $null
    try {
        $document = [Text.Json.JsonDocument]::Parse([Text.Encoding]::UTF8.GetString($bytes))
        $root = $document.RootElement
        $sub = $root.GetProperty('sub').GetString()
        $iss = $root.GetProperty('iss').GetString()
        $exp = $root.GetProperty('exp').GetInt64()
        if ([string]::IsNullOrWhiteSpace($sub) -or [string]::IsNullOrWhiteSpace($iss) -or $sub.Length -gt 512 -or $iss.Length -gt 512) { throw 'input_invalid' }
        return [pscustomobject]@{ Subject = $sub; Issuer = $iss; ExpiryUnixSeconds = $exp; ExpiresUtc = [DateTimeOffset]::FromUnixTimeSeconds($exp) }
    }
    finally {
        if ($null -ne $document) { $document.Dispose() }
        [Array]::Clear($bytes, 0, $bytes.Length)
    }
}

function Read-CurlCredentials {
    param([string]$Text, [switch]$IncludeProvidedClearance)
    $tokens = Split-LiteralCurl $Text
    $cookieInputs = [Collections.Generic.List[string]]::new()
    $bearerInputs = [Collections.Generic.List[string]]::new()
    $userAgents = [Collections.Generic.List[string]]::new()
    for ($i = 1; $i -lt $tokens.Count; $i++) {
        $option = $tokens[$i]
        $kind = $null; $value = $null
        if ($option -in @('-H', '--header', '-b', '--cookie', '-A', '--user-agent')) {
            if ($i + 1 -ge $tokens.Count) { throw 'input_invalid' }
            $kind = $option; $i++; $value = $tokens[$i]
        }
        elseif ($option -match '^(--header|--cookie|--user-agent)=(.*)$') { $kind = $Matches[1]; $value = $Matches[2] }
        elseif ($option -match '^(-H|-b|-A)(.+)$') { $kind = $Matches[1]; $value = $Matches[2] }
        if ($null -eq $kind) { continue }
        if ($kind -in @('-b', '--cookie')) { $cookieInputs.Add($value); continue }
        if ($kind -in @('-A', '--user-agent')) { $userAgents.Add($value); continue }
        $colon = $value.IndexOf(':')
        if ($colon -lt 1) { continue }
        $headerName = $value.Substring(0, $colon).Trim()
        $headerValue = $value.Substring($colon + 1).Trim()
        if ($headerName -ieq 'Cookie') { $cookieInputs.Add($headerValue) }
        elseif ($headerName -ieq 'Authorization') {
            if ($headerValue -notmatch '^Bearer[ \t]+([^ \t\r\n]+)$') { throw 'input_invalid' }
            $bearerInputs.Add($Matches[1])
        }
        elseif ($headerName -ieq 'User-Agent') { $userAgents.Add($headerValue) }
    }
    if ($bearerInputs.Count -ne 1 -or $userAgents.Count -gt 1) { throw 'input_invalid' }
    $cookies = [Collections.Generic.Dictionary[string, string]]::new([StringComparer]::Ordinal)
    foreach ($inputValue in $cookieInputs) {
        foreach ($pair in $inputValue.Split(';')) {
            $equals = $pair.IndexOf('=')
            if ($equals -lt 1) { continue }
            $providedName = $pair.Substring(0, $equals).Trim()
            $name = Get-SessionCookieName $providedName
            $isSessionCookie = $null -ne $name
            if (-not $isSessionCookie -and $IncludeProvidedClearance -and $providedName -cin @('cf_clearance', '__cf_bm')) { $name = $providedName }
            if ($null -eq $name) { continue }
            $cookieValue = $pair.Substring($equals + 1)
            if ($isSessionCookie) { $cookieValue = $cookieValue.Trim(); Assert-CookieValue $cookieValue }
            else { Assert-ProvidedClearanceValue $cookieValue }
            if (-not $cookies.TryAdd($name, $cookieValue)) { throw 'input_invalid' }
        }
    }
    Assert-SessionCookieSet $cookies
    $ua = $null
    if ($userAgents.Count -eq 1) {
        if ($userAgents[0] -match '[\r\n]') { throw 'input_invalid' }
        if ($IncludeProvidedClearance) {
            # Clearance is used only with the exact supplied, safe UA. Do not
            # normalize it into a different client identity or invent one.
            if ($userAgents[0].Length -gt 512 -or $userAgents[0] -match '[^\x20-\x7E]') { throw 'input_invalid' }
            $ua = $userAgents[0]
        }
        else {
            $ua = [regex]::Replace($userAgents[0], '[^\x20-\x7E]', '').Trim()
            if ($ua.Length -gt 512) { $ua = $ua.Substring(0, 512) }
        }
        if ($ua.Length -eq 0) { $ua = $null }
    }
    $identity = Read-JwtIdentity $bearerInputs[0]
    return [pscustomobject]@{ Cookies = $cookies; AccessToken = $bearerInputs[0]; UserAgent = $ua; Identity = $identity; ProvidedClearanceIncluded = ($cookies.ContainsKey('cf_clearance') -or $cookies.ContainsKey('__cf_bm')) }
}

function New-CredentialPack {
    param($Credentials, [DateTimeOffset]$Now)
    $cookieRecords = [ordered]@{}
    foreach ($name in ($Credentials.Cookies.Keys | Sort-Object)) { $cookieRecords[$name] = $Credentials.Cookies[$name] }
    return [ordered]@{
        schemaVersion = 1
        source = 'chatgpt-web-session-probe-unofficial'
        savedAtUtc = $Now.ToUniversalTime().ToString('o')
        cookies = $cookieRecords
        accessToken = $Credentials.AccessToken
        userAgent = $Credentials.UserAgent
        providedClearanceIncluded = $Credentials.ProvidedClearanceIncluded
        expiresUtc = $Credentials.Identity.ExpiresUtc.ToUniversalTime().ToString('o')
    }
}

function Write-EncryptedPack {
    param($Pack, [string]$Path, [switch]$Replace)
    $plainBytes = [Text.Encoding]::UTF8.GetBytes(($Pack | ConvertTo-Json -Depth 5 -Compress))
    $encrypted = $null; $temporaryPath = $null
    try {
        $encrypted = [Security.Cryptography.ProtectedData]::Protect($plainBytes, $null, [Security.Cryptography.DataProtectionScope]::CurrentUser)
        if ($Replace) {
            $temporaryPath = Join-Path ([IO.Path]::GetDirectoryName($Path)) ('.web-session-probe-' + [Guid]::NewGuid().ToString('N') + '.tmp')
            [IO.File]::WriteAllBytes($temporaryPath, $encrypted)
            [IO.File]::Replace($temporaryPath, $Path, [System.Management.Automation.Language.NullString]::Value)
            $temporaryPath = $null
        }
        else {
            $stream = [IO.FileStream]::new($Path, [IO.FileMode]::CreateNew, [IO.FileAccess]::Write, [IO.FileShare]::None)
            try { $stream.Write($encrypted, 0, $encrypted.Length); $stream.Flush($true) } finally { $stream.Dispose() }
        }
    }
    finally {
        [Array]::Clear($plainBytes, 0, $plainBytes.Length)
        if ($null -ne $encrypted) { [Array]::Clear($encrypted, 0, $encrypted.Length) }
        if ($null -ne $temporaryPath -and [IO.File]::Exists($temporaryPath)) { [IO.File]::Delete($temporaryPath) }
    }
}

function Merge-SessionCookies {
    param($Original, [string[]]$SetCookieHeaders, [DateTimeOffset]$Now)
    $updated = [Collections.Generic.Dictionary[string, string]]::new([StringComparer]::Ordinal)
    foreach ($entry in $Original.GetEnumerator()) { $updated.Add($entry.Key, $entry.Value) }
    # Set-Cookie normally uses separate header fields. Also handle a combined
    # field without splitting the comma inside an Expires date.
    $cookieLines = foreach ($rawLine in $SetCookieHeaders) { [regex]::Split($rawLine, ',(?=\s*[^=;,\s]+=)') }
    foreach ($line in $cookieLines) {
        $attributes = $line.Split(';')
        $equals = $attributes[0].IndexOf('=')
        if ($equals -lt 1) { continue }
        $name = Get-SessionCookieName $attributes[0].Substring(0, $equals).Trim()
        if ($null -eq $name) { continue }
        $value = $attributes[0].Substring($equals + 1).Trim()
        $delete = $value.Length -eq 0
        $maxAgeOverride = $null
        $expiresInPast = $false
        foreach ($attribute in $attributes | Select-Object -Skip 1) {
            if ($attribute -match '^\s*Max-Age\s*=\s*(-?[0-9]+)\s*$') {
                $maxAge = 0L
                if (-not [long]::TryParse($Matches[1], [ref]$maxAge)) { throw 'response_invalid' }
                $maxAgeOverride = $maxAge
            }
            elseif ($attribute -match '^\s*Expires\s*=\s*(.+?)\s*$') {
                $expires = [DateTimeOffset]::MinValue
                if ([DateTimeOffset]::TryParse($Matches[1], [Globalization.CultureInfo]::InvariantCulture, [Globalization.DateTimeStyles]::AssumeUniversal, [ref]$expires) -and $expires -le $Now) { $expiresInPast = $true }
            }
        }
        if ($null -ne $maxAgeOverride) { $delete = $delete -or $maxAgeOverride -le 0 }
        else { $delete = $delete -or $expiresInPast }
        if ($delete) { [void]$updated.Remove($name) }
        else { Assert-CookieValue $value; $updated[$name] = $value }
    }
    Assert-SessionCookieSet $updated
    $changed = $updated.Count -ne $Original.Count
    foreach ($entry in $updated.GetEnumerator()) {
        if (-not $Original.ContainsKey($entry.Key) -or $Original[$entry.Key] -cne $entry.Value) { $changed = $true }
    }
    return [pscustomobject]@{ Cookies = $updated; Changed = $changed }
}

function Invoke-BoundedSessionGet {
    param($Credentials, [Net.Http.HttpMessageHandler]$TestHandler)
    $handler = $TestHandler
    if ($null -eq $handler) {
        $handler = [Net.Http.HttpClientHandler]::new()
        $handler.AllowAutoRedirect = $false
        $handler.UseCookies = $false
        $handler.MaxResponseHeadersLength = 64
        $handler.AutomaticDecompression = [Net.DecompressionMethods]::GZip -bor [Net.DecompressionMethods]::Deflate -bor [Net.DecompressionMethods]::Brotli
        # The platform's normal TLS certificate verification remains enabled.
    }
    $client = [Net.Http.HttpClient]::new($handler, $true)
    $client.Timeout = [Threading.Timeout]::InfiniteTimeSpan
    $cancellation = [Threading.CancellationTokenSource]::new([TimeSpan]::FromSeconds(20))
    $request = [Net.Http.HttpRequestMessage]::new([Net.Http.HttpMethod]::Get, $script:SessionUri)
    $response = $null
    try {
        $cookieParts = foreach ($name in ($Credentials.Cookies.Keys | Sort-Object)) { $name + '=' + $Credentials.Cookies[$name] }
        if (-not $request.Headers.TryAddWithoutValidation('Cookie', ($cookieParts -join '; '))) { throw 'input_invalid' }
        if ($null -ne $Credentials.UserAgent -and -not $request.Headers.TryAddWithoutValidation('User-Agent', $Credentials.UserAgent)) { throw 'input_invalid' }
        $response = $client.SendAsync($request, [Net.Http.HttpCompletionOption]::ResponseHeadersRead, $cancellation.Token).GetAwaiter().GetResult()
        $status = [int]$response.StatusCode
        $setCookie = $null
        $cookieHeaders = @()
        if ($response.Headers.TryGetValues('Set-Cookie', [ref]$setCookie)) { $cookieHeaders = @($setCookie) }
        $challengeValues = $null
        $challengeDetected = $false
        if ($response.Headers.TryGetValues('cf-mitigated', [ref]$challengeValues)) {
            foreach ($value in $challengeValues) {
                if ([string]::Equals($value.Trim(), 'challenge', [StringComparison]::OrdinalIgnoreCase)) { $challengeDetected = $true }
            }
        }
        $contentTypeCategory = 'other'
        $contentType = $response.Content.Headers.ContentType
        if ($null -ne $contentType) {
            $mediaType = $contentType.MediaType.ToLowerInvariant()
            if ($mediaType -eq 'application/json' -or $mediaType.EndsWith('+json', [StringComparison]::Ordinal)) { $contentTypeCategory = 'json' }
            elseif ($mediaType -in @('text/html', 'application/xhtml+xml')) { $contentTypeCategory = 'html' }
        }
        $httpResult = [pscustomobject]@{ Status = $status; Body = $null; SetCookieHeaders = $cookieHeaders; Failure = $null; ChallengeDetected = $challengeDetected; ContentTypeCategory = $contentTypeCategory }
        # These diagnostics use only normalized headers. Error bodies are not read.
        if ($status -ne 200) { return $httpResult }
        if ($null -ne $response.Content.Headers.ContentLength -and $response.Content.Headers.ContentLength -gt $script:MaximumBytes) { $httpResult.Failure = 'response_too_large'; return $httpResult }
        $bodyStream = $response.Content.ReadAsStreamAsync($cancellation.Token).GetAwaiter().GetResult()
        $memory = [IO.MemoryStream]::new()
        $buffer = [byte[]]::new(8192)
        try {
            while (($read = $bodyStream.ReadAsync($buffer, 0, $buffer.Length, $cancellation.Token).GetAwaiter().GetResult()) -gt 0) {
                if ($memory.Length + $read -gt $script:MaximumBytes) { $httpResult.Failure = 'response_too_large'; return $httpResult }
                $memory.Write($buffer, 0, $read)
            }
            $httpResult.Body = [Text.Encoding]::UTF8.GetString($memory.ToArray())
            return $httpResult
        }
        finally { [Array]::Clear($buffer, 0, $buffer.Length); $memory.Dispose(); $bodyStream.Dispose() }
    }
    finally {
        if ($null -ne $response) { $response.Dispose() }
        $request.Dispose(); $cancellation.Dispose(); $client.Dispose()
    }
}

function Invoke-SessionFlow {
    param([string]$CurlText, [string]$RuntimeDirectory, [Net.Http.HttpMessageHandler]$TestHandler, [DateTimeOffset]$Now = [DateTimeOffset]::UtcNow, [switch]$IncludeProvidedClearance)
    $result = New-ProbeResult
    $failureStatus = 'input_invalid'
    $document = $null
    try {
        if (-not [OperatingSystem]::IsWindows()) { $result.status = 'windows_required'; return $result }
        $credentials = Read-CurlCredentials $CurlText -IncludeProvidedClearance:$IncludeProvidedClearance
        $result.providedClearanceIncluded = $credentials.ProvidedClearanceIncluded
        $filename = 'web-session-probe-' + [Guid]::NewGuid().ToString('N') + '.dpapi'
        $packPath = Join-Path $RuntimeDirectory $filename
        $failureStatus = 'pack_write_failed'
        Write-EncryptedPack (New-CredentialPack $credentials $Now) $packPath
        $result.encryptedPackFilename = $filename
        $failureStatus = 'request_failed'
        $http = Invoke-BoundedSessionGet $credentials $TestHandler
        $result.httpStatus = $http.Status
        $result.challengeDetected = $http.ChallengeDetected
        $result.contentTypeCategory = $http.ContentTypeCategory
        if ($null -ne $http.Failure) { $result.status = $http.Failure; return $result }
        if ($http.Status -eq 401) { $result.status = 'http_unauthorized'; return $result }
        if ($http.Status -eq 403) { $result.status = 'http_forbidden'; return $result }
        if ($http.Status -ge 300 -and $http.Status -lt 400) { $result.status = 'redirect_blocked'; return $result }
        if ($http.Status -ne 200) { $result.status = 'http_error'; return $result }
        $failureStatus = 'response_invalid'
        $document = [Text.Json.JsonDocument]::Parse($http.Body)
        $property = [Text.Json.JsonElement]::new()
        if (-not $document.RootElement.TryGetProperty('accessToken', [ref]$property) -or $property.ValueKind -ne [Text.Json.JsonValueKind]::String) { $result.status = 'token_missing'; return $result }
        $candidate = $property.GetString()
        $result.tokenPresent = -not [string]::IsNullOrWhiteSpace($candidate)
        if (-not $result.tokenPresent) { $result.status = 'token_missing'; return $result }
        $identity = Read-JwtIdentity $candidate
        $result.identityMatch = $identity.Subject -ceq $credentials.Identity.Subject -and $identity.Issuer -ceq $credentials.Identity.Issuer
        $result.expiryMinutes = [long][Math]::Floor(($identity.ExpiresUtc - $Now).TotalMinutes)
        if (-not $result.identityMatch) { $result.status = 'identity_mismatch'; return $result }
        if ($identity.ExpiresUtc -le $Now) { $result.status = 'token_expired'; return $result }
        $merged = Merge-SessionCookies $credentials.Cookies $http.SetCookieHeaders $Now
        $updatedCredentials = [pscustomobject]@{ Cookies = $merged.Cookies; AccessToken = $candidate; UserAgent = $credentials.UserAgent; Identity = $identity; ProvidedClearanceIncluded = $credentials.ProvidedClearanceIncluded }
        $pack = New-CredentialPack $updatedCredentials $Now
        # Read only the optional session-expiry field, never user/account data.
        $sessionExpiry = [Text.Json.JsonElement]::new()
        if ($document.RootElement.TryGetProperty('expires', [ref]$sessionExpiry) -and $sessionExpiry.ValueKind -eq [Text.Json.JsonValueKind]::String) {
            $sessionExpiresAt = [DateTimeOffset]::MinValue
            if ([DateTimeOffset]::TryParse($sessionExpiry.GetString(), [Globalization.CultureInfo]::InvariantCulture, [Globalization.DateTimeStyles]::AssumeUniversal, [ref]$sessionExpiresAt)) { $pack.sessionExpiresUtc = $sessionExpiresAt.ToUniversalTime().ToString('o') }
        }
        $failureStatus = 'pack_write_failed'
        Write-EncryptedPack $pack $packPath -Replace
        $result.cookieRotation = $merged.Changed
        $result.status = if ($identity.ExpiryUnixSeconds -gt $credentials.Identity.ExpiryUnixSeconds) { 'renewal_observed' } else { 'session_fetch_proven' }
        return $result
    }
    catch {
        # Never emit raw exception messages: they can contain input or response data.
        $result.status = $failureStatus
        return $result
    }
    finally { if ($null -ne $document) { $document.Dispose() } }
}

function Invoke-SyntheticSelfCheck {
    if (-not [OperatingSystem]::IsWindows()) { throw 'windows_required' }
    if ($null -eq ('WebSessionProbeSyntheticHandler' -as [type])) {
        Add-Type -TypeDefinition @'
using System;
using System.Net;
using System.Net.Http;
using System.Threading;
using System.Threading.Tasks;
public sealed class WebSessionProbeSyntheticHandler : HttpMessageHandler {
    public int Calls;
    public int Status = 200;
    public string Body = "{}";
    public string ContentType = "application/json";
    public string CfMitigated;
    public string[] Cookies = new string[0];
    public string RequestUri;
    public string RequestMethod;
    public bool AuthorizationPresent;
    public string RequestCookies;
    public string UserAgent;
    protected override Task<HttpResponseMessage> SendAsync(HttpRequestMessage request, CancellationToken cancellationToken) {
        Calls++;
        RequestUri = request.RequestUri.AbsoluteUri;
        RequestMethod = request.Method.Method;
        AuthorizationPresent = request.Headers.Contains("Authorization");
        System.Collections.Generic.IEnumerable<string> values;
        RequestCookies = request.Headers.TryGetValues("Cookie", out values) ? string.Join("; ", values) : "";
        UserAgent = request.Headers.TryGetValues("User-Agent", out values) ? string.Join(" ", values) : "";
        var response = new HttpResponseMessage((HttpStatusCode)Status);
        response.Content = new StringContent(Body);
        response.Content.Headers.ContentType = new System.Net.Http.Headers.MediaTypeHeaderValue(ContentType);
        if (CfMitigated != null) response.Headers.TryAddWithoutValidation("cf-mitigated", CfMitigated);
        if (Cookies.Length > 0) response.Headers.TryAddWithoutValidation("Set-Cookie", Cookies);
        if (Status == 302) response.Headers.Location = new Uri("https://example.invalid/never-follow");
        return Task.FromResult(response);
    }
}
'@
    }
    function Assert-Synthetic([bool]$Condition) { if (-not $Condition) { throw 'self_check_failed' } }
    function New-SyntheticToken([long]$Expires, [string]$Subject = 'synthetic-subject', [string]$Issuer = 'https://issuer.invalid') {
        $payload = [Text.Encoding]::UTF8.GetBytes((@{ sub = $Subject; iss = $Issuer; exp = $Expires } | ConvertTo-Json -Compress))
        return 'eyJhbGciOiJIUzI1NiJ9.' + [Convert]::ToBase64String($payload).TrimEnd('=').Replace('+', '-').Replace('/', '_') + '.c3ludGhldGlj'
    }
    function Read-SyntheticPack([string]$Path) {
        $cipher = [IO.File]::ReadAllBytes($Path)
        $plain = [Security.Cryptography.ProtectedData]::Unprotect($cipher, $null, [Security.Cryptography.DataProtectionScope]::CurrentUser)
        try { return [Text.Encoding]::UTF8.GetString($plain) | ConvertFrom-Json -AsHashtable } finally { [Array]::Clear($plain, 0, $plain.Length) }
    }
    $ownedPaths = [Collections.Generic.List[string]]::new()
    $now = [DateTimeOffset]::FromUnixTimeSeconds(1800000000)
    $oldToken = New-SyntheticToken ($now.ToUnixTimeSeconds() + 3600)
    $newToken = New-SyntheticToken ($now.ToUnixTimeSeconds() + 7200)
    $curl = "curl 'https://example.invalid/ignored' -H 'Authorization: Bearer $oldToken' --cookie 'unrelated=never-send; __Secure-next-auth.session-token.0=chunkA; __Secure-next-auth.session-token.1=chunkB; cf_clearance=providedClearanceMarker; __cf_bm=providedBmMarker' -H 'User-Agent: Synthetic/1.0'"
    try {
        $parsed = Read-CurlCredentials $curl
        Assert-Synthetic ($parsed.Cookies.Count -eq 2 -and $parsed.UserAgent -ceq 'Synthetic/1.0' -and -not $parsed.ProvidedClearanceIncluded)
        $withClearance = Read-CurlCredentials $curl -IncludeProvidedClearance
        Assert-Synthetic ($withClearance.Cookies.Count -eq 4 -and $withClearance.ProvidedClearanceIncluded -and $withClearance.UserAgent -ceq 'Synthetic/1.0')
        foreach ($invalidClearance in @(
            $curl.Replace('unrelated=never-send', 'cf_clearance=duplicate'),
            $curl.Replace('providedBmMarker', 'unsafe value'),
            $curl.Replace('Synthetic/1.0', ('S' * 513))
        )) {
            $rejected = $false
            try { $null = Read-CurlCredentials $invalidClearance -IncludeProvidedClearance } catch { $rejected = $true }
            Assert-Synthetic $rejected
        }
        foreach ($invalid in @(
            $curl.Replace('session-token.1=chunkB', 'session-token.2=chunkB'),
            $curl.Replace('unrelated=never-send', '__Secure-next-auth.session-token.0=duplicate'),
            $curl.Replace('unrelated=never-send', '__Secure-next-auth.session-token=whole'),
            $curl.Replace('Synthetic/1.0', "Synthetic`r`nInjected: bad")
        )) {
            $rejected = $false
            try { $null = Read-CurlCredentials $invalid } catch { $rejected = $true }
            Assert-Synthetic $rejected
        }
        $headerCurl = $curl.Replace("--cookie '", "-H 'Cookie: ")
        Assert-Synthetic ((Read-CurlCredentials $headerCurl).Cookies.Count -eq 2)
        $continuedCurl = $curl.Replace(' -H ', " \`r`n-H ")
        Assert-Synthetic ((Read-CurlCredentials $continuedCurl).Cookies.Count -eq 2)
        $mergedCombined = Merge-SessionCookies $parsed.Cookies @('unrelated=ignore; Path=/, __Secure-next-auth.session-token.0=combined; Expires=Thu, 01 Jan 1970 00:00:00 GMT; Max-Age=3600, __Secure-next-auth.session-token.1=; Max-Age=0') $now
        Assert-Synthetic ($mergedCombined.Changed -and $mergedCombined.Cookies.Count -eq 1 -and $mergedCombined.Cookies['__Secure-next-auth.session-token.0'] -ceq 'combined')
        foreach ($scenario in @('unchanged', 'provided_clearance', 'renewed', 'mismatch', 'issuer_mismatch', 'forbidden', 'forbidden_challenge', 'forbidden_other', 'unauthorized', 'redirect', 'oversize', 'missing', 'invalid_rotation', 'expired')) {
            $handler = [WebSessionProbeSyntheticHandler]::new()
            $candidate = if ($scenario -in @('unchanged', 'provided_clearance')) { $oldToken } else { $newToken }
            if ($scenario -eq 'mismatch') { $candidate = New-SyntheticToken ($now.ToUnixTimeSeconds() + 7200) 'different-subject' }
            if ($scenario -eq 'issuer_mismatch') { $candidate = New-SyntheticToken ($now.ToUnixTimeSeconds() + 7200) 'synthetic-subject' 'https://different.invalid' }
            if ($scenario -eq 'expired') { $candidate = New-SyntheticToken ($now.ToUnixTimeSeconds() - 60) }
            $handler.Body = @{ accessToken = $candidate; expires = $now.AddDays(30).ToString('o'); user = @{ private = 'must-not-be-saved' } } | ConvertTo-Json -Depth 3 -Compress
            if ($scenario -eq 'renewed') { $handler.Cookies = @('__Secure-next-auth.session-token.0=rotated; Path=/; Secure', '__Secure-next-auth.session-token.1=; Max-Age=0; Path=/', 'unrelated=ignore; Path=/') }
            if ($scenario -eq 'invalid_rotation') { $handler.Cookies = @('__Secure-next-auth.session-token.0=; Max-Age=0; Path=/') }
            if ($scenario -eq 'forbidden') { $handler.Status = 403 }
            if ($scenario -eq 'forbidden_challenge') { $handler.Status = 403; $handler.CfMitigated = 'challenge'; $handler.ContentType = 'text/html'; $handler.Body = '<html>must-not-be-saved-challenge</html>' }
            if ($scenario -eq 'forbidden_other') { $handler.Status = 403; $handler.CfMitigated = 'unrelated-sensitive-header-marker'; $handler.ContentType = 'application/octet-stream'; $handler.Body = 'must-not-be-saved-other' }
            if ($scenario -eq 'unauthorized') { $handler.Status = 401 }
            if ($scenario -eq 'redirect') { $handler.Status = 302 }
            if ($scenario -eq 'oversize') { $handler.Body = 'x' * ($script:MaximumBytes + 1) }
            if ($scenario -eq 'missing') { $handler.Body = '{"user":{"private":"must-not-be-saved"}}' }
            $includeClearance = $scenario -eq 'provided_clearance'
            $result = Invoke-SessionFlow $curl $PSScriptRoot $handler $now -IncludeProvidedClearance:$includeClearance
            Assert-Synthetic ($null -ne $result.encryptedPackFilename)
            $path = Join-Path $PSScriptRoot $result.encryptedPackFilename
            $ownedPaths.Add($path)
            Assert-Synthetic ($handler.Calls -eq 1 -and $handler.RequestUri -ceq 'https://chatgpt.com/api/auth/session' -and $handler.RequestMethod -ceq 'GET' -and -not $handler.AuthorizationPresent)
            Assert-Synthetic ($handler.RequestCookies -notmatch 'unrelated' -and $handler.UserAgent -ceq 'Synthetic/1.0')
            $sentCookieNames = @($handler.RequestCookies.Split(';') | ForEach-Object { $_.Split('=')[0].Trim() } | Sort-Object)
            $expectedCookieNames = if ($includeClearance) { @('__cf_bm', '__Secure-next-auth.session-token.0', '__Secure-next-auth.session-token.1', 'cf_clearance') | Sort-Object } else { @('__Secure-next-auth.session-token.0', '__Secure-next-auth.session-token.1') | Sort-Object }
            Assert-Synthetic (($sentCookieNames -join ',') -ceq ($expectedCookieNames -join ',') -and $result.providedClearanceIncluded -eq $includeClearance)
            $pack = Read-SyntheticPack $path
            Assert-Synthetic (-not $pack.ContainsKey('user') -and -not (($pack | ConvertTo-Json -Depth 5) -match 'must-not-be-saved'))
            $expected = switch ($scenario) {
                'unchanged' { 'session_fetch_proven' }
                'provided_clearance' { 'session_fetch_proven' }
                'renewed' { 'renewal_observed' }
                'mismatch' { 'identity_mismatch' }
                'issuer_mismatch' { 'identity_mismatch' }
                'forbidden' { 'http_forbidden' }
                'forbidden_challenge' { 'http_forbidden' }
                'forbidden_other' { 'http_forbidden' }
                'unauthorized' { 'http_unauthorized' }
                'redirect' { 'redirect_blocked' }
                'oversize' { 'response_too_large' }
                'missing' { 'token_missing' }
                'invalid_rotation' { 'response_invalid' }
                'expired' { 'token_expired' }
            }
            if ($result.status -cne $expected) { throw ('synthetic_' + $scenario + '_' + $result.status) }
            if ($scenario -eq 'forbidden_challenge') { Assert-Synthetic ($result.challengeDetected -and $result.contentTypeCategory -ceq 'html' -and -not $result.tokenPresent -and $null -eq $result.identityMatch -and -not $result.cookieRotation) }
            elseif ($scenario -eq 'forbidden_other') { Assert-Synthetic (-not $result.challengeDetected -and $result.contentTypeCategory -ceq 'other') }
            else { Assert-Synthetic (-not $result.challengeDetected -and $result.contentTypeCategory -ceq 'json') }
            if ($scenario -eq 'renewed') {
                Assert-Synthetic ($result.cookieRotation -and $pack.accessToken -ceq $newToken -and $pack.cookies.Count -eq 1 -and $pack.cookies['__Secure-next-auth.session-token.0'] -ceq 'rotated')
            }
            elseif ($scenario -eq 'provided_clearance') { Assert-Synthetic ($pack.accessToken -ceq $oldToken -and $pack.cookies.Count -eq 4 -and $pack.providedClearanceIncluded -and $pack.cookies['cf_clearance'] -ceq 'providedClearanceMarker' -and $pack.cookies['__cf_bm'] -ceq 'providedBmMarker') }
            elseif ($scenario -ne 'unchanged') { Assert-Synthetic ($pack.accessToken -ceq $oldToken -and $pack.cookies.Count -eq 2) }
            $serialized = $result | ConvertTo-Json -Compress
            Assert-Synthetic ($serialized -notmatch 'synthetic-subject|issuer.invalid|chunkA|chunkB|eyJ|must-not-be-saved|unrelated-sensitive-header-marker|text/html|application/octet-stream|providedClearanceMarker|providedBmMarker')
        }
    }
    finally { foreach ($ownedPath in $ownedPaths) { if ([IO.File]::Exists($ownedPath)) { [IO.File]::Delete($ownedPath) } } }
}

$finalResult = New-ProbeResult
try {
    if ($RequestSession -and $SelfCheck) { $finalResult.status = 'invocation_invalid' }
    elseif (-not $RequestSession) {
        if (-not [string]::IsNullOrWhiteSpace($InputCurlPath)) { $finalResult.status = 'invocation_invalid' }
        else { Invoke-SyntheticSelfCheck; $finalResult.status = 'self_check_passed' }
    }
    elseif ([string]::IsNullOrWhiteSpace($InputCurlPath)) { $finalResult.status = 'input_invalid' }
    else {
        $file = [IO.FileInfo]::new($InputCurlPath)
        if (-not $file.Exists -or $file.Length -gt $script:MaximumBytes) { $finalResult.status = 'input_invalid' }
        else { $finalResult = Invoke-SessionFlow ([IO.File]::ReadAllText($file.FullName)) $PSScriptRoot -IncludeProvidedClearance:$IncludeProvidedClearance }
    }
}
catch { $finalResult.status = if ($RequestSession) { 'input_invalid' } else { 'self_check_failed' } }
$finalResult | ConvertTo-Json -Compress
