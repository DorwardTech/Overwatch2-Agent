; Installer for the Overwatch Site Agent.
;
; WINDOWS.md documents doing all of this by hand, and that is still supported.
; This exists because the last step of the hand install is an elevated
; PowerShell prompt, and the person installing at a venue is whoever is on
; shift. Double-clicking one file and answering "yes" to the elevation prompt is
; the whole interaction this replaces it with.
;
; It does not reimplement any of the install. `overwatch-agent install` still
; registers the service, locks down the data directory, writes the starter
; configuration and adds the Start Menu entry, exactly as it does for a hand
; install. This puts the files where the service account can read them, calls
; that, and offers to open the setup page afterwards. One implementation, two
; front doors.
;
; Built by .github/workflows/windows-release.yml on a v* tag, and compiled by
; ci.yml on every change so the script is never first exercised at release time.
; To build it by hand, stage the two release archives and point ISCC at them:
;
;   Expand-Archive overwatch-agent_1.5.0_windows_amd64.zip -DestinationPath stage\amd64
;   Expand-Archive overwatch-agent_1.5.0_windows_arm64.zip -DestinationPath stage\arm64
;
; both run from the repository root, which is what SourceDir below anchors to:
;
;   & "C:\Program Files (x86)\Inno Setup 6\ISCC.exe" `
;       /DAppVersion=1.5.0 `
;       /DAmd64Dir=stage\amd64\overwatch-agent_1.5.0_windows_amd64 `
;       /DArm64Dir=stage\arm64\overwatch-agent_1.5.0_windows_arm64 `
;       packaging\windows\overwatch-agent.iss
;
; Requires Inno Setup 6.3 or later, for the Arm64 architecture identifiers.

#ifndef AppVersion
  #define AppVersion "0.0.0"
#endif
#ifndef Amd64Dir
  #define Amd64Dir "stage\amd64"
#endif
#ifndef Arm64Dir
  #define Arm64Dir "stage\arm64"
#endif
#ifndef OutDir
  #define OutDir "dist"
#endif

[Setup]
; Relative Source and OutputDir paths resolve against SourceDir, and SourceDir
; itself against the directory holding this script — NOT the directory ISCC was
; run from. Without this, staging into <repo>\stage and compiling from <repo>
; sends Inno looking in <repo>\packaging\windows\stage instead, which is
; exactly what the first run of this script did. Anchoring it at the repository
; root makes the relative paths below mean what both workflows, and anyone
; compiling from the root by hand, already assume they mean.
SourceDir=..\..

; Never change AppId. It is what makes the next version an upgrade of this one
; rather than a second copy sitting beside it in Add/Remove Programs.
AppId={{B3F1C7A8-6D42-4E19-9A5C-2F8E0D3B7146}
AppName=Overwatch Site Agent
AppVersion={#AppVersion}
AppVerName=Overwatch Site Agent {#AppVersion}
AppPublisher=DorwardTech
AppPublisherURL=https://github.com/DorwardTech/Overwatch2-Agent
AppSupportURL=https://github.com/DorwardTech/Overwatch2-Agent/issues
VersionInfoVersion={#AppVersion}
VersionInfoDescription=Overwatch Site Agent installer

; Program Files, and only Program Files: the service runs as a limited account
; that cannot read a user's profile, so an agent installed under one starts and
; then cannot read its own executable's folder. A scripted rollout can still
; override this with /DIR="..." if a venue genuinely needs another disk.
DefaultDirName={autopf}\Overwatch Agent
DisableDirPage=yes

; `overwatch-agent install` creates the Start Menu entry itself, marked
; run-as-administrator — the setup page controls the service, so it needs the
; elevation prompt that flag produces, and Inno cannot set it. So Inno does not
; compete: no program group, no second shortcut.
DisableProgramGroupPage=yes

UninstallDisplayName=Overwatch Site Agent
UninstallDisplayIcon={app}\overwatch-agent.exe

; Registering a service and writing under %ProgramData% both need it, and asking
; up front is better than failing halfway through.
PrivilegesRequired=admin
ArchitecturesAllowed=x64compatible or arm64
ArchitecturesInstallIn64BitMode=x64compatible or arm64
MinVersion=10.0

; The service holds the executable open, and PrepareToInstall below stops it
; deliberately. Leaving the Restart Manager to notice as well would only add a
; second, confusing prompt about closing an application nobody opened.
CloseApplications=no

OutputDir={#OutDir}
OutputBaseFilename=overwatch-agent_{#AppVersion}_windows_setup
Compression=lzma2/max
SolidCompression=yes
WizardStyle=modern

[Languages]
Name: "english"; MessagesFile: "compiler:Default.isl"

[Files]
; Both builds are carried; only the one this machine can run is installed, so
; there is no architecture for the operator to get wrong.
Source: "{#Amd64Dir}\overwatch-agent.exe"; DestDir: "{app}"; DestName: "overwatch-agent.exe"; Flags: ignoreversion; Check: not IsArm64
Source: "{#Arm64Dir}\overwatch-agent.exe"; DestDir: "{app}"; DestName: "overwatch-agent.exe"; Flags: ignoreversion; Check: IsArm64
; The rest of the archive is identical between the two, so it comes from one.
Source: "{#Amd64Dir}\agent.env.example"; DestDir: "{app}"; Flags: ignoreversion
Source: "{#Amd64Dir}\WINDOWS.md"; DestDir: "{app}"; Flags: ignoreversion
Source: "{#Amd64Dir}\CHANGELOG.md"; DestDir: "{app}"; Flags: ignoreversion
Source: "{#Amd64Dir}\LICENSE"; DestDir: "{app}"; Flags: ignoreversion

[Run]
; Registering the service and starting it are done from CurStepChanged instead,
; because a [Run] entry cannot fail the install: Inno reports a program it could
; not START, but ignores what that program exits with, and an agent that was
; never registered must not finish on a green page.
;
; This one is the Finished-page checkbox, which runs after all of that.
Filename: "{app}\overwatch-agent.exe"; Parameters: "setup"; Description: "Set this venue up now"; Flags: postinstall nowait skipifsilent; Check: ShouldOfferSetup

[UninstallRun]
; Stops the service, deregisters it, removes the event log source and the Start
; Menu entry. It leaves the settings, cached games and logs alone —
; CurUninstallStepChanged asks about those separately.
Filename: "{app}\overwatch-agent.exe"; Parameters: "uninstall"; Flags: runhidden waituntilterminated; RunOnceId: "RemoveOverwatchAgentService"

[Code]
var
  PreviousInstall: Boolean;
  RegistrationFailed: Boolean;

// The setup page is offered on a first install, where there is nothing in the
// configuration yet. An upgrade already has the venue's settings and should not
// ask for them again, and there is nothing to set up if the service was never
// registered.
function ShouldOfferSetup: Boolean;
begin
  Result := (not PreviousInstall) and (not RegistrationFailed);
end;

// An existing agent has to be deregistered before this one can be written over
// it: the running service holds the executable open, and `install` refuses to
// touch a service that already exists rather than half-reconfigure it.
//
// `uninstall` is the right tool for that and not a destructive one — it stops
// the service and leaves the configuration, cache and logs where they are, so
// the venue's token and settings survive the upgrade and the reinstall picks
// them straight back up.
function PrepareToInstall(var NeedsRestart: Boolean): String;
var
  Exe: String;
  ResultCode: Integer;
begin
  Result := '';
  Exe := ExpandConstant('{app}\overwatch-agent.exe');
  if not FileExists(Exe) then
    Exit;

  PreviousInstall := True;
  // A non-zero exit is not worth stopping for: it is what an executable left
  // behind without its service reports, and the install that follows is the fix
  // for that. Only being unable to run it at all is a problem, because then the
  // service is still holding the file open.
  if not Exec(Exe, 'uninstall', '', SW_HIDE, ewWaitUntilTerminated, ResultCode) then
    Result := 'The existing Overwatch Site Agent could not be stopped, so its files cannot be replaced.' + #13#10#13#10 +
              'Open Services, stop "Overwatch Site Agent", and run this installer again.';
end;

procedure CurStepChanged(CurStep: TSetupStep);
var
  Exe: String;
  ResultCode: Integer;
begin
  if CurStep <> ssPostInstall then
    Exit;

  Exe := ExpandConstant('{app}\overwatch-agent.exe');

  // The step that actually creates the service. If it fails there is no agent
  // registered, however many files were copied, so say so plainly and name the
  // command that will print the reason.
  if (not Exec(Exe, 'install', ExtractFileDir(Exe), SW_HIDE, ewWaitUntilTerminated, ResultCode)) or (ResultCode <> 0) then
  begin
    RegistrationFailed := True;
    MsgBox('The agent was copied to this computer, but registering it as a Windows service failed.' + #13#10#13#10 +
           'Open PowerShell as an administrator and run:' + #13#10#13#10 +
           '    & "' + Exe + '" install' + #13#10#13#10 +
           'It prints what went wrong.', mbError, MB_OK);
    Exit;
  end;

  // An upgrade ends with the agent running, because that is the state it was in
  // before. A first install does not: there is nothing to connect with yet, and
  // the setup page starts it as its last step.
  if PreviousInstall then
    Exec(Exe, 'start', ExtractFileDir(Exe), SW_HIDE, ewWaitUntilTerminated, ResultCode);
end;

procedure CurUninstallStepChanged(CurUninstallStep: TUninstallStep);
var
  DataDir: String;
begin
  if CurUninstallStep <> usPostUninstall then
    Exit;

  DataDir := ExpandConstant('{commonappdata}\Overwatch Agent');
  if not DirExists(DataDir) then
    Exit;

  // A scripted removal keeps the data. Deleting a venue's site token and its
  // cached games is not something to do because nobody was there to answer.
  if UninstallSilent then
    Exit;

  if MsgBox('Also delete this venue''s settings, cached games and logs?' + #13#10#13#10 +
            DataDir + #13#10#13#10 +
            'Choose No if you are reinstalling, upgrading or moving the agent — the site token and the settings are in there.',
            mbConfirmation, MB_YESNO or MB_DEFBUTTON2) = IDYES then
    DelTree(DataDir, True, True, True);
end;
