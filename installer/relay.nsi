; SPDX-FileCopyrightText: 2026 Corey Koval <ckoval7@gmail.com>
; SPDX-License-Identifier: GPL-3.0-or-later

; The Windows installer. Build it with scripts/build-installer.sh.
;
; It installs for the person running it, so nobody needs administrator rights on
; Field Day morning. The token and the start-at-sign-in entry are per person too.
; installer_test.go checks the names shared with the relay.

Unicode true
SetCompressor /SOLID lzma

!include "MUI2.nsh"
!include "LogicLib.nsh"
!include "WinVer.nsh"
!include "WinMessages.nsh"
!include "x64.nsh"

!define PRODUCT "Field Day Commander Relay"
!define PUBLISHER "Corey Koval"
!define WINDOW_CLASS "FieldDayCommanderRelayTray"
!define INSTANCE_MUTEX "Global\FieldDayCommanderRelay"
!define RUN_KEY "Software\Microsoft\Windows\CurrentVersion\Run"
!define SWITCH_KEY "Software\Microsoft\Windows\CurrentVersion\Explorer\StartupApproved\Run"
!define UNINSTALL_KEY "Software\Microsoft\Windows\CurrentVersion\Uninstall\${PRODUCT}"
!define SYNCHRONIZE 0x00100000
!define CLOSE_WAIT_SECONDS 30

; Passed in by scripts/build-installer.sh.
!ifndef VERSION | NUMERIC_VERSION | RELAY_EXE | OUT_FILE
  !error "Build with scripts/build-installer.sh"
!endif

Name "${PRODUCT}"
OutFile "${OUT_FILE}"
InstallDir "$LOCALAPPDATA\Programs\${PRODUCT}"
RequestExecutionLevel user
ShowInstDetails nevershow
ShowUninstDetails nevershow
BrandingText " "

VIProductVersion "${NUMERIC_VERSION}"
VIAddVersionKey "ProductName" "${PRODUCT}"
VIAddVersionKey "ProductVersion" "${VERSION}"
VIAddVersionKey "FileVersion" "${NUMERIC_VERSION}"
VIAddVersionKey "FileDescription" "${PRODUCT} installer"
VIAddVersionKey "LegalCopyright" "2026 ${PUBLISHER}. GPL-3.0-or-later."

!define MUI_ICON "assets\relay.ico"
!define MUI_UNICON "assets\relay.ico"
!define MUI_ABORTWARNING

!define MUI_WELCOMEPAGE_TEXT "This installs ${PRODUCT} ${VERSION} for your Windows account. You don't need administrator rights.$\r$\n$\r$\nTo update, run the new installer. Your saved contacts and server connection are kept."
!insertmacro MUI_PAGE_WELCOME
!insertmacro MUI_PAGE_INSTFILES
!define MUI_PAGE_CUSTOMFUNCTION_SHOW FinishShow
!define MUI_FINISHPAGE_RUN "$INSTDIR\relay.exe"
!define MUI_FINISHPAGE_RUN_TEXT "Start the relay now"
!define MUI_FINISHPAGE_SHOWREADME ""
!define MUI_FINISHPAGE_SHOWREADME_NOTCHECKED
!define MUI_FINISHPAGE_SHOWREADME_TEXT "Start the relay when I sign in to Windows"
!define MUI_FINISHPAGE_SHOWREADME_FUNCTION StartAtSignIn
!insertmacro MUI_PAGE_FINISH

!define MUI_UNCONFIRMPAGE_TEXT_TOP "This removes ${PRODUCT}, its Start menu shortcut, and this install's start-at-sign-in entry.$\r$\n$\r$\nCollected contacts, settings, and the connection token stay on this computer. To remove the token too, click Disconnect on the status page before uninstalling."
!insertmacro MUI_UNPAGE_CONFIRM
!insertmacro MUI_UNPAGE_INSTFILES

!insertmacro MUI_LANGUAGE "English"

; Closing through the tray window lets the relay finish its work and close its data.
; Only a relay this person can open is looked for. Someone else's on this computer
; isn't using these files, and couldn't be closed from here anyway.
!macro CLOSE_RELAY_FUNCTION prefix
Function ${prefix}RelayIsRunning
  System::Call 'kernel32::OpenMutexW(i ${SYNCHRONIZE}, i 0, w "${INSTANCE_MUTEX}") p .r1'
  ${If} $1 P<> 0
    System::Call 'kernel32::CloseHandle(p r1)'
    StrCpy $2 1
  ${Else}
    StrCpy $2 0
  ${EndIf}
FunctionEnd

Function ${prefix}CloseRelay
  Call ${prefix}RelayIsRunning
  ${If} $2 = 0
    Return
  ${EndIf}
  MessageBox MB_OKCANCEL|MB_ICONINFORMATION "${PRODUCT} is running. Click OK to close it and continue.$\r$\n$\r$\nSaved contacts are kept. Contacts your loggers send while the relay is closed may need sending again. N1MM broadcasts are lost while it's closed." /SD IDOK IDOK close
  Abort
  close:
  FindWindow $0 "${WINDOW_CLASS}"
  ${If} $0 <> 0
    SendMessage $0 ${WM_CLOSE} 0 0 /TIMEOUT=5000
  ${EndIf}
  retry:
  StrCpy $3 0
  ${Do}
    Call ${prefix}RelayIsRunning
    ${If} $2 = 0
      Return
    ${EndIf}
    Sleep 1000
    IntOp $3 $3 + 1
  ${LoopUntil} $3 >= ${CLOSE_WAIT_SECONDS}
  MessageBox MB_RETRYCANCEL|MB_ICONEXCLAMATION "${PRODUCT} is still running. Right-click its tray icon and choose Quit relay, then click Retry." /SD IDCANCEL IDRETRY retry
  Abort
FunctionEnd
!macroend
!insertmacro CLOSE_RELAY_FUNCTION ""
!insertmacro CLOSE_RELAY_FUNCTION "un."

Function .onInit
  ${IfNot} ${AtLeastWin10}
  ${OrIfNot} ${RunningX64}
    MessageBox MB_OK|MB_ICONSTOP "${PRODUCT} needs 64-bit Windows 10 or later." /SD IDOK
    Abort
  ${EndIf}
FunctionEnd

; The relay already starts at sign-in after an update, so don't offer it again. An
; entry for some other copy still gets the choice, which replaces it.
Function FinishShow
  ReadRegStr $0 HKCU "${RUN_KEY}" "${PRODUCT}"
  ${If} $0 == '"$INSTDIR\relay.exe" --no-browser'
    ShowWindow $mui.FinishPage.ShowReadme ${SW_HIDE}
  ${EndIf}
FunctionEnd

Function StartAtSignIn
  WriteRegStr HKCU "${RUN_KEY}" "${PRODUCT}" '"$INSTDIR\relay.exe" --no-browser'
  DeleteRegValue HKCU "${SWITCH_KEY}" "${PRODUCT}"
FunctionEnd

Section
  SetShellVarContext current
  Call CloseRelay

  SetOutPath "$INSTDIR"
  File "/oname=relay.exe" "${RELAY_EXE}"
  WriteUninstaller "$INSTDIR\uninstall.exe"
  CreateShortcut "$SMPROGRAMS\${PRODUCT}.lnk" "$INSTDIR\relay.exe"

  WriteRegStr HKCU "${UNINSTALL_KEY}" "DisplayName" "${PRODUCT}"
  WriteRegStr HKCU "${UNINSTALL_KEY}" "DisplayVersion" "${VERSION}"
  WriteRegStr HKCU "${UNINSTALL_KEY}" "Publisher" "${PUBLISHER}"
  WriteRegStr HKCU "${UNINSTALL_KEY}" "DisplayIcon" "$INSTDIR\relay.exe"
  WriteRegStr HKCU "${UNINSTALL_KEY}" "InstallLocation" "$INSTDIR"
  WriteRegStr HKCU "${UNINSTALL_KEY}" "UninstallString" '"$INSTDIR\uninstall.exe"'
  WriteRegStr HKCU "${UNINSTALL_KEY}" "QuietUninstallString" '"$INSTDIR\uninstall.exe" /S'
  WriteRegDWORD HKCU "${UNINSTALL_KEY}" "NoModify" 1
  WriteRegDWORD HKCU "${UNINSTALL_KEY}" "NoRepair" 1
  SectionGetSize 0 $0
  WriteRegDWORD HKCU "${UNINSTALL_KEY}" "EstimatedSize" $0
SectionEnd

; The data folder and the keychain's token stay. Losing a contact is the worst thing
; this program can do, and someone may just be reinstalling.
Section "Uninstall"
  SetShellVarContext current
  Call un.CloseRelay

  ; Only this copy's entry: another copy's isn't this uninstaller's to remove.
  ReadRegStr $0 HKCU "${RUN_KEY}" "${PRODUCT}"
  ${If} $0 == '"$INSTDIR\relay.exe" --no-browser'
    DeleteRegValue HKCU "${RUN_KEY}" "${PRODUCT}"
    DeleteRegValue HKCU "${SWITCH_KEY}" "${PRODUCT}"
  ${EndIf}

  Delete "$SMPROGRAMS\${PRODUCT}.lnk"
  Delete "$INSTDIR\relay.exe"
  Delete "$INSTDIR\uninstall.exe"
  RMDir "$INSTDIR"
  DeleteRegKey HKCU "${UNINSTALL_KEY}"
SectionEnd
