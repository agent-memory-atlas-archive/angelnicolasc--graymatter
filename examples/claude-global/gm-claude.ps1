#requires -Version 7.3
$ClaudeArgs = @($args)
$ErrorActionPreference = 'Stop'
$PSNativeCommandArgumentPassing = 'Standard'
$PSNativeCommandUseErrorActionPreference = $false

$gitExe = (Get-Command git.exe -CommandType Application -ErrorAction Stop | Select-Object -First 1).Source
$graymatterExe = (Get-Command graymatter.exe -CommandType Application -ErrorAction Stop | Select-Object -First 1).Source
$claudeExe = (Get-Command claude.exe -CommandType Application -ErrorAction Stop | Select-Object -First 1).Source

$projectRoot = & $gitExe rev-parse --show-toplevel 2>$null
if ($LASTEXITCODE -ne 0) {
    [Console]::Error.WriteLine('Run gm-claude inside a Git checkout or worktree.')
    exit 2
}

$sessionExit = 1
Push-Location -LiteralPath ([string]$projectRoot)
try {
    & $graymatterExe init --store-only --quiet
    $prepareExit = $LASTEXITCODE
    if ($prepareExit -ne 0) {
        [Console]::Error.WriteLine("GrayMatter store preparation failed (exit $prepareExit).")
        $sessionExit = $prepareExit
    }
    else {
        & $claudeExe @ClaudeArgs
        $sessionExit = $LASTEXITCODE
    }
}
finally {
    Pop-Location
}
exit $sessionExit
