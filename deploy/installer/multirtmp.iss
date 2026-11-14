; multirtmp installer (Inno Setup 6). Build with build-installer.ps1 in the
; repository root — it builds multirtmp.exe, checks mediamtx.exe and passes
; the defines below to ISCC.
;
; Exit codes, on top of Inno Setup's own (see "Setup Exit Codes" in its help):
;   10 — a stream is live, nothing was changed (silent mode; run with /FORCE
;        to update anyway);
;   11 — files were installed, but "multirtmp.exe install" or "start"
;        failed (details in the setup log, /LOG).

#ifndef AppVersion
  #error AppVersion is not defined: build with build-installer.ps1
#endif
#ifndef FileVersion
  #error FileVersion is not defined: build with build-installer.ps1
#endif
#ifndef NumericVersion
  #error NumericVersion is not defined: build with build-installer.ps1
#endif
#ifndef SourceRoot
  #error SourceRoot is not defined: build with build-installer.ps1
#endif
#ifndef MediaMTXDir
  #error MediaMTXDir is not defined: build with build-installer.ps1
#endif
#ifndef OutputDir
  #define OutputDir SourceRoot + "\dist"
#endif

[Setup]
; Fixed AppId: every version of the installer recognizes the previous one.
AppId={{8F3C2A51-4E2B-4C8E-9B7A-5D1E6F2A9C30}
AppName=multirtmp
AppVersion={#AppVersion}
AppVerName=multirtmp {#AppVersion}
VersionInfoVersion={#NumericVersion}
VersionInfoProductName=multirtmp
DefaultDirName=C:\multirtmp
DisableDirPage=yes
UsePreviousAppDir=no
DisableProgramGroupPage=yes
PrivilegesRequired=admin
ArchitecturesAllowed=x64compatible
ArchitecturesInstallIn64BitMode=x64compatible
OutputDir={#OutputDir}
OutputBaseFilename=multirtmp-setup-{#FileVersion}
Compression=lzma2
SolidCompression=yes
WizardStyle=modern
SetupLogging=yes
; Files in use are handled in PrepareToInstall (service stopped, MediaMTX
; killed) — Restart Manager would only ask confusing questions.
CloseApplications=no
RestartApplications=no
UninstallDisplayName=multirtmp
UninstallDisplayIcon={app}\multirtmp.exe

[Languages]
Name: "english"; MessagesFile: "compiler:Default.isl"

[Files]
; config.json is deliberately NOT here: the installer never creates,
; overwrites or deletes it ("multirtmp.exe install" creates it only if it
; does not exist).
Source: "{#SourceRoot}\bin\multirtmp.exe"; DestDir: "{app}"; Flags: ignoreversion
Source: "{#SourceRoot}\README.md"; DestDir: "{app}"; Flags: ignoreversion
Source: "{#MediaMTXDir}\mediamtx.exe"; DestDir: "{app}\mediamtx"; Flags: ignoreversion
Source: "{#MediaMTXDir}\LICENSE"; DestDir: "{app}\mediamtx"; Flags: ignoreversion
; Always overwritten: it is made for this exact mediamtx.exe version, and
; MediaMTX refuses to start with settings it does not know.
Source: "{#SourceRoot}\deploy\mediamtx\mediamtx.yml"; DestDir: "{app}\mediamtx"; Flags: ignoreversion

[Run]
; Not elevated: the window opens as the user who started the installer.
Filename: "{app}\multirtmp.exe"; Parameters: "open"; Description: "Open multirtmp"; Flags: postinstall nowait skipifsilent runasoriginaluser

[UninstallRun]
; Stops the service (MediaMTX goes with it), removes it, the desktop icon and
; the firewall rules. config.json and the logs stay: keys and history.
Filename: "{app}\multirtmp.exe"; Parameters: "uninstall"; Flags: runhidden waituntilterminated; RunOnceId: "RemoveService"

[UninstallDelete]
Type: filesandordirs; Name: "{app}\backup"

[Code]
const
  ServiceKey = 'SYSTEM\CurrentControlSet\Services\multirtmp';
  ExitStreamLive = 10;
  ExitServiceFailed = 11;

var
  OldExe: String;          // exe the registered service points to ('' — no service)
  OldConfigArg: String;    // its -config value ('' — config.json next to the exe)
  ServiceStepFailed: Boolean;

procedure ExitProcess(uExitCode: Cardinal); external 'ExitProcess@kernel32.dll stdcall';

function HasParam(const Name: String): Boolean;
var
  I: Integer;
begin
  Result := False;
  for I := 1 to ParamCount do
    if CompareText(ParamStr(I), Name) = 0 then
      Result := True;
end;

#include "parse.iss"

// Reads the registered multirtmp service: path to its exe and its -config
// argument, if any. v1.0.0 sites have it in C:\multi-rtmp\bin, possibly
// elsewhere — the service is the only reliable pointer.
procedure ReadOldService;
var
  ImagePath: String;
begin
  OldExe := '';
  OldConfigArg := '';
  if not RegQueryStringValue(HKLM, ServiceKey, 'ImagePath', ImagePath) then
    Exit;
  if not ParseImagePath(ImagePath, OldExe, OldConfigArg) then
  begin
    Log('Service ImagePath not understood: ' + ImagePath);
    Exit;
  end;
  Log('Registered service: exe=' + OldExe + ' config=' + OldConfigArg);
end;

function OldConfigPath: String;
begin
  if OldConfigArg <> '' then
    Result := OldConfigArg
  else if OldExe <> '' then
    Result := AddBackslash(ExtractFileDir(OldExe)) + 'config.json'
  else
    Result := '';
end;

// Is a source connected right now? Port and token from the api section of
// the running multirtmp's config (see ApiEndpoint, QueryLive).
function StreamIsLive(const CfgPath: String): Boolean;
var
  Cfg: AnsiString;
  Port, Token: String;
begin
  Port := '8080';
  Token := '';
  if (CfgPath <> '') and LoadStringFromFile(CfgPath, Cfg) then
    ApiEndpoint(String(Cfg), Port, Token);
  Result := QueryLive(Port, Token);
end;

// Runs multirtmp.exe (or another exe) with Args, writes its output into the
// setup log, returns its exit code (-1 if it could not be started).
function RunLogged(const Exe, Args: String; var Output: String): Integer;
var
  OutFile: String;
  Lines: TArrayOfString;
  I, Code: Integer;
begin
  OutFile := ExpandConstant('{tmp}\run-output.txt');
  DeleteFile(OutFile);
  Output := '';
  if not Exec(ExpandConstant('{cmd}'), '/C ""' + Exe + '" ' + Args + ' > "' + OutFile + '" 2>&1"',
              ExtractFileDir(Exe), SW_HIDE, ewWaitUntilTerminated, Code) then
    Code := -1;
  if LoadStringsFromFile(OutFile, Lines) then
    for I := 0 to GetArrayLength(Lines) - 1 do
    begin
      Log('  ' + Lines[I]);
      Output := Output + Lines[I] + #13#10;
    end;
  Log(ExtractFileName(Exe) + ' ' + Args + ' -> exit code ' + IntToStr(Code));
  Result := Code;
end;

// Copy of the previous multirtmp.exe into {app}\backup\<its version>\ for
// a manual rollback.
procedure BackupPreviousExe;
var
  Prev, Output, Version, Dir: String;
  P, I: Integer;
begin
  Prev := '';
  if (OldExe <> '') and FileExists(OldExe) then
    Prev := OldExe
  else if FileExists(ExpandConstant('{app}\multirtmp.exe')) then
    Prev := ExpandConstant('{app}\multirtmp.exe');
  if Prev = '' then
    Exit;

  // "multirtmp v1.0.0 (a032f25)" -> "v1.0.0"
  RunLogged(Prev, '-version', Output);
  Version := Trim(Output);
  P := Pos(' ', Version);
  if P > 0 then
    Version := Copy(Version, P + 1, MaxInt);
  P := Pos(' ', Version);
  if P > 0 then
    Version := Copy(Version, 1, P - 1);
  for I := 1 to Length(Version) do
    if Pos(Version[I], '\/:*?"<>|') > 0 then
      Version[I] := '_';
  if Version = '' then
    Version := 'unknown-' + GetDateTimeString('yyyymmdd-hhnnss', '-', '-');

  Dir := ExpandConstant('{app}\backup\') + Version;
  if ForceDirectories(Dir) and FileCopy(Prev, Dir + '\multirtmp.exe', False) then
    Log('Previous multirtmp.exe backed up to ' + Dir)
  else
    Log('Warning: could not back up ' + Prev + ' to ' + Dir);
end;

function PrepareToInstall(var NeedsRestart: Boolean): String;
var
  ServiceExists: Boolean;
  Output, Src, Dst, CheckCfg: String;
  Code, I: Integer;
begin
  Result := '';
  ReadOldService;
  ServiceExists := RegKeyExists(HKLM, ServiceKey);

  // 1. Never interrupt a live stream without asking.
  if ServiceExists then
  begin
    CheckCfg := OldConfigPath;
    if (CheckCfg = '') or not FileExists(CheckCfg) then
      CheckCfg := ExpandConstant('{app}\config.json');
    if StreamIsLive(CheckCfg) then
    begin
      if WizardSilent then
      begin
        if HasParam('/FORCE') then
          Log('A stream is live; /FORCE given — updating anyway.')
        else
        begin
          Log('A stream is live: nothing changed. Run again when the stream is over, or with /FORCE.');
          DelTree(ExpandConstant('{tmp}'), True, True, True);
          ExitProcess(ExitStreamLive);
        end;
      end
      else if MsgBox('A stream is live. Updating will interrupt it for 10-20 seconds.' + #13#10#13#10 +
                     'Update now?', mbConfirmation, MB_YESNO or MB_DEFBUTTON2) <> IDYES then
      begin
        Result := 'Update cancelled: a stream is live. Run the installer again when the stream is over.';
        Exit;
      end;
    end;
  end;

  // 2. Keep the previous version for a manual rollback.
  BackupPreviousExe;

  // 3. Stop and remove the service — the new multirtmp.exe from the
  //    installer's temp folder does it for any old version (by service
  //    name), waiting until the process has exited and released its files.
  if ServiceExists then
  begin
    ExtractTemporaryFile('multirtmp.exe');
    Code := RunLogged(ExpandConstant('{tmp}\multirtmp.exe'), 'uninstall', Output);
    if Code <> 0 then
    begin
      Result := 'Could not stop and remove the multirtmp service (exit code ' + IntToStr(Code) + '):' + #13#10 + Output;
      Exit;
    end;
    // The service can stay "marked for deletion" while something holds a
    // handle to it (e.g. the Services window is open).
    for I := 1 to 20 do
      if RegKeyExists(HKLM, ServiceKey) then
        Sleep(500);
    if RegKeyExists(HKLM, ServiceKey) then
    begin
      Result := 'The multirtmp service is still marked for deletion. Close the Services window (services.msc) and run the installer again.';
      Exit;
    end;
  end;

  // 4. Any running MediaMTX, from any folder: one started by hand would take
  //    the ports of the bundled one; ours would hold its files.
  RunLogged(ExpandConstant('{sys}\taskkill.exe'), '/F /T /IM mediamtx.exe', Output);

  // 5. Bring the old config over, if this folder has none yet. The old
  //    folder itself is left as it is.
  Dst := ExpandConstant('{app}\config.json');
  Src := OldConfigPath;
  if FileExists(Dst) then
    Log('config.json already exists in ' + ExpandConstant('{app}') + ', left untouched')
  else if (Src <> '') and FileExists(Src) and (CompareText(ExpandFileName(Src), ExpandFileName(Dst)) <> 0) then
  begin
    if ForceDirectories(ExtractFileDir(Dst)) and FileCopy(Src, Dst, True) then
      Log('Copied config from ' + Src + ' to ' + Dst)
    else
    begin
      Result := 'Could not copy the existing configuration from ' + Src + ' to ' + Dst + '.';
      Exit;
    end;
  end;
end;

// After the files are in place: register and start the service.
procedure CurStepChanged(CurStep: TSetupStep);
var
  Exe, Output: String;
begin
  if CurStep <> ssPostInstall then
    Exit;

  Exe := ExpandConstant('{app}\multirtmp.exe');
  if RunLogged(Exe, 'install', Output) <> 0 then
  begin
    ServiceStepFailed := True;
    if not WizardSilent then
      MsgBox('multirtmp was copied, but registering the service failed:' + #13#10#13#10 + Output, mbError, MB_OK);
    Exit;
  end;

  if RunLogged(Exe, 'start', Output) <> 0 then
  begin
    ServiceStepFailed := True;
    if not WizardSilent then
      MsgBox('multirtmp was installed, but the service did not start:' + #13#10#13#10 + Output, mbError, MB_OK);
  end;
end;

function GetCustomSetupExitCode: Integer;
begin
  if ServiceStepFailed then
    Result := ExitServiceFailed
  else
    Result := 0;
end;
