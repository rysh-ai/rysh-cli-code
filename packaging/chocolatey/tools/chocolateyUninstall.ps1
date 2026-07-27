$ErrorActionPreference = 'Stop'

$packageName = 'rysh'
$toolsDir    = "$(Split-Path -parent $MyInvocation.MyCommand.Definition)"

# Remove rysh binary
$exePath = Join-Path $toolsDir 'rysh.exe'
if (Test-Path $exePath) {
  Remove-Item $exePath -Force
  Write-Host "Rysh binary removed."
}

Write-Host ""
Write-Host "Rysh has been uninstalled."
Write-Host "Your session data (~\.local\state\rysh) has been preserved."
Write-Host "To remove session data: rmdir /s /q %USERPROFILE%\.local\state\rysh"
