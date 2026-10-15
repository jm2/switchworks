# Registers `swyd node run` as a logon task for the current user on Windows.
# It must run in your user session (not as a service account) so it inherits
# your Codex sign-in, the OS keyring agy needs, your repos and toolchains.
# Not yet tested on real Windows hardware; report problems.
#
#   powershell -ExecutionPolicy Bypass -File install-node-task.ps1 -Swyd C:\tools\swyd.exe

param(
  [Parameter(Mandatory = $true)][string]$Swyd
)

$action = New-ScheduledTaskAction -Execute $Swyd -Argument "node run"
$trigger = New-ScheduledTaskTrigger -AtLogOn -User $env:USERNAME
$settings = New-ScheduledTaskSettingsSet -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries `
  -RestartCount 999 -RestartInterval (New-TimeSpan -Minutes 1) -ExecutionTimeLimit (New-TimeSpan -Days 0)
$principal = New-ScheduledTaskPrincipal -UserId $env:USERNAME -LogonType Interactive -RunLevel Limited

Register-ScheduledTask -TaskName "Switchyard node" -Action $action -Trigger $trigger `
  -Settings $settings -Principal $principal -Force
Start-ScheduledTask -TaskName "Switchyard node"
Write-Host "Registered and started 'Switchyard node'."
