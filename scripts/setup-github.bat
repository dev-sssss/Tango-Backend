@echo off
rem One-time setup: AWS access for the GitHub deploy pipeline.
powershell -NoProfile -ExecutionPolicy Bypass -File "%~dp0setup-github.ps1" %*
pause
