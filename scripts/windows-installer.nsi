; Комплект устанавливается для текущего пользователя. Ключи и настройки
; находятся отдельно в APPDATA\Marvia и не входят в удаляемые файлы.
Unicode True
RequestExecutionLevel user
CRCCheck force
SetCompressor /SOLID lzma

!include "MUI2.nsh"
!include "LogicLib.nsh"
!include "x64.nsh"

!ifdef NSIS_WIN32_MAKENSIS
  !define SOURCE_SEPARATOR "\"
!else
  !define SOURCE_SEPARATOR "/"
!endif

Name "Marvia ${VERSION}"
OutFile "${OUTPUT_FILE}"
InstallDir "$LOCALAPPDATA\Programs\Marvia"
InstallDirRegKey HKCU "Software\Marvia" "InstallPath"
VIProductVersion "${NUMERIC_VERSION}"
VIAddVersionKey /LANG=1049 "ProductName" "Marvia"
VIAddVersionKey /LANG=1049 "ProductVersion" "${VERSION}"
VIAddVersionKey /LANG=1049 "FileDescription" "Установка Marvia для Windows"
VIAddVersionKey /LANG=1049 "FileVersion" "${VERSION}"
VIAddVersionKey /LANG=1049 "LegalCopyright" "Marvia"
ShowInstDetails show
ShowUninstDetails show

!define MUI_ABORTWARNING
!define MUI_WELCOMEPAGE_TITLE "Marvia ${VERSION}"
!define MUI_WELCOMEPAGE_TEXT "Установка приложения и драйвера VPN.$\r$\n$\r$\nПриложение и Wintun уже в комплекте. Для окна нужен Microsoft Edge WebView2 Runtime: при его отсутствии Marvia предложит официальную загрузку.$\r$\n$\r$\nВаши ключи, язык и настройки сохраняются при обновлении. Перед установкой завершите Marvia через «Выйти» в трее."
!insertmacro MUI_PAGE_WELCOME
!insertmacro MUI_PAGE_DIRECTORY
!insertmacro MUI_PAGE_INSTFILES
!define MUI_FINISHPAGE_RUN "$INSTDIR\marvia-windows.exe"
!define MUI_FINISHPAGE_RUN_TEXT "Открыть Marvia"
!define MUI_FINISHPAGE_TEXT "Marvia установлена. Добавьте свою ссылку доступа и подключитесь.$\r$\n$\r$\nПри запуске приложение запросит права администратора для VPN-адаптера."
!insertmacro MUI_PAGE_FINISH
!insertmacro MUI_UNPAGE_CONFIRM
!insertmacro MUI_UNPAGE_INSTFILES
!insertmacro MUI_LANGUAGE "Russian"
!insertmacro MUI_LANGUAGE "English"

Function .onInit
  ${IfNot} ${IsNativeAMD64}
    MessageBox MB_ICONSTOP "Этот комплект предназначен для Windows x64." /SD IDOK
    SetErrorLevel 2
    Quit
  ${EndIf}
  SetRegView 64
  !ifndef TEST_INSTALL
    FindWindow $0 "MarviaTray"
    ${If} $0 != 0
      MessageBox MB_ICONINFORMATION "Marvia сейчас работает. Выберите «Выйти» в трее и запустите установщик снова." /SD IDOK
      SetErrorLevel 2
      Quit
    ${EndIf}
  !endif
FunctionEnd

Section "Marvia"
  SetOutPath "$INSTDIR"
  SetOverwrite on
  File "${INPUT_DIR}${SOURCE_SEPARATOR}marvia-windows.exe"
  File "${INPUT_DIR}${SOURCE_SEPARATOR}wintun.dll"
  File "${INPUT_DIR}${SOURCE_SEPARATOR}WINTUN-LICENSE.txt"
  File "${INPUT_DIR}${SOURCE_SEPARATOR}НАЧНИТЕ-ЗДЕСЬ.txt"
  WriteUninstaller "$INSTDIR\Удалить Marvia.exe"
  !ifndef TEST_INSTALL
    CreateDirectory "$SMPROGRAMS\Marvia"
    CreateShortcut "$SMPROGRAMS\Marvia\Marvia.lnk" "$INSTDIR\marvia-windows.exe"
    CreateShortcut "$SMPROGRAMS\Marvia\Удалить Marvia.lnk" "$INSTDIR\Удалить Marvia.exe"
    WriteRegStr HKCU "Software\Marvia" "InstallPath" "$INSTDIR"
    WriteRegStr HKCU "Software\Microsoft\Windows\CurrentVersion\Uninstall\Marvia" "DisplayName" "Marvia"
    WriteRegStr HKCU "Software\Microsoft\Windows\CurrentVersion\Uninstall\Marvia" "DisplayVersion" "${VERSION}"
    WriteRegStr HKCU "Software\Microsoft\Windows\CurrentVersion\Uninstall\Marvia" "Publisher" "Marvia"
    WriteRegStr HKCU "Software\Microsoft\Windows\CurrentVersion\Uninstall\Marvia" "UninstallString" '$\"$INSTDIR\Удалить Marvia.exe$\"'
    WriteRegStr HKCU "Software\Microsoft\Windows\CurrentVersion\Uninstall\Marvia" "DisplayIcon" "$INSTDIR\marvia-windows.exe"
    WriteRegStr HKCU "Software\Microsoft\Windows\CurrentVersion\Uninstall\Marvia" "InstallLocation" "$INSTDIR"
    WriteRegDWORD HKCU "Software\Microsoft\Windows\CurrentVersion\Uninstall\Marvia" "NoModify" 1
    WriteRegDWORD HKCU "Software\Microsoft\Windows\CurrentVersion\Uninstall\Marvia" "NoRepair" 1
  !endif
SectionEnd

Function un.onInit
  SetRegView 64
  !ifndef TEST_INSTALL
    FindWindow $0 "MarviaTray"
    ${If} $0 != 0
      MessageBox MB_ICONINFORMATION "Сначала завершите Marvia через «Выйти» в трее." /SD IDOK
      SetErrorLevel 2
      Quit
    ${EndIf}
  !endif
FunctionEnd

Section "Uninstall"
  ; Только известные файлы: выбранная папка может содержать чужие документы.
  Delete "$INSTDIR\marvia-windows.exe"
  Delete "$INSTDIR\wintun.dll"
  Delete "$INSTDIR\WINTUN-LICENSE.txt"
  Delete "$INSTDIR\НАЧНИТЕ-ЗДЕСЬ.txt"
  Delete "$INSTDIR\Удалить Marvia.exe"
  RMDir "$INSTDIR"
  !ifndef TEST_INSTALL
    Delete "$SMPROGRAMS\Marvia\Marvia.lnk"
    Delete "$SMPROGRAMS\Marvia\Удалить Marvia.lnk"
    RMDir "$SMPROGRAMS\Marvia"
    DeleteRegKey HKCU "Software\Microsoft\Windows\CurrentVersion\Uninstall\Marvia"
    DeleteRegValue HKCU "Software\Marvia" "InstallPath"
    ; Переносимый клиент мог уже переписать схему на другой путь. Удаляем
    ; только регистрацию, которая всё ещё указывает на эту установку.
    ReadRegStr $0 HKCU "Software\Classes\marvia\shell\open\command" ""
    ${If} $0 == '$\"$INSTDIR\marvia-windows.exe$\" $\"%1$\"'
      DeleteRegKey HKCU "Software\Classes\marvia"
    ${EndIf}
    ReadRegStr $0 HKCU "Software\Classes\veil\shell\open\command" ""
    ${If} $0 == '$\"$INSTDIR\marvia-windows.exe$\" $\"%1$\"'
      DeleteRegKey HKCU "Software\Classes\veil"
    ${EndIf}
  !endif
SectionEnd
