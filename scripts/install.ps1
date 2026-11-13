# Vario installer for Windows (Windows PowerShell 5.1 or PowerShell 7+).
#
#   irm https://raw.githubusercontent.com/nedanwr/vario/master/scripts/install.ps1 | iex
#
#   # A specific version:
#   $env:VARIO_VERSION = 'v0.1.0-beta.1'; irm https://raw.githubusercontent.com/nedanwr/vario/master/scripts/install.ps1 | iex
#   # or: & ([scriptblock]::Create((irm <same URL>))) -Version v0.1.0-beta.1
#
# Downloads a release archive (from GitHub Releases by default; see "Download
# source" below), verifies its SHA-256 checksum, and installs vario.exe into a per-user
# directory (default %LOCALAPPDATA%\Programs\Vario). It needs no administrator
# rights and sends no telemetry. It adds the directory to the *user* PATH
# (HKCU) unless -NoModifyPath is given, never touches Vario's configuration or
# data (%USERPROFILE%\.vario), and never installs or starts a service.
#
# Parameters (each also settable through the environment variable in brackets):
#   -Version VERSION   install this version, e.g. v0.1.0-beta.1      [VARIO_VERSION]
#   -Channel CHANNEL   beta (default) or stable, when no version      [VARIO_CHANNEL]
#   -InstallDir DIR    default %LOCALAPPDATA%\Programs\Vario          [VARIO_INSTALL_DIR]
#   -Source SOURCE     github, or the https:// URL of a download
#                      mirror (releases/<tag>/, channels/)            [VARIO_DOWNLOAD_SOURCE]
#   -NoModifyPath      do not add the install directory to the user PATH [VARIO_NO_MODIFY_PATH=1]
#   -Force             reinstall the same version or downgrade

param(
    [string]$Version = $env:VARIO_VERSION,
    [string]$Channel = $env:VARIO_CHANNEL,
    [string]$InstallDir = $env:VARIO_INSTALL_DIR,
    [string]$Source = $env:VARIO_DOWNLOAD_SOURCE,
    [switch]$NoModifyPath,
    [switch]$Force
)

# --- Download source ----------------------------------------------------------
# Every download URL is built in this file by Resolve-Channel and Get-AssetUrl
# below, from these settings. Moving to Vario's own download domain means
# changing $DefaultSource and the two commands (docs/distribution.md, "Moving to
# a custom domain"); the release workflow checks that both installers agree.
#
#   github        GitHub Releases of $GitHubRepo. A channel's newest release is
#                 found through the GitHub REST API.
#   https://...   a static mirror (e.g. Cloudflare R2 behind a custom domain)
#                 serving releases/<tag>/<file> and channels/<channel>.txt.
$DefaultSource = 'github'
$GitHubRepo = 'nedanwr/vario'
$InstallShCommand = 'curl -fsSL https://raw.githubusercontent.com/nedanwr/vario/master/scripts/install.sh | sh'
# GitHub endpoints; overridable (https:// only) for tests and GitHub mirrors.
$GitHubUrl = $(if ($env:VARIO_GITHUB_URL) { $env:VARIO_GITHUB_URL } else { 'https://github.com' }).TrimEnd('/')
$GitHubApiUrl = $(if ($env:VARIO_GITHUB_API_URL) { $env:VARIO_GITHUB_API_URL } else { 'https://api.github.com' }).TrimEnd('/')
$SafeUrlPattern = '^https://[A-Za-z0-9.-]+(:[0-9]+)?(/[A-Za-z0-9._~/-]*)?$'

$ReleaseTagPattern = '^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?$'
$RunningAsFile = [bool]$MyInvocation.MyCommand.Path

function Write-Step([string]$Message) { Write-Host $Message }
function Write-Warn([string]$Message) { Write-Host "vario-install: warning: $Message" -ForegroundColor Yellow }

# Compares two digit strings of any length without numeric conversion.
function Compare-Digits([string]$X, [string]$Y) {
    if ($X.Length -ne $Y.Length) { return [Math]::Sign($X.Length - $Y.Length) }
    return [Math]::Sign([string]::CompareOrdinal($X, $Y))
}

# SemVer 2.0 precedence of two release tags: -1, 0 or 1.
function Compare-SemVer([string]$A, [string]$B) {
    $aCore, $aPre = $A.TrimStart('v') -split '-', 2
    $bCore, $bPre = $B.TrimStart('v') -split '-', 2
    $ac = $aCore -split '\.'; $bc = $bCore -split '\.'
    for ($i = 0; $i -lt 3; $i++) {
        $c = Compare-Digits $ac[$i] $bc[$i]
        if ($c -ne 0) { return $c }
    }
    if (-not $aPre -and -not $bPre) { return 0 }
    if (-not $aPre) { return 1 }
    if (-not $bPre) { return -1 }
    $ai = @($aPre -split '\.'); $bi = @($bPre -split '\.')
    for ($i = 0; $i -lt [Math]::Min($ai.Length, $bi.Length); $i++) {
        $x = $ai[$i]; $y = $bi[$i]
        $xn = $x -match '^[0-9]+$'; $yn = $y -match '^[0-9]+$'
        if ($xn -and $yn) { $c = Compare-Digits $x $y }
        elseif ($xn) { $c = -1 }
        elseif ($yn) { $c = 1 }
        else { $c = [Math]::Sign([string]::CompareOrdinal($x, $y)) }
        if ($c -ne 0) { return $c }
    }
    return [Math]::Sign($ai.Length - $bi.Length)
}

function Get-VarioArch {
    # A 32-bit PowerShell on 64-bit Windows reports its own architecture in
    # PROCESSOR_ARCHITECTURE and the machine's in PROCESSOR_ARCHITEW6432.
    $names = @($env:PROCESSOR_ARCHITEW6432, $env:PROCESSOR_ARCHITECTURE)
    if ([Environment]::OSVersion.Platform -eq 'Win32NT') {
        # An x64 PowerShell emulated on ARM64 reports AMD64 in its environment;
        # the OS and the processor tell the truth.
        try { $names += [string][System.Runtime.InteropServices.RuntimeInformation]::OSArchitecture } catch { }
        try {
            $cpu = Get-CimInstance -ClassName Win32_Processor -ErrorAction Stop | Select-Object -First 1
            if ($cpu.Architecture -eq 12) { $names += 'ARM64' }
        } catch { }
    }
    if ($names -contains 'ARM64') { return 'arm64' }
    if ($names -contains 'AMD64' -or $names -contains 'X64') { return 'amd64' }
    throw "unsupported CPU architecture ($(($names | Where-Object { $_ }) -join ', ')); Vario is built for 64-bit Windows on amd64 and arm64"
}

# Downloads over HTTPS; anything but a final 200 fails. With -FollowRedirects
# (GitHub, whose release downloads redirect to its storage host) up to 5
# redirects are followed, each only to an https:// URL; a mirror must answer
# directly.
function Get-Download([string]$Url, [string]$OutFile, [string]$Accept = '*/*', [switch]$FollowRedirects) {
    $lastError = $null
    for ($attempt = 1; $attempt -le 3; $attempt++) {
        $response = $null
        $retry = $true
        try {
            $uri = [Uri]$Url
            for ($hop = 0; ; $hop++) {
                if ($uri.Scheme -cne 'https') { $retry = $false; throw "refusing to download from a non-HTTPS URL ($uri)" }
                $request = [System.Net.WebRequest]::Create($uri)
                $request.AllowAutoRedirect = $false
                $request.Timeout = 30000
                $request.ReadWriteTimeout = 120000
                $request.UserAgent = 'vario-installer'
                $request.Accept = $Accept
                try { $response = $request.GetResponse() }
                catch [System.Net.WebException] {
                    # Error statuses (and, on some .NET versions, redirects) throw.
                    if (-not $_.Exception.Response) { throw }
                    $response = $_.Exception.Response
                }
                $code = [int]$response.StatusCode
                if ($FollowRedirects -and $code -in @(301, 302, 303, 307, 308) -and $hop -lt 5) {
                    $location = $response.Headers['Location']
                    $response.Close(); $response = $null
                    if (-not $location) { throw "HTTP $code without a Location" }
                    $uri = New-Object System.Uri($uri, $location)
                    continue
                }
                if ($code -ne 200) {
                    # Client errors (404 for an unknown version, 403 for a rate
                    # limit) will not go away by retrying.
                    if ($code -ge 400 -and $code -lt 500 -and $code -ne 408 -and $code -ne 429) { $retry = $false }
                    throw "HTTP $code"
                }
                break
            }
            $in = $response.GetResponseStream()
            $out = [System.IO.File]::Create($OutFile)
            try { $in.CopyTo($out) } finally { $out.Dispose(); $in.Dispose() }
            if ($response.ContentLength -ge 0 -and (Get-Item -LiteralPath $OutFile).Length -ne $response.ContentLength) {
                throw "incomplete download"
            }
            return
        } catch {
            $lastError = $_.Exception.Message
        } finally {
            if ($response) { $response.Close() }
        }
        Remove-Item -LiteralPath $OutFile -Force -ErrorAction SilentlyContinue
        if (-not $retry) { break }
        if ($attempt -lt 3) { Start-Sleep -Seconds (2 * $attempt) }
    }
    Remove-Item -LiteralPath $OutFile -Force -ErrorAction SilentlyContinue
    throw "download failed: $Url ($lastError). Check your connection and that the version exists; nothing was installed."
}

# --- Releases -----------------------------------------------------------------
# The only code that knows how a source lays out its files. $Src is 'github' or
# a validated mirror URL.

# Stable releases are on both channels; beta and rc prereleases
# (v1.2.0-beta.3, v1.2.0-rc.1) only on beta; other prereleases (alpha, ...) on
# neither. install.sh and scripts/release/r2-publish.sh follow the same rule.
function Test-InChannel([string]$Chan, [string]$Tag) {
    $dash = $Tag.IndexOf('-')
    if ($dash -lt 0) { return $true }
    if ($Chan -cne 'beta') { return $false }
    return $Tag.Substring($dash + 1) -cmatch '^(beta|rc)(\..*)?$'
}

# The newest release tag on a channel.
function Resolve-Channel([string]$Src, [string]$Chan, [string]$Tmp) {
    if ($Src -ceq 'github') {
        # Not /releases/latest: it skips prereleases, and every beta is one.
        # Anonymous requests list only published releases, and the release
        # workflow publishes a release only once all of its assets are uploaded.
        $api = "$GitHubApiUrl/repos/$GitHubRepo/releases?per_page=100"
        $file = Join-Path $Tmp 'releases.json'
        try { Get-Download $api $file -Accept 'application/vnd.github+json' }
        catch {
            throw "could not list Vario releases from GitHub ($($_.Exception.Message)). GitHub allows 60 anonymous API requests an hour per address; try again later, or pass -Version (e.g. -Version v0.1.0-beta.1), which needs no API request"
        }
        $text = [System.IO.File]::ReadAllText($file)
        if (-not $text.TrimStart().StartsWith('[')) { throw "unexpected response from $api; nothing was installed" }
        # Every unescaped "tag_name" key is a release's own (quotes inside
        # release notes are escaped); invalid tags are skipped. Parsed with a
        # regex because ConvertFrom-Json in Windows PowerShell 5.1 rejects
        # responses over 2 MB.
        $newest = $null
        foreach ($m in [regex]::Matches($text, '"tag_name"\s*:\s*"([^"\\]*)"')) {
            $tag = $m.Groups[1].Value
            if ($tag -cnotmatch $ReleaseTagPattern -or -not (Test-InChannel $Chan $tag)) { continue }
            if (-not $newest -or (Compare-SemVer $tag $newest) -gt 0) { $newest = $tag }
        }
        if (-not $newest) { throw "no $Chan release of Vario is published on GitHub yet; pass -Version to choose one" }
        return $newest
    }
    $url = "$Src/channels/$Chan.txt"
    $file = Join-Path $Tmp 'channel'
    Get-Download $url $file
    $bad = "malformed channel metadata at $url"
    if ((Get-Item -LiteralPath $file).Length -gt 64) { throw $bad }
    $lines = @([System.IO.File]::ReadAllLines($file) | ForEach-Object { $_.Trim() } | Where-Object { $_ })
    if ($lines.Count -ne 1 -or $lines[0] -cnotmatch $ReleaseTagPattern) { throw $bad }
    return $lines[0]
}

# URL of a file in a release.
function Get-AssetUrl([string]$Src, [string]$Tag, [string]$File) {
    if ($Src -ceq 'github') { return "$GitHubUrl/$GitHubRepo/releases/download/$Tag/$File" }
    return "$Src/releases/$Tag/$File"
}

# Downloads a file of a release.
function Get-Asset([string]$Src, [string]$Tag, [string]$File, [string]$OutFile) {
    Get-Download (Get-AssetUrl $Src $Tag $File) $OutFile -FollowRedirects:($Src -ceq 'github')
}

function Get-ReportedVersion([string]$Exe) {
    # Only reachable when the installer is tested with pwsh on Linux or macOS,
    # where Expand-Archive and Copy-Item drop the executable bit.
    if ([Environment]::OSVersion.Platform -eq 'Unix') { chmod 0755 $Exe 2>$null }
    try {
        $out = & $Exe version --short 2>$null | Select-Object -First 1
        if ($LASTEXITCODE -eq 0 -and $out) { return ([string]$out).Trim() }
    } catch { }
    return ''
}

function Add-UserPath([string]$Dir) {
    $key = [Microsoft.Win32.Registry]::CurrentUser.OpenSubKey('Environment', $true)
    if (-not $key) { throw "cannot open HKCU\Environment" }
    try {
        # Read the raw value so %VARIABLES% in existing entries are preserved.
        $current = [string]$key.GetValue('Path', '', [Microsoft.Win32.RegistryValueOptions]::DoNotExpandEnvironmentNames)
        $entries = @($current.Split(';') | Where-Object { $_ })
        foreach ($e in $entries) {
            if ([Environment]::ExpandEnvironmentVariables($e).TrimEnd('\') -ieq $Dir.TrimEnd('\')) { return $false }
        }
        $new = (@($entries) + $Dir) -join ';'
        $key.SetValue('Path', $new, [Microsoft.Win32.RegistryValueKind]::ExpandString)
    } finally { $key.Close() }
    # Setting a user variable broadcasts WM_SETTINGCHANGE, so new terminals see the change.
    [Environment]::SetEnvironmentVariable('VARIO_INSTALLER_REFRESH', '1', 'User')
    [Environment]::SetEnvironmentVariable('VARIO_INSTALLER_REFRESH', $null, 'User')
    return $true
}

function Install-Vario([string]$ReqVersion, [string]$ReqChannel, [string]$ReqDir, [string]$ReqSource, [bool]$SkipPath, [bool]$Reinstall) {
    $ErrorActionPreference = 'Stop'
    Set-StrictMode -Version 2

    $onWindows = $env:OS -eq 'Windows_NT'
    if (-not $onWindows) {
        throw "this installer is for Windows; on macOS and Linux run: $InstallShCommand"
    }
    try {
        [Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12
    } catch { }

    $chan = if ($ReqChannel) { $ReqChannel } else { 'beta' }
    if ($chan -cnotin @('beta', 'stable')) { throw "unknown channel '$chan' (expected beta or stable)" }

    $src = if ($ReqSource) { $ReqSource } else { $DefaultSource }
    if ($src -ceq 'github') {
        foreach ($u in @($GitHubUrl, $GitHubApiUrl)) {
            if ($u -cnotmatch $SafeUrlPattern) { throw "the GitHub URLs must be https:// URLs (got '$u')" }
        }
    } else {
        $src = $src.TrimEnd('/')
        if ($src -cnotmatch $SafeUrlPattern) {
            throw "the download source must be github or an https:// URL (got '$src')"
        }
    }

    $dir = $ReqDir
    if (-not $dir) {
        if (-not $env:LOCALAPPDATA) { throw "LOCALAPPDATA is not set; pass -InstallDir" }
        $dir = Join-Path $env:LOCALAPPDATA 'Programs\Vario'
    }
    $dir = [System.IO.Path]::GetFullPath($dir).TrimEnd('\')
    $SkipPath = $SkipPath -or $env:VARIO_NO_MODIFY_PATH -eq '1'

    $arch = Get-VarioArch
    $tmp = Join-Path ([System.IO.Path]::GetTempPath()) ("vario-install-" + [Guid]::NewGuid().ToString('N'))
    New-Item -ItemType Directory -Path $tmp | Out-Null
    try {
        $tag = $ReqVersion
        if ($tag) {
            if (-not $tag.StartsWith('v')) { $tag = "v$tag" }
            if ($tag -cnotmatch $ReleaseTagPattern) { throw "invalid version '$tag' (expected e.g. v0.1.0-beta.1)" }
            Write-Step "Installing Vario $tag for windows/$arch"
        } else {
            $tag = Resolve-Channel $src $chan $tmp
            Write-Step "Installing Vario $tag ($chan channel) for windows/$arch"
        }

        $nameVersion = $tag.Substring(1)
        $stem = "vario_${nameVersion}_windows_$arch"
        $archive = "$stem.zip"
        $target = Join-Path $dir 'vario.exe'

        # Check the existing installation before downloading anything.
        $current = ''
        if (Test-Path -LiteralPath $target) {
            if (Test-Path -LiteralPath $target -PathType Container) { throw "$target exists and is a directory" }
            $current = Get-ReportedVersion $target
            if ($current -cmatch $ReleaseTagPattern) {
                $c = Compare-SemVer $current $tag
                if ($c -eq 0 -and -not $Reinstall) {
                    Write-Step "Vario $current is already installed at $target. Nothing to do (use -Force to reinstall)."
                    return
                }
                if ($c -gt 0 -and -not $Reinstall) {
                    throw "$target is Vario $current, newer than $tag. Downgrades are not supported (local state may not be readable by older versions); pass -Force to install anyway"
                }
            } else {
                Write-Warn "$target exists but does not report a release version; it will be replaced"
                $current = ''
            }
        }

        Write-Step "Downloading $(Get-AssetUrl $src $tag $archive)"
        $sumsFile = Join-Path $tmp 'checksums.txt'
        Get-Asset $src $tag 'checksums.txt' $sumsFile
        $expected = @(Get-Content -LiteralPath $sumsFile | ForEach-Object {
                $f = $_.Trim() -split '\s+'
                if ($f.Count -eq 2 -and ($f[1] -ceq $archive -or $f[1] -ceq "*$archive")) { $f[0] }
            })
        if ($expected.Count -ne 1 -or $expected[0] -cnotmatch '^[0-9a-f]{64}$') {
            throw "checksums.txt has no valid entry for $archive; refusing to install"
        }
        $zip = Join-Path $tmp $archive
        Get-Asset $src $tag $archive $zip
        $actual = (Get-FileHash -LiteralPath $zip -Algorithm SHA256).Hash.ToLowerInvariant()
        if ($actual -ne $expected[0]) {
            throw "checksum mismatch for $archive (expected $($expected[0]), got $actual); the download is incomplete or corrupted. Nothing was installed"
        }
        Write-Step "Verified SHA-256 checksum"

        $x = Join-Path $tmp 'x'
        Expand-Archive -LiteralPath $zip -DestinationPath $x
        $new = Join-Path $x "$stem\vario.exe"
        if (-not (Test-Path -LiteralPath $new -PathType Leaf)) { throw "$archive does not contain $stem\vario.exe" }
        $reported = Get-ReportedVersion $new
        if ($reported -ne $tag) {
            throw "the downloaded binary does not run on this system or reports '$reported' instead of $tag; nothing was replaced"
        }

        if (-not (Test-Path -LiteralPath $dir)) {
            try { New-Item -ItemType Directory -Path $dir -Force | Out-Null }
            catch { throw "cannot create $dir; choose a writable directory with -InstallDir" }
            Write-Step "Created $dir"
        }
        # Leftovers from earlier upgrades whose old binary was still running.
        Get-ChildItem -LiteralPath $dir -Filter 'vario.exe.old*' -ErrorAction SilentlyContinue |
            Remove-Item -Force -ErrorAction SilentlyContinue

        # Stage in the same directory, then swap. A running vario.exe cannot be
        # overwritten, but it can be renamed out of the way.
        $staged = Join-Path $dir 'vario.exe.new'
        try { Copy-Item -LiteralPath $new -Destination $staged -Force }
        catch { throw "$dir is not writable; choose a writable directory with -InstallDir (administrator rights are not needed)" }
        $old = $null
        try {
            if (Test-Path -LiteralPath $target) {
                $old = "$target.old-" + [Guid]::NewGuid().ToString('N').Substring(0, 8)
                Move-Item -LiteralPath $target -Destination $old
            }
            Move-Item -LiteralPath $staged -Destination $target
        } catch {
            if ($old -and -not (Test-Path -LiteralPath $target)) { Move-Item -LiteralPath $old -Destination $target -ErrorAction SilentlyContinue }
            Remove-Item -LiteralPath $staged -Force -ErrorAction SilentlyContinue
            throw "could not replace ${target}: $($_.Exception.Message). The previous installation was kept"
        }
        $stillRunning = $false
        if ($old) {
            Remove-Item -LiteralPath $old -Force -ErrorAction SilentlyContinue
            $stillRunning = Test-Path -LiteralPath $old
        }

        Write-Step ""
        if ($current) { Write-Step "Upgraded Vario $current -> $tag at $target" }
        else { Write-Step "Installed Vario $tag at $target" }
        if ($stillRunning) {
            Write-Step "The previous version is still running; restart any 'vario run' to use $tag."
        }

        $sessionPath = [string][Environment]::GetEnvironmentVariable('PATH')
        $inSession = @($sessionPath.Split([System.IO.Path]::PathSeparator) | Where-Object { $_.TrimEnd('\') -ieq $dir }).Count -gt 0
        if (-not $inSession) {
            if ($SkipPath) {
                Write-Step ""
                Write-Step "$dir is not on your PATH. Add it under Settings > System > About > Advanced system settings > Environment Variables, or run Vario as $target"
            } else {
                if (Add-UserPath $dir) { Write-Step "Added $dir to your user PATH (open a new terminal to use 'vario')" }
                $env:PATH = "$sessionPath;$dir"
            }
        }
        $resolved = Get-Command vario -CommandType Application -ErrorAction SilentlyContinue | Select-Object -First 1
        if ($resolved -and $resolved.Path -ine $target) {
            Write-Warn "'vario' on your PATH resolves to $($resolved.Path), not $target. Remove the other copy or move $dir earlier on your PATH."
        }

        $varioHome = if ($env:VARIO_HOME) { $env:VARIO_HOME } else { Join-Path $env:USERPROFILE '.vario' }
        Write-Step ""
        if (Test-Path -LiteralPath (Join-Path $varioHome 'config.toml')) {
            Write-Step "Your configuration in $varioHome was left unchanged. Next:"
            Write-Step "  vario doctor      # check the configuration; it says if 'vario migrate' is needed"
        } else {
            Write-Step "Next:"
            Write-Step "  vario init        # guided setup: ClickHouse, collection mode, sources"
            Write-Step "  vario doctor      # check everything"
        }
        Write-Step "  vario run         # collect in the foreground (the background service is macOS only)"
        Write-Step ""
        Write-Step "Documentation: https://github.com/nedanwr/vario#readme"
    } finally {
        Remove-Item -LiteralPath $tmp -Recurse -Force -ErrorAction SilentlyContinue
    }
}

try {
    Install-Vario -ReqVersion $Version -ReqChannel $Channel -ReqDir $InstallDir -ReqSource $Source -SkipPath $NoModifyPath.IsPresent -Reinstall $Force.IsPresent
} catch {
    Write-Host "vario-install: error: $($_.Exception.Message)" -ForegroundColor Red
    # `exit` would close the window of someone who ran `irm ... | iex`.
    if ($RunningAsFile) { exit 1 }
    $global:LASTEXITCODE = 1
}
