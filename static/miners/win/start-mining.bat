@echo off
cd /d "%~dp0"
title Scorbits Miner
set /p ADDR="Entrez votre adresse SCO : "
if "%ADDR%"=="" (echo Adresse vide. && pause && exit /b 1)
:loop
scorbits-miner-windows.exe --address %ADDR% --node https://scorbits.com --threads 2
timeout /t 3 /nobreak >nul
goto loop
