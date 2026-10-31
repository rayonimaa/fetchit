@echo off
setlocal enabledelayedexpansion

if not exist "dist" mkdir dist

echo Building dist\fetchit-windows-amd64.exe...
set CGO_ENABLED=0
set GOOS=windows
set GOARCH=amd64
go build -ldflags "-s -w" -o dist\fetchit-windows-amd64.exe .
if errorlevel 1 exit /b 1

echo Building dist\fetchit-windows-arm64.exe...
set CGO_ENABLED=0
set GOOS=windows
set GOARCH=arm64
go build -ldflags "-s -w" -o dist\fetchit-windows-arm64.exe .
if errorlevel 1 exit /b 1

echo Building dist\fetchit-linux-amd64...
set CGO_ENABLED=0
set GOOS=linux
set GOARCH=amd64
go build -ldflags "-s -w" -o dist\fetchit-linux-amd64 .
if errorlevel 1 exit /b 1

echo Building dist\fetchit-linux-arm64...
set CGO_ENABLED=0
set GOOS=linux
set GOARCH=arm64
go build -ldflags "-s -w" -o dist\fetchit-linux-arm64 .
if errorlevel 1 exit /b 1

echo Building dist\fetchit-darwin-amd64...
set CGO_ENABLED=0
set GOOS=darwin
set GOARCH=amd64
go build -ldflags "-s -w" -o dist\fetchit-darwin-amd64 .
if errorlevel 1 exit /b 1

echo Building dist\fetchit-darwin-arm64...
set CGO_ENABLED=0
set GOOS=darwin
set GOARCH=arm64
go build -ldflags "-s -w" -o dist\fetchit-darwin-arm64 .
if errorlevel 1 exit /b 1

echo Installing locally to user CLI...
for /f "usebackq tokens=*" %%i in (`go env GOPATH`) do (
    if not exist "%%i\bin" mkdir "%%i\bin"
    copy /y dist\fetchit-windows-amd64.exe "%%i\bin\fetchit.exe" >nul
)
if exist "%USERPROFILE%\go\bin" (
    copy /y dist\fetchit-windows-amd64.exe "%USERPROFILE%\go\bin\fetchit.exe" >nul
)
if exist "%USERPROFILE%\.local\bin" (
    copy /y dist\fetchit-windows-amd64.exe "%USERPROFILE%\.local\bin\fetchit.exe" >nul
)

echo Done! All binaries built in dist\ and installed locally to CLI.
