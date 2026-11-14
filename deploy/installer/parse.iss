// Pure helpers of the installer's [Code] that parse what it finds on the
// machine: the registered service's command line and config.json, plus the
// /status request. Kept in a separate file so deploy\installer\test\
// parse-test.iss can check them without running the installer itself.

// Takes the first argument from S (quoted or not) and returns it; S keeps the rest.
function TakeArg(var S: String): String;
var
  P: Integer;
begin
  S := Trim(S);
  if Copy(S, 1, 1) = '"' then
  begin
    Delete(S, 1, 1);
    P := Pos('"', S);
    if P = 0 then P := Length(S) + 1;
  end
  else
  begin
    P := Pos(' ', S);
    if P = 0 then P := Length(S) + 1;
  end;
  Result := Copy(S, 1, P - 1);
  Delete(S, 1, P);
end;

// Splits a service ImagePath into the exe path and the value of -config.
// Go's service registration quotes the exe path only if it has spaces, so
// both "C:\multi-rtmp\bin\multirtmp.exe -config x" and
// "\"C:\Program Files\x\multirtmp.exe\" -config \"D:\a b\c.json\"" occur.
function ParseImagePath(ImagePath: String; var Exe, ConfigArg: String): Boolean;
var
  Rest, Arg: String;
  P: Integer;
begin
  Exe := '';
  ConfigArg := '';
  ImagePath := Trim(ImagePath);
  P := Pos('.exe', Lowercase(ImagePath));
  Result := P > 0;
  if not Result then
    Exit;

  Exe := Copy(ImagePath, 1, P + 3);
  if Copy(Exe, 1, 1) = '"' then
    Delete(Exe, 1, 1);
  Rest := Copy(ImagePath, P + 4, MaxInt);
  if Copy(Rest, 1, 1) = '"' then
    Delete(Rest, 1, 1);

  while Trim(Rest) <> '' do
  begin
    Arg := TakeArg(Rest);
    if (CompareText(Arg, '-config') = 0) or (CompareText(Arg, '--config') = 0) then
      ConfigArg := TakeArg(Rest);
  end;
end;

// Value of "Key": "..." in S (crude, for config.json: enough for addr/token).
function JsonStringValue(const S, Key: String): String;
var
  P: Integer;
  T: String;
begin
  Result := '';
  P := Pos('"' + Key + '"', S);
  if P = 0 then Exit;
  T := Copy(S, P + Length(Key) + 2, MaxInt);
  P := Pos('"', T);
  if P = 0 then Exit;
  T := Copy(T, P + 1, MaxInt);
  P := Pos('"', T);
  if P = 0 then Exit;
  Result := Copy(T, 1, P - 1);
end;

// Port and token of the web API from the text of config.json: the "api"
// section's addr and token. Port 8080 (the default of every version) if not
// found.
procedure ApiEndpoint(const Cfg: String; var Port, Token: String);
var
  Section, Addr: String;
  P: Integer;
begin
  Port := '8080';
  Token := '';
  P := Pos('"api"', Cfg);
  if P = 0 then
    Exit;
  Section := Copy(Cfg, P, 400);
  Addr := JsonStringValue(Section, 'addr');
  Token := JsonStringValue(Section, 'token');
  P := Length(Addr);
  while (P > 0) and (Addr[P] <> ':') do
    P := P - 1;
  if (P > 0) and (P < Length(Addr)) then
    Port := Copy(Addr, P + 1, MaxInt);
end;

// Asks multirtmp's /status on this computer whether a source is connected.
// Anything that fails counts as "not live": the check must never block an
// update by itself.
function QueryLive(const Port, Token: String): Boolean;
var
  Http: Variant;
begin
  Result := False;
  try
    Http := CreateOleObject('WinHttp.WinHttpRequest.5.1');
    Http.Open('GET', 'http://127.0.0.1:' + Port + '/status', False);
    Http.SetTimeouts(2000, 2000, 2000, 3000);
    if Token <> '' then
      Http.SetRequestHeader('Authorization', 'Bearer ' + Token);
    Http.Send('');
    if Http.Status = 200 then
      Result := Pos('"source":{"connected":true', String(Http.ResponseText)) > 0;
    Log('Live check on port ' + Port + ': HTTP ' + IntToStr(Http.Status) + ', live=' + IntToStr(Ord(Result)));
  except
    Log('Live check on port ' + Port + ': no answer (' + GetExceptionMessage + ')');
  end;
end;
