#ifndef BuildRoot
  #define BuildRoot "..\.tools\setup-payload"
#endif
#define AppVersion "0.3.3"

[Setup]
AppId={{94CD94E5-53DB-4320-BAB7-5FAC0B9BC8D4}
AppName=链跃 LinkWeaver
AppVersion={#AppVersion}
AppPublisher=LinkWeaver
DefaultDirName={code:DefaultInstallDir}
DefaultGroupName=链跃 LinkWeaver
DisableDirPage=no
DisableProgramGroupPage=yes
PrivilegesRequired=lowest
ArchitecturesAllowed=x64compatible
ArchitecturesInstallIn64BitMode=x64compatible
MinVersion=10.0
OutputDir=..\dist
OutputBaseFilename=LinkWeaver-Setup-{#AppVersion}-Windows-x64
SetupIconFile={#BuildRoot}\LinkWeaver.ico
UninstallDisplayIcon={app}\LinkWeaver.ico
UninstallDisplayName=链跃 LinkWeaver
Compression=lzma2
SolidCompression=yes
WizardStyle=modern
CloseApplications=no
RestartApplications=no
SetupLogging=yes
VersionInfoVersion={#AppVersion}.0

[Languages]
Name: "chinesesimplified"; MessagesFile: "compiler:Languages\ChineseSimplified.isl"

[Tasks]
Name: "autostart"; Description: "登录 Windows 后自动启动到托盘"; GroupDescription: "启动选项："; Flags: unchecked
Name: "desktopicon"; Description: "创建桌面快捷方式"; GroupDescription: "快捷方式："

[Files]
Source: "{#BuildRoot}\LinkWeaver.exe"; DestDir: "{app}"; Flags: ignoreversion
Source: "{#BuildRoot}\LinkWeaver.exe"; DestName: "LinkWeaver-setup-helper.exe"; Flags: dontcopy
Source: "{#BuildRoot}\LinkWeaver.ico"; DestDir: "{app}"; Flags: ignoreversion
Source: "{#BuildRoot}\README.md"; DestDir: "{app}"; Flags: ignoreversion
Source: "{#BuildRoot}\bin\mihomo.exe"; DestDir: "{app}\bin"; Flags: ignoreversion
Source: "{#BuildRoot}\licenses\*"; DestDir: "{app}\licenses"; Flags: ignoreversion overwritereadonly

[Dirs]
Name: "{app}\data"
Name: "{app}\runtime"

[Icons]
Name: "{autoprograms}\链跃 LinkWeaver"; Filename: "{app}\LinkWeaver.exe"; Parameters: "--open"; WorkingDir: "{app}"; IconFilename: "{app}\LinkWeaver.ico"
Name: "{autodesktop}\链跃 LinkWeaver"; Filename: "{app}\LinkWeaver.exe"; Parameters: "--open"; WorkingDir: "{app}"; IconFilename: "{app}\LinkWeaver.ico"; Tasks: desktopicon

[Run]
Filename: "{app}\LinkWeaver.exe"; Parameters: "--open"; WorkingDir: "{app}"; Description: "启动链跃并打开网页管理端"; Flags: nowait postinstall skipifsilent

[Code]
function DefaultInstallDir(Param: String): String;
begin
  if DirExists('D:\') then
    Result := 'D:\LinkWeaver'
  else
    Result := ExpandConstant('{localappdata}\Programs\LinkWeaver');
end;

function PrepareToInstall(var NeedsRestart: Boolean): String;
var
  ExitCode: Integer;
begin
  Result := '';
  if FileExists(ExpandConstant('{app}\data\instance.json')) then
  begin
    ExtractTemporaryFile('LinkWeaver-setup-helper.exe');
    if not Exec(ExpandConstant('{tmp}\LinkWeaver-setup-helper.exe'),
      '--root "' + ExpandConstant('{app}') + '" --shutdown', '',
      SW_HIDE, ewWaitUntilTerminated, ExitCode) then
      Result := '无法关闭正在运行的链跃，请先从托盘退出程序后重试。'
    else if ExitCode <> 0 then
      Result := '链跃尚未正常退出，请先从托盘退出程序后重试。';
  end;
end;

procedure CurStepChanged(CurStep: TSetupStep);
var
  StartupOption: String;
  ExitCode: Integer;
begin
  if CurStep = ssPostInstall then
  begin
    if WizardIsTaskSelected('autostart') then
      StartupOption := 'on'
    else
      StartupOption := 'off';
    if not Exec(ExpandConstant('{app}\LinkWeaver.exe'),
      '--root "' + ExpandConstant('{app}') + '" --set-startup=' + StartupOption,
      ExpandConstant('{app}'), SW_HIDE, ewWaitUntilTerminated, ExitCode) then
      RaiseException('程序文件已安装，但无法应用启动选项，请重新运行安装程序。')
    else if ExitCode <> 0 then
      RaiseException('启动选项保存失败，请查看安装目录 data\install-error.txt 后重试。');
  end;
end;

function InitializeUninstall(): Boolean;
var
  ExitCode: Integer;
begin
  Result := False;
  if FileExists(ExpandConstant('{app}\LinkWeaver.exe')) then
  begin
    if not Exec(ExpandConstant('{app}\LinkWeaver.exe'),
      '--root "' + ExpandConstant('{app}') + '" --shutdown',
      ExpandConstant('{app}'), SW_HIDE, ewWaitUntilTerminated, ExitCode) then
      ExitCode := 1;
    if ExitCode <> 0 then
    begin
      SuppressibleMsgBox('请先从托盘退出链跃，再进行卸载。', mbError, MB_OK, IDOK);
      Exit;
    end;
    if not Exec(ExpandConstant('{app}\LinkWeaver.exe'),
      '--root "' + ExpandConstant('{app}') + '" --remove-startup',
      ExpandConstant('{app}'), SW_HIDE, ewWaitUntilTerminated, ExitCode) then
      ExitCode := 1;
    if ExitCode <> 0 then
    begin
      SuppressibleMsgBox('无法移除开机启动项，卸载已停止。', mbError, MB_OK, IDOK);
      Exit;
    end;
  end;
  Result := True;
end;
