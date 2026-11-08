#Requires -Version 5.1
<#
.SYNOPSIS
Build the Praxis-RAG Go backend and Console into dist/, then smoke-test the
service locally.

.DESCRIPTION
Steps:
  1. Frontend: npm ci (when node_modules is missing or -FreshFrontendDeps) and
     npm run build (vite writes to src/rag_mcp/web), copied to dist/web.
  2. Go: every cmd/*/main.go entrypoint is built into dist/bin.
  3. Smoke: rag-service starts on a free loopback port with a temporary config
     and SQLite state, then the script polls /health and checks that /console
     and / serve the built Console index. The process is always stopped and the
     temporary directory removed.
  4. Optional -Test runs `go test ./...`.

The script is idempotent: dist/bin and dist/web are rebuilt from scratch, and
package installation is skipped unless needed. It fails fast on any tool,
build, or smoke error.

.EXAMPLE
pwsh -File scripts/build-go.ps1
pwsh -File scripts/build-go.ps1 -SkipFrontend          # reuse dist/web
pwsh -File scripts/build-go.ps1 -Test -SmokeTimeoutSeconds 60
pwsh -File scripts/build-go.ps1 -SmokeOnly             # re-run smoke only
#>
[CmdletBinding()]
param(
    # Do not rebuild the Console; requires an existing dist/web/index.html.
    [switch]$SkipFrontend,
    # Do not rebuild Go binaries; requires existing dist/bin binaries.
    [switch]$SkipGo,
    # Only run the smoke check against the existing dist artifacts.
    [switch]$SmokeOnly,
    # Run `go test ./...` after building.
    [switch]$Test,
    # Force a fresh `npm ci` even when frontend/node_modules exists.
    [switch]$FreshFrontendDeps,
    # Seconds to wait for /health before failing the smoke check.
    [int]$SmokeTimeoutSeconds = 30
)

Set-StrictMode -Version Latest
$ErrorActionPreference = "Stop"

$root = Split-Path -Parent $PSScriptRoot
$dist = Join-Path $root "dist"
$binDir = Join-Path $dist "bin"
$webDir = Join-Path $dist "web"
$exeSuffix = if ($env:OS -eq "Windows_NT") { ".exe" } else { "" }
$serviceExe = Join-Path $binDir ("rag-service" + $exeSuffix)
$frontendDir = Join-Path $root "frontend"
$webBuildDir = Join-Path $root "src\rag_mcp\web"

Push-Location $root
try {
    function Assert-Tool {
        param([string]$Name, [string]$Hint)
        if (-not (Get-Command $Name -ErrorAction SilentlyContinue)) {
            throw "Required tool '$Name' is not on PATH. $Hint"
        }
    }

    function Invoke-Native {
        param([string]$File, [string[]]$Arguments, [string]$Description)
        Write-Host "[run] $Description"
        & $File @Arguments
        if ($LASTEXITCODE -ne 0) {
            throw "$Description failed (exit $LASTEXITCODE)"
        }
    }

    function Build-Frontend {
        Assert-Tool "npm" "Install Node.js (>=20.19) and rerun."
        Push-Location $frontendDir
        try {
            if ($FreshFrontendDeps -or -not (Test-Path (Join-Path $frontendDir "node_modules"))) {
                Invoke-Native "npm" @("ci", "--no-audit", "--no-fund") "npm ci (frontend)"
            } else {
                Write-Host "[skip] frontend/node_modules already present (use -FreshFrontendDeps to reinstall)"
            }
            Invoke-Native "npm" @("run", "build") "npm run build (vite -> src/rag_mcp/web)"
        } finally {
            Pop-Location
        }
        $index = Join-Path $webBuildDir "index.html"
        if (-not (Test-Path $index)) {
            throw "Frontend build did not produce $index"
        }
        if (Test-Path $webDir) {
            Remove-Item $webDir -Recurse -Force
        }
        New-Item -ItemType Directory -Force -Path $webDir | Out-Null
        Copy-Item -Path (Join-Path $webBuildDir "*") -Destination $webDir -Recurse -Force
        $files = @(Get-ChildItem $webDir -Recurse -File)
        Write-Host "[ok] frontend -> $webDir ($($files.Count) files)"
    }

    function Build-Go {
        Assert-Tool "go" "Install Go 1.27+ and rerun."
        if (Test-Path $binDir) {
            Remove-Item $binDir -Recurse -Force
        }
        New-Item -ItemType Directory -Force -Path $binDir | Out-Null
        $commands = @(Get-ChildItem (Join-Path $root "cmd") -Directory |
            Where-Object { Test-Path (Join-Path $_.FullName "main.go") } |
            Sort-Object Name)
        if ($commands.Count -eq 0) {
            throw "No cmd/*/main.go entrypoints found"
        }
        if (-not ($commands.Name -contains "rag-service")) {
            throw "cmd/rag-service/main.go is missing"
        }
        foreach ($command in $commands) {
            $target = Join-Path $binDir ($command.Name + $exeSuffix)
            Invoke-Native "go" @(
                "build", "-trimpath", "-buildvcs=false",
                "-ldflags", "-s -w",
                "-o", $target,
                "./cmd/$($command.Name)"
            ) "go build ./cmd/$($command.Name)"
            if (-not (Test-Path $target)) {
                throw "go build did not produce $target"
            }
            Write-Host "[ok] go -> $target"
        }
    }

    function Invoke-GoTests {
        Assert-Tool "go" "Install Go 1.27+ and rerun."
        Invoke-Native "go" @("test", "./...") "go test ./..."
    }

    function Get-FreeLoopbackPort {
        $listener = [System.Net.Sockets.TcpListener]::new([System.Net.IPAddress]::Loopback, 0)
        $listener.Start()
        try {
            return ([System.Net.IPEndPoint]$listener.LocalEndpoint).Port
        } finally {
            $listener.Stop()
        }
    }

    function Set-ProcessEnvironment {
        param([string]$Name, [string]$Value)
        # Remove-Item is required: [Environment]::SetEnvironmentVariable($null,
        # "Process") leaves an empty-but-present variable on Windows, which the
        # service would treat as "set to empty" and reject for allowlists.
        if ([string]::IsNullOrEmpty($Value)) {
            Remove-Item -Path ("Env:" + $Name) -ErrorAction SilentlyContinue
        } else {
            Set-Item -Path ("Env:" + $Name) -Value $Value
        }
    }

    function Invoke-Smoke {
        if (-not (Test-Path $serviceExe)) {
            throw "Missing $serviceExe; run the build without -SkipGo first"
        }
        if (-not (Test-Path (Join-Path $webDir "index.html"))) {
            throw "Missing $(Join-Path $webDir 'index.html'); run the build without -SkipFrontend first"
        }
        $port = Get-FreeLoopbackPort
        # Keep the smoke workspace inside the repository: the Windows agent
        # sandbox lets the spawned service write here, while %TEMP% writes from a
        # child process can fail with SQLite "unable to open database file".
        $smokeRoot = Join-Path $root ".test-tmp"
        New-Item -ItemType Directory -Force -Path $smokeRoot | Out-Null
        $tempDir = Join-Path $smokeRoot ("smoke-" + [guid]::NewGuid().ToString("N"))
        New-Item -ItemType Directory -Force -Path $tempDir | Out-Null
        $configFile = Join-Path $tempDir "config.toml"
        $dataRoot = Join-Path $tempDir "data"
        $stdout = Join-Path $tempDir "service.log"
        $stderr = Join-Path $tempDir "service.err.log"
        # A minimal config file avoids touching the user's ~/.praxis-rag/config.toml.
        # Write it without a BOM so the TOML parser sees plain UTF-8.
        [System.IO.File]::WriteAllText($configFile, "[environment]`n", (New-Object System.Text.UTF8Encoding($false)))
        New-Item -ItemType Directory -Force -Path $dataRoot | Out-Null

        $environmentNames = @(
            "RAG_CONFIG_FILE", "RAG_BACKEND", "RAG_SERVICE_HOST", "RAG_SERVICE_PORT",
            "RAG_SERVICE_DATA_ROOT", "RAG_SERVICE_TOKEN", "RAG_MCP_ALLOWED_HOSTS",
            "RAG_MCP_ALLOWED_ORIGINS", "RAG_WEB_ROOT", "RAG_STATE_URL",
            "RAG_MYSQL_HOST", "RAG_MYSQL_PORT", "RAG_MYSQL_USER", "RAG_MYSQL_DATABASE",
            "RAG_STATE_ENCRYPTION_KEY", "QDRANT_URL", "QDRANT_PATH"
        )
        $saved = @{}
        foreach ($name in $environmentNames) {
            $saved[$name] = [Environment]::GetEnvironmentVariable($name, "Process")
        }
        $process = $null
        try {
            # Isolate the smoke run from any developer environment/state database.
            foreach ($name in $environmentNames) {
                Set-ProcessEnvironment $name ""
            }
            Set-ProcessEnvironment "RAG_CONFIG_FILE" $configFile
            Set-ProcessEnvironment "RAG_BACKEND" "qdrant"
            Set-ProcessEnvironment "RAG_SERVICE_HOST" "127.0.0.1"
            Set-ProcessEnvironment "RAG_SERVICE_PORT" ([string]$port)
            Set-ProcessEnvironment "RAG_SERVICE_DATA_ROOT" $dataRoot
            Set-ProcessEnvironment "RAG_WEB_ROOT" $webDir
            Set-ProcessEnvironment "QDRANT_URL" "http://127.0.0.1:6333"

            Write-Host "[run] smoke: $serviceExe on http://127.0.0.1:$port"
            $process = Start-Process -FilePath $serviceExe -PassThru -NoNewWindow `
                -RedirectStandardOutput $stdout -RedirectStandardError $stderr

            $health = $null
            $deadline = (Get-Date).AddSeconds($SmokeTimeoutSeconds)
            while ((Get-Date) -lt $deadline) {
                if ($process.HasExited) {
                    break
                }
                try {
                    $response = Invoke-WebRequest -UseBasicParsing -Uri "http://127.0.0.1:$port/health" -TimeoutSec 2
                    if ($response.StatusCode -eq 200) {
                        $candidate = $response.Content | ConvertFrom-Json
                        if ($candidate.status -eq "ok") {
                            $health = $candidate
                            break
                        }
                    }
                } catch {
                    # The listener is not up yet; keep polling until the deadline.
                }
                Start-Sleep -Milliseconds 200
            }
            if ($null -eq $health) {
                $detail = ""
                foreach ($log in @($stderr, $stdout)) {
                    if (Test-Path $log) {
                        $tail = @(Get-Content $log -Tail 20 -ErrorAction SilentlyContinue)
                        if ($tail.Count -gt 0) {
                            $detail = $detail + "`n" + ($tail -join "`n")
                        }
                    }
                }
                throw "Smoke check failed: /health did not report status=ok on port $port.$detail"
            }

            $console = Invoke-WebRequest -UseBasicParsing -Uri "http://127.0.0.1:$port/console" -TimeoutSec 5
            if ($console.StatusCode -ne 200) {
                throw "Smoke check failed: GET /console returned $($console.StatusCode)"
            }
            if ($console.Content -notmatch 'id="root"') {
                throw "Smoke check failed: /console did not serve the Console index.html"
            }
            if ($console.Content -notmatch "/console/assets/") {
                throw "Smoke check failed: /console index does not reference built assets (was dist/web built?)"
            }
            $rootPage = Invoke-WebRequest -UseBasicParsing -Uri "http://127.0.0.1:$port/" -TimeoutSec 5
            if ($rootPage.StatusCode -ne 200) {
                throw "Smoke check failed: GET / returned $($rootPage.StatusCode)"
            }

            $state = ""
            if ($null -ne $health.PSObject.Properties["state"]) {
                $state = " backend=$($health.backend) state=$($health.state)"
            }
            Write-Host "[ok] smoke: /health 200 status=ok; /console serves index.html; / 200$state"
        } finally {
            if ($null -ne $process -and -not $process.HasExited) {
                try {
                    Stop-Process -Id $process.Id -Force -ErrorAction SilentlyContinue
                    $null = $process.WaitForExit(5000)
                } catch {
                    # Best-effort cleanup; the temporary directory is removed below.
                }
            }
            foreach ($name in $environmentNames) {
                Set-ProcessEnvironment $name ([string]$saved[$name])
            }
            Remove-Item $tempDir -Recurse -Force -ErrorAction SilentlyContinue
        }
    }

    Write-Host "Praxis-RAG build (root: $root)"
    if (-not $SmokeOnly) {
        if (-not $SkipFrontend) {
            Build-Frontend
        } elseif (-not (Test-Path (Join-Path $webDir "index.html"))) {
            throw "-SkipFrontend requires an existing $(Join-Path $webDir 'index.html')"
        }
        if (-not $SkipGo) {
            Build-Go
        } elseif (-not (Test-Path $serviceExe)) {
            throw "-SkipGo requires an existing $serviceExe"
        }
        if ($Test) {
            Invoke-GoTests
        }
        Invoke-Smoke
    } else {
        Invoke-Smoke
    }

    Write-Host ""
    Write-Host "Artifacts:"
    Get-ChildItem $binDir -File | Sort-Object Name | ForEach-Object {
        Write-Host ("  bin/{0}  {1:n0} bytes" -f $_.Name, $_.Length)
    }
    $webFiles = @(Get-ChildItem $webDir -Recurse -File)
    $webBytes = ($webFiles | Measure-Object -Property Length -Sum).Sum
    Write-Host ("  web/  {0:n0} files, {1:n0} bytes" -f $webFiles.Count, $webBytes)
}
finally {
    Pop-Location
}
