; Inno Setup 6 安装脚本 —— 社媒图文水印抹除工具
;
; 编译（在项目根目录执行）:
;   "C:\Program Files (x86)\Inno Setup 6\ISCC.exe" packaging\installer.iss
; 产物:
;   build\installer\社媒图文水印抹除工具_Setup_1.1.1.exe
;
; 说明:
;   - lamacore\ 为 PyInstaller onedir 引擎束（含 big-lama.pt，约 670MB），
;     整目录递归打包；lzma2/max 压缩（非 solid，兼顾压缩率与编译耗时）。
;   - PrivilegesRequired=lowest：按用户安装（%LOCALAPPDATA%\Programs），
;     无需管理员权限，卸载入口写入 HKCU。
;   - 中文界面使用 Inno 6（6.3+）官方随附的 Languages\ChineseSimplified.isl；
;     若编译环境缺失该文件，删除 [Languages] 的 chinesesimplified 行即可回退英文。
;   - 本文件含中文，必须以 UTF-8（带 BOM）保存，否则 ISCC 按 ANSI 解析会乱码。

#define MyAppName "社媒图文水印抹除工具"
#define MyAppVersion "1.1.1"
#define MyAppPublisher "csy"
#define MyAppExeName "社媒图文水印抹除工具.exe"

[Setup]
AppId={{8F7A2C90-5B1E-4C6D-9E3F-A1B2C3D4E5F6}
AppName={#MyAppName}
AppVersion={#MyAppVersion}
AppPublisher={#MyAppPublisher}
DefaultDirName={autopf}\{#MyAppName}
DefaultGroupName={#MyAppName}
DisableProgramGroupPage=yes
OutputDir=..\build\installer
OutputBaseFilename=社媒图文水印抹除工具_Setup_{#MyAppVersion}
Compression=lzma2/max
SolidCompression=no
WizardStyle=modern
PrivilegesRequired=lowest
UninstallDisplayIcon={app}\{#MyAppExeName}
SetupLogging=yes

[Languages]
; winget 安装的 Inno Setup 6.7.3 未随附 ChineseSimplified.isl（中文属非官方
; 翻译包，不随发行版分发），按预案回退英文界面。如需中文：获取
; ChineseSimplified.isl 放入 <Inno Setup>\Languages\ 后，删除下一行并
; 取消注释再下一行即可。
Name: "default"; MessagesFile: "compiler:Default.isl"
;Name: "chinesesimplified"; MessagesFile: "compiler:Languages\ChineseSimplified.isl"

[Tasks]
Name: "desktopicon"; Description: "{cm:CreateDesktopIcon}"; GroupDescription: "{cm:AdditionalIcons}"

[Files]
; 主程序（Wails 构建产物，自带应用图标）
Source: "..\build\bin\{#MyAppExeName}"; DestDir: "{app}"; Flags: ignoreversion
; Python 引擎束整目录递归打包（含子目录中的 _internal、big-lama.pt 等）
Source: "..\build\bin\lamacore\*"; DestDir: "{app}\lamacore"; Flags: ignoreversion recursesubdirs createallsubdirs

[Icons]
Name: "{group}\{#MyAppName}"; Filename: "{app}\{#MyAppExeName}"
Name: "{autodesktop}\{#MyAppName}"; Filename: "{app}\{#MyAppExeName}"; Tasks: desktopicon

[Run]
Filename: "{app}\{#MyAppExeName}"; Description: "{cm:LaunchProgram,{#MyAppName}}"; Flags: nowait postinstall skipifsilent
