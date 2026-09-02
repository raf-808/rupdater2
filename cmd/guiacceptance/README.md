# guiacceptance

Windows-only real-input acceptance helper for `Updater.exe`.

It does **not** call updater handlers or send `WM_COMMAND`. Mouse and keyboard actions go through Win32 `SendInput`, and screenshots copy the actual desktop pixels covered by the foreground updater window.

## Build

```powershell
Set-Location C:\U-geminiServer_release\updater
$env:GOROOT = 'C:\Program Files\Go'
$env:PATH = 'C:\Program Files\Go\bin;' + $env:PATH
go build -o ..\build\GuiAcceptance.exe .\cmd\guiacceptance
```

## Inspect the live updater

Start the real `build\Updater.exe`, then:

```powershell
..\build\GuiAcceptance.exe -list -check-overlap -screenshot ..\build\gui-current.png
```

`-check-overlap` checks visible non-group-box Win32 `Button` controls by their real screen rectangles. Hidden dynamic buttons are ignored.

If more than one updater window exists, pass the target process ID:

```powershell
..\build\GuiAcceptance.exe -pid 1234 -list
```

## Real keyboard input

```powershell
..\build\GuiAcceptance.exe -key R
..\build\GuiAcceptance.exe -key S
..\build\GuiAcceptance.exe -key A
..\build\GuiAcceptance.exe -key Esc
```

Repeated input for duplicate-submit/auto-repeat guards:

```powershell
..\build\GuiAcceptance.exe -key S -repeat 8 -interval 25ms
```

The tool first verifies that the updater is the foreground window. If Windows refuses foreground activation, it fails instead of sending input to another application.

## Real mouse input and focus

Click a visible control by caption:

```powershell
..\build\GuiAcceptance.exe -click '跳过（S）'
..\build\GuiAcceptance.exe -click '打开网盘'
..\build\GuiAcceptance.exe -click '复制下载信息'
```

Focus the first visible `Edit` control using a real mouse click, then send a shortcut in a second invocation:

```powershell
..\build\GuiAcceptance.exe -focus-class Edit
..\build\GuiAcceptance.exe -key S
```

Use `-index N` when multiple visible controls have the same class.

## Screenshot and clipboard

```powershell
..\build\GuiAcceptance.exe -screenshot ..\build\gui-after-skip.png
..\build\GuiAcceptance.exe -clipboard
```

The screenshot path uses desktop `BitBlt`, so it represents the visible screen pixels, including real Win32 theme/layout. Keep the updater unobscured when capturing.

`-clipboard` reads the system `CF_UNICODETEXT` value and can be used after clicking `复制下载信息` to verify package name, URL, and extraction code.
