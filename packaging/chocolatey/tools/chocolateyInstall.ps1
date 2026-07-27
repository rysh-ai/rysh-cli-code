$ErrorActionPreference = 'Stop'

# NOTE: this package ships the CLI-ONLY Windows build. It cannot open rysh
# panes — rysh has no ConPTY implementation, so a windows/amd64 binary links
# and runs but cannot allocate a terminal. The artifact name says so, and the
# post-install message below says so, because a user who installs a package
# called "rysh" on Windows reasonably expects a multiplexer.

$packageName = 'rysh'
$toolsDir    = "$(Split-Path -parent $MyInvocation.MyCommand.Definition)"
$version     = $env:ChocolateyPackageVersion

$packageArgs = @{
  packageName   = $packageName
  unzipLocation = $toolsDir
  fileType      = 'zip'
  # Artifacts are served from packages.rysh.ai, NOT GitHub Releases: the
  # rysh-cli repository is private, so a github.com/... download URL 404s for
  # every user. (The previous URL pointed at github.com/rysh-ai/rysh, which is
  # not even a repository that exists.)
  url          = "https://packages.rysh.ai/releases/v${version}/rysh_windows_amd64_cli-only.zip"
  checksum     = 'PLACEHOLDER_SHA256_WINDOWS_AMD64_CLI_ONLY'
  checksumType = 'sha256'
}

Install-ChocolateyZipPackage @packageArgs

$exePath = Join-Path $toolsDir 'rysh.exe'
if (-not (Test-Path $exePath)) {
  throw "Rysh binary not found after extraction at $exePath"
}

Write-Host "Rysh (CLI-only build) installed at: $exePath"
Write-Host ""
Write-Host "IMPORTANT: this build CANNOT open rysh panes."
Write-Host "Every pane is a PTY-backed shell and rysh has no ConPTY support yet,"
Write-Host "so session commands (rysh, rysh <name>, rysh attach) refuse up front."
Write-Host ""
Write-Host "What works here:"
Write-Host "  rysh send <session> <input>   talk to a session running in WSL"
Write-Host "  rysh install <package>"
Write-Host "  rysh list-sessions / list-packages / eval / doctor / version"
Write-Host ""
Write-Host "For the actual multiplexer, run rysh inside WSL2:"
Write-Host "  wsl --install"
Write-Host "  wsl"
Write-Host "  curl -fsSL https://rysh.ai/install.sh | sh"
