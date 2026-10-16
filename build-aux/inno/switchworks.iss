; Switchworks — Inno Setup installer script
;
; A per-user install: no administrator rights, nothing for other users.
; It installs into %LOCALAPPDATA%\Programs\Switchworks:
;   swk.exe       the command line (added to the user's PATH, optional)
;   swkw.exe      the same program built without a console window, which
;                  the node's logon task runs
;   swk-tray.exe  the notification-area icon (started at sign-in, optional)
; It does not register the node: after enrolling, `swk service install`
; (or "Run node at login" in the tray menu) adds a Task Scheduler logon task
; that runs as the user, with least privilege. Never a Windows service.
;
; Preprocessor defines (passed via /D on the iscc command line):
;   AppVersion        — the release version, e.g. "0.2.0" or "0.2.0-rc.1"
;   AppNumericVersion — its four-part numeric form, e.g. "0.2.0.0"
;   SourceDir         — folder with swk.exe, swkw.exe and swk-tray.exe
;   OutputDir         — where to write the installer

#ifndef AppVersion
  #define AppVersion "0.1.0"
#endif
#ifndef AppNumericVersion
  #define AppNumericVersion "0.1.0.0"
#endif
#ifndef SourceDir
  #define SourceDir "..\..\dist\windows-amd64"
#endif
#ifndef OutputDir
  #define OutputDir "..\..\dist"
#endif

[Setup]
AppName=Switchworks
AppVersion={#AppVersion}
AppVerName=Switchworks {#AppVersion}
; Deterministic application GUID: UUID version 5 of the URL namespace and
; https://github.com/jm2/switchworks/io.github.jm2.Switchworks. It must never
; change and must never reuse another application's GUID.
AppId={{0405C893-111E-5752-8C84-4CF4FD2637A3}
VersionInfoVersion={#AppNumericVersion}
VersionInfoProductVersion={#AppNumericVersion}
VersionInfoTextVersion={#AppVersion}
VersionInfoProductTextVersion={#AppVersion}
VersionInfoProductName=Switchworks
VersionInfoDescription=Switchworks Setup
VersionInfoCopyright=Copyright (C) 2026 Switchworks Contributors
AppPublisher=Switchworks Contributors
AppPublisherURL=https://github.com/jm2/switchworks
AppSupportURL=https://github.com/jm2/switchworks/issues
AppUpdatesURL=https://github.com/jm2/switchworks/releases
; Per-user: %LOCALAPPDATA%\Programs\Switchworks, HKCU only, never elevated.
PrivilegesRequired=lowest
DefaultDirName={userpf}\Switchworks
; Always the per-user folder: a folder chosen elsewhere (such as C:\Switchworks)
; can be writable by other users, who could then replace what the logon task
; runs.
DisableDirPage=yes
; Not even an earlier install's folder or /DIR= (PrepareToInstall checks).
UsePreviousAppDir=no
DefaultGroupName=Switchworks
DisableProgramGroupPage=yes
UninstallDisplayIcon={app}\swk-tray.exe
UninstallDisplayName=Switchworks
OutputDir={#OutputDir}
OutputBaseFilename=switchworks-windows-x86_64-setup
Compression=lzma2/ultra64
SolidCompression=yes
SetupIconFile=..\..\data\switchworks.ico
LicenseFile=..\..\LICENSE
WizardStyle=modern
ChangesEnvironment=yes
; Silent install support (winget passes /VERYSILENT /SUPPRESSMSGBOXES /NORESTART /SP-)
CloseApplications=yes
CloseApplicationsFilter=swk.exe,swkw.exe,swk-tray.exe
RestartApplications=no
SetupLogging=yes
ArchitecturesAllowed=x64compatible
ArchitecturesInstallIn64BitMode=x64compatible

[Languages]
Name: "english"; MessagesFile: "compiler:Default.isl"

[Tasks]
Name: "addtopath"; Description: "Add swk to my PATH"
Name: "trayatlogin"; Description: "Start the Switchworks tray icon when I sign in"

[Files]
Source: "{#SourceDir}\swk.exe"; DestDir: "{app}"; Flags: ignoreversion
Source: "{#SourceDir}\swkw.exe"; DestDir: "{app}"; Flags: ignoreversion
Source: "{#SourceDir}\swk-tray.exe"; DestDir: "{app}"; Flags: ignoreversion
Source: "..\..\LICENSE"; DestDir: "{app}"; DestName: "LICENSE.txt"; Flags: ignoreversion
Source: "..\..\internal\web\assets\fonts\OFL-Onest.txt"; DestDir: "{app}\licenses"; Flags: ignoreversion
Source: "..\..\internal\web\assets\fonts\OFL-JetBrainsMono.txt"; DestDir: "{app}\licenses"; Flags: ignoreversion
Source: "..\..\README.md"; DestDir: "{app}"; Flags: ignoreversion

[Icons]
Name: "{group}\Switchworks"; Filename: "{app}\swk-tray.exe"; Comment: "Switchworks status in the notification area"
Name: "{group}\Uninstall Switchworks"; Filename: "{uninstallexe}"

[Registry]
; The same value the tray's "Start this tray at login" toggles.
Root: HKCU; Subkey: "Software\Microsoft\Windows\CurrentVersion\Run"; ValueType: string; ValueName: "Switchworks"; ValueData: """{app}\swk-tray.exe"""; Tasks: trayatlogin; Flags: uninsdeletevalue
Root: HKCU; Subkey: "Environment"; ValueType: expandsz; ValueName: "Path"; ValueData: "{olddata};{app}"; Tasks: addtopath; Check: NeedsAddPath(ExpandConstant('{app}'))

[Run]
Filename: "{app}\swk-tray.exe"; Description: "Start the Switchworks tray icon"; Flags: nowait postinstall skipifsilent

[UninstallRun]
; Stop the node and coordinator and remove their logon tasks; their
; configuration and data under %AppData%\swk are kept.
Filename: "{app}\swk.exe"; Parameters: "service uninstall node"; Flags: runhidden waituntilterminated; RunOnceId: "RemoveNodeTask"
Filename: "{app}\swk.exe"; Parameters: "service uninstall coordinator"; Flags: runhidden waituntilterminated; RunOnceId: "RemoveCoordinatorTask"
Filename: "{sys}\taskkill.exe"; Parameters: "/F /IM swk-tray.exe"; Flags: runhidden waituntilterminated; RunOnceId: "StopTray"

[Code]
const
  EnvKey = 'Environment';
  RunKey = 'Software\Microsoft\Windows\CurrentVersion\Run';

function NeedsAddPath(Dir: string): Boolean;
var
  Paths: string;
begin
  if not RegQueryStringValue(HKCU, EnvKey, 'Path', Paths) then
  begin
    Result := True;
    exit;
  end;
  Result := Pos(';' + Uppercase(Dir) + ';', ';' + Uppercase(Paths) + ';') = 0;
end;

procedure RemovePath(Dir: string);
var
  Paths: string;
  P: Integer;
begin
  if not RegQueryStringValue(HKCU, EnvKey, 'Path', Paths) then
    exit;
  P := Pos(';' + Uppercase(Dir) + ';', ';' + Uppercase(Paths) + ';');
  if P = 0 then
    exit;
  { Dir starts at P in Paths; drop it with one neighbouring separator. }
  if P > 1 then
    Delete(Paths, P - 1, Length(Dir) + 1)
  else
    Delete(Paths, P, Length(Dir) + 1);
  RegWriteExpandStringValue(HKCU, EnvKey, 'Path', Paths);
end;

procedure RunSwk(Params: string);
var
  Code: Integer;
begin
  if FileExists(ExpandConstant('{app}\swk.exe')) then
    Exec(ExpandConstant('{app}\swk.exe'), Params, '', SW_HIDE, ewWaitUntilTerminated, Code);
end;

{ Switchworks runs as you. Setup started from an elevated prompt (winget in an
  administrator terminal, say) would run its steps and start the tray
  elevated, so it refuses. }
function InitializeSetup: Boolean;
begin
  Result := True;
  { /ALLOWELEVATED=1 is for CI runners, which are always elevated. }
  if IsAdmin and (ExpandConstant('{param:ALLOWELEVATED|0}') <> '1') then
  begin
    SuppressibleMsgBox('Switchworks installs for your own account and must not run elevated.' + #13#10 +
      'Run setup again from a normal, non-administrator prompt.', mbError, MB_OK, IDOK);
    Result := False;
  end;
end;

var
  ServicesStopped: Boolean;

procedure StartServices;
begin
  RunSwk('service start --if-installed node');
  RunSwk('service start --if-installed coordinator');
  ServicesStopped := False;
end;

{ On an upgrade, stop what runs from the old files so they can be replaced,
  and start the installed services again afterwards, whether the upgrade
  finishes or not. }
function PrepareToInstall(var NeedsRestart: Boolean): String;
var
  Code: Integer;
begin
  if CompareText(RemoveBackslashUnlessRoot(ExpandConstant('{app}')),
                 ExpandConstant('{userpf}\Switchworks')) <> 0 then
  begin
    Result := 'Switchworks installs only into ' + ExpandConstant('{userpf}\Switchworks') + '.';
    exit;
  end;
  if FileExists(ExpandConstant('{app}\swk.exe')) then
  begin
    RunSwk('service stop --if-installed node');
    RunSwk('service stop --if-installed coordinator');
    ServicesStopped := True;
  end;
  Exec(ExpandConstant('{sys}\taskkill.exe'), '/F /IM swk-tray.exe', '', SW_HIDE, ewWaitUntilTerminated, Code);
  Result := '';
end;

procedure CurStepChanged(CurStep: TSetupStep);
begin
  if (CurStep = ssPostInstall) and ServicesStopped then
    StartServices;
end;

{ Runs when setup exits, also after a failed or cancelled upgrade. }
procedure DeinitializeSetup;
begin
  if ServicesStopped then
    StartServices;
end;

procedure CurUninstallStepChanged(CurUninstallStep: TUninstallStep);
begin
  if CurUninstallStep = usPostUninstall then
  begin
    RemovePath(ExpandConstant('{app}'));
    { Also when the tray, rather than setup, added it. }
    RegDeleteValue(HKCU, RunKey, 'Switchworks');
  end;
end;
