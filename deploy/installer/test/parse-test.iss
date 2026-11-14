; Test of deploy\installer\parse.iss — the installer's code that parses the
; service command line and config.json and asks /status whether a stream is
; live. It installs nothing: no app folder, no uninstaller, no admin rights.
;
; Run (see test-installer-parse.ps1):
;   parse-test.exe /VERYSILENT /RESULT=<file> /TESTDATA=<internal\config\testdata>
;       /INSTALLCFG=<config made by "install"> /LIVEPORT=<port> /EXPECTLIVE=0|1
; Exit code 0 — all checks passed, 1 — some failed (details in <file>).

[Setup]
AppName=multirtmp-installer-parse-test
AppVersion=0
CreateAppDir=no
Uninstallable=no
PrivilegesRequired=lowest
OutputDir=.
OutputBaseFilename=parse-test

[Code]
#include "..\parse.iss"

procedure ExitProcess(uExitCode: Cardinal); external 'ExitProcess@kernel32.dll stdcall';
function GetTickCount: Cardinal; external 'GetTickCount@kernel32.dll stdcall';

var
  Report: TArrayOfString;
  Failed: Boolean;

procedure Check(const Name: String; Ok: Boolean; const Detail: String);
var
  N: Integer;
begin
  N := GetArrayLength(Report);
  SetArrayLength(Report, N + 1);
  if Ok then
    Report[N] := 'PASS  ' + Name
  else
  begin
    Report[N] := 'FAIL  ' + Name + ' — ' + Detail;
    Failed := True;
  end;
end;

procedure CheckImagePath(const ImagePath, WantExe, WantCfg: String; WantOk: Boolean);
var
  Exe, Cfg: String;
  Ok: Boolean;
begin
  Ok := ParseImagePath(ImagePath, Exe, Cfg);
  Check('ImagePath [' + ImagePath + ']',
        (Ok = WantOk) and (Exe = WantExe) and (Cfg = WantCfg),
        'got ok=' + IntToStr(Ord(Ok)) + ' exe=[' + Exe + '] cfg=[' + Cfg + ']');
end;

procedure CheckApi(const Name, Cfg, WantPort, WantToken: String);
var
  Port, Token: String;
begin
  ApiEndpoint(Cfg, Port, Token);
  Check('api ' + Name, (Port = WantPort) and (Token = WantToken),
        'got port=[' + Port + '] token=[' + Token + ']');
end;

function FileText(const Path: String): String;
var
  S: AnsiString;
begin
  Result := '';
  if LoadStringFromFile(Path, S) then
    Result := String(S)
  else
    Check('read ' + Path, False, 'cannot read');
end;

function InitializeSetup(): Boolean;
var
  TestData, InstallCfg, LivePort: String;
  Live, WantLive: Boolean;
  T0: Cardinal;
begin
  Failed := False;

  // Service command lines as Go's mgr.CreateService writes them.
  CheckImagePath('C:\multi-rtmp\bin\multirtmp.exe', 'C:\multi-rtmp\bin\multirtmp.exe', '', True);
  CheckImagePath('C:\multi-rtmp\bin\multirtmp.exe -config C:\multi-rtmp\bin\config.json',
                 'C:\multi-rtmp\bin\multirtmp.exe', 'C:\multi-rtmp\bin\config.json', True);
  CheckImagePath('"C:\Program Files\relay\multirtmp.exe" -config "D:\my cfg\config.json"',
                 'C:\Program Files\relay\multirtmp.exe', 'D:\my cfg\config.json', True);
  CheckImagePath('"C:\Program Files\relay\multirtmp.exe"', 'C:\Program Files\relay\multirtmp.exe', '', True);
  CheckImagePath('  C:\multirtmp\MULTIRTMP.EXE  ', 'C:\multirtmp\MULTIRTMP.EXE', '', True);
  CheckImagePath('svchost -k netsvcs', '', '', False);

  // Real configs: both v1.0.0 testdata files and the one "install" creates.
  TestData := ExpandConstant('{param:TESTDATA|}');
  CheckApi('v1.0.0 site config', FileText(TestData + '\v1.0.0-site.json'), '8080', '');
  CheckApi('v1.0.0 sample (comments mention addr before the api section)',
           FileText(TestData + '\v1.0.0-sample.json'), '8080', '__TOKEN__');
  InstallCfg := ExpandConstant('{param:INSTALLCFG|}');
  CheckApi('install-generated config', FileText(InstallCfg), '8080',
           JsonStringValue(Copy(FileText(InstallCfg), Pos('"api"', FileText(InstallCfg)), 400), 'token'));
  CheckApi('custom port and token', '{"api": {"addr": "127.0.0.1:9090", "token": "abc"}}', '9090', 'abc');
  CheckApi('no api section', '{"outputs": []}', '8080', '');

  // /status of a real multirtmp.
  LivePort := ExpandConstant('{param:LIVEPORT|}');
  if LivePort <> '' then
  begin
    WantLive := ExpandConstant('{param:EXPECTLIVE|0}') = '1';
    Live := QueryLive(LivePort, '');
    Check('QueryLive on ' + LivePort + ' (expect live=' + IntToStr(Ord(WantLive)) + ')', Live = WantLive,
          'got live=' + IntToStr(Ord(Live)));
  end;

  // Nobody listening: "not live", and quickly.
  T0 := GetTickCount;
  Live := QueryLive('1', '');
  Check('QueryLive on a closed port', not Live, 'got live');
  Check('QueryLive on a closed port is fast', GetTickCount - T0 < 5000, IntToStr(GetTickCount - T0) + ' ms');

  SaveStringsToUTF8File(ExpandConstant('{param:RESULT|parse-test-result.txt}'), Report, False);
  if Failed then
    ExitProcess(1)
  else
    ExitProcess(0);
  Result := False;
end;
