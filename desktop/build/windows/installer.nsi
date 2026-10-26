; The Windows installer, after Wails' own NSIS template (wails_tools.nsh
; of the pinned version) with its per-user scope: no UAC prompt, into
; %LOCALAPPDATA%\Programs, like the sync clients users install themselves.
; `make windows` passes ARCH (amd64|arm64), VERSION, VI_VERSION (x.y.z.w),
; BIN (bin\windows: icon.ico, Microsoft's WebView2 bootstrapper from the
; Wails module, <arch>\*.exe) and OUTFILE, with backslashes: makensis on
; Windows finds no file in a path with forward slashes.
Unicode true
SetCompressor /SOLID lzma

!include "MUI2.nsh"
!include "LogicLib.nsh"
!include "WinVer.nsh"
!include "x64.nsh"
!include "FileFunc.nsh"

!define PRODUCT "Облако"
!define EXE "cloud-desktop.exe"
!define UNINST_KEY "Software\Microsoft\Windows\CurrentVersion\Uninstall\com.do-service.cloud"
; The WebView2 runtime's id in EdgeUpdate, the same for every channel.
!define WEBVIEW2_KEY "Microsoft\EdgeUpdate\Clients\{F3017226-FE2A-4295-8BDF-00C3A9A7E4C5}"

Name "${PRODUCT}"
OutFile "${OUTFILE}"
!define SRC "${BIN}\${ARCH}"
RequestExecutionLevel user
InstallDir "$LOCALAPPDATA\Programs\Cloud"
InstallDirRegKey HKCU "${UNINST_KEY}" "InstallLocation"
ManifestDPIAware true
ShowInstDetails show

!define MUI_ICON "${BIN}\icon.ico"
!define MUI_UNICON "${BIN}\icon.ico"
!define MUI_ABORTWARNING
!define MUI_FINISHPAGE_RUN "$INSTDIR\${EXE}"

!insertmacro MUI_PAGE_WELCOME
!insertmacro MUI_PAGE_DIRECTORY
!insertmacro MUI_PAGE_INSTFILES
!insertmacro MUI_PAGE_FINISH
!insertmacro MUI_UNPAGE_CONFIRM
!insertmacro MUI_UNPAGE_INSTFILES
!insertmacro MUI_LANGUAGE "Russian"

VIProductVersion "${VI_VERSION}"
VIFileVersion "${VI_VERSION}"
VIAddVersionKey /LANG=${LANG_RUSSIAN} "ProductName" "${PRODUCT}"
VIAddVersionKey /LANG=${LANG_RUSSIAN} "FileDescription" "Установка ${PRODUCT}"
VIAddVersionKey /LANG=${LANG_RUSSIAN} "CompanyName" "do-service"
VIAddVersionKey /LANG=${LANG_RUSSIAN} "LegalCopyright" "Apache License 2.0"
VIAddVersionKey /LANG=${LANG_RUSSIAN} "ProductVersion" "${VERSION}"
VIAddVersionKey /LANG=${LANG_RUSSIAN} "FileVersion" "${VERSION}"

Function .onInit
	${IfNot} ${AtLeastWin10}
		MessageBox MB_OK|MB_ICONSTOP "${PRODUCT} работает на Windows 10 и новее." /SD IDOK
		Abort
	${EndIf}
	; x64 code also runs on ARM64 Windows 11, through emulation.
	!if "${ARCH}" == "arm64"
		${IfNot} ${IsNativeARM64}
			MessageBox MB_OK|MB_ICONSTOP "Этот установщик — для Windows на ARM. Скачайте версию windows-amd64." /SD IDOK
			Abort
		${EndIf}
	!else
		${IfNot} ${IsNativeAMD64}
		${AndIfNot} ${IsNativeARM64}
			MessageBox MB_OK|MB_ICONSTOP "Нужна 64-разрядная Windows." /SD IDOK
			Abort
		${EndIf}
	!endif
FunctionEnd

Section
	SetShellVarContext current

	; As wails.webview2runtime: Windows 11 has the runtime, older Windows 10
	; may not; the bootstrapper downloads and installs it.
	SetRegView 64
	ReadRegStr $0 HKLM "SOFTWARE\WOW6432Node\${WEBVIEW2_KEY}" "pv"
	${If} $0 == ""
	${OrIf} $0 == "0.0.0.0"
		ReadRegStr $0 HKCU "Software\${WEBVIEW2_KEY}" "pv"
	${EndIf}
	${If} $0 == ""
	${OrIf} $0 == "0.0.0.0"
		DetailPrint "Установка WebView2 Runtime…"
		InitPluginsDir
		File "/oname=$PLUGINSDIR\MicrosoftEdgeWebview2Setup.exe" "${BIN}\MicrosoftEdgeWebview2Setup.exe"
		ExecWait '"$PLUGINSDIR\MicrosoftEdgeWebview2Setup.exe" /silent /install'
	${EndIf}

	SetOutPath "$INSTDIR"
	File "${SRC}\${EXE}"
	File "${SRC}\cloudsync.exe"
	CreateShortcut "$SMPROGRAMS\${PRODUCT}.lnk" "$INSTDIR\${EXE}"

	WriteUninstaller "$INSTDIR\uninstall.exe"
	WriteRegStr HKCU "${UNINST_KEY}" "DisplayName" "${PRODUCT}"
	WriteRegStr HKCU "${UNINST_KEY}" "DisplayVersion" "${VERSION}"
	WriteRegStr HKCU "${UNINST_KEY}" "Publisher" "do-service"
	WriteRegStr HKCU "${UNINST_KEY}" "DisplayIcon" "$INSTDIR\${EXE}"
	WriteRegStr HKCU "${UNINST_KEY}" "InstallLocation" "$INSTDIR"
	WriteRegStr HKCU "${UNINST_KEY}" "UninstallString" '"$INSTDIR\uninstall.exe"'
	WriteRegStr HKCU "${UNINST_KEY}" "QuietUninstallString" '"$INSTDIR\uninstall.exe" /S'
	WriteRegDWORD HKCU "${UNINST_KEY}" "NoModify" 1
	WriteRegDWORD HKCU "${UNINST_KEY}" "NoRepair" 1
	${GetSize} "$INSTDIR" "/S=0K" $0 $1 $2
	WriteRegDWORD HKCU "${UNINST_KEY}" "EstimatedSize" $0
SectionEnd

; The sync state and tokens (%LOCALAPPDATA%\Cloud) and the synced folders
; stay: they are the user's, and a reinstall picks them up.
Section "Uninstall"
	SetShellVarContext current
	Delete "$INSTDIR\${EXE}"
	Delete "$INSTDIR\cloudsync.exe"
	Delete "$INSTDIR\uninstall.exe"
	RMDir "$INSTDIR"
	Delete "$SMPROGRAMS\${PRODUCT}.lnk"
	; WebView2's profile, which it keeps under the executable's name.
	RMDir /r "$APPDATA\${EXE}"
	DeleteRegKey HKCU "${UNINST_KEY}"
SectionEnd
