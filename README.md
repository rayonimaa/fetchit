# fetchit

Codex-style YouTube downloader. One binary, no installs. Auto-downloads yt-dlp and ffmpeg on first run.

## Download

Pre-compiled static binaries are available for each platform:

- [Windows (amd64)](https://github.com/example/fetchit/releases/latest/download/fetchit-windows-amd64.exe)
- [Windows (arm64)](https://github.com/example/fetchit/releases/latest/download/fetchit-windows-arm64.exe)
- [Linux (amd64)](https://github.com/example/fetchit/releases/latest/download/fetchit-linux-amd64)
- [Linux (arm64)](https://github.com/example/fetchit/releases/latest/download/fetchit-linux-arm64)
- [macOS (Apple Silicon - arm64)](https://github.com/example/fetchit/releases/latest/download/fetchit-darwin-arm64)
- [macOS (Intel - amd64)](https://github.com/example/fetchit/releases/latest/download/fetchit-darwin-amd64)

> Windows: just double-click `fetchit-windows-amd64.exe`.

## First Run Note

The first run downloads ~75 MB (`yt-dlp` + `ffmpeg`) into your OS cache directory:
- Windows: `%LOCALAPPDATA%\fetchit\bin`
- Linux: `~/.cache/fetchit/bin`
- macOS: `~/Library/Caches/fetchit/bin`

Subsequent runs are instant.

## Usage Examples

```bash
# Basic video download (default: mp4, best quality)
fetchit https://youtu.be/...

# Extract audio as MP3 at 320 kbps
fetchit -m -q 320 <url>

# Download video capped at 4K resolution
fetchit -q 4k <url>

# Verbose output showing raw engine logs
fetchit -V <url>

# Clean cached binaries
fetchit --clean

# Interactive wizard (no arguments)
fetchit
```

## CLI Reference

```text
Usage:
  fetchit [options] <url>...

Options:
  -f, --format <fmt>     video: mp4 mkv webm mov
                         audio: mp3 m4a opus flac wav aac
                         (default: mp4)
  -q, --quality <q>      video: best 8k 4k 2k 1440 1080 720 480 360
                         audio: best 320 256 192 128 (kbps)
                         (default: best)
  -o, --output <dir>     initial save directory (default: $HOME/Downloads)
  -V, --verbose          show full yt-dlp log stream (no progress bar)
  --debug                implies --verbose, plus stderr dumps on resolve fail
  --bootstrap            delete cache + re-download yt-dlp and ffmpeg
  --clean                remove cache dir, exit
  -y, --yes              skip end-of-run prompts (for CI/scripting)
  -m, --mp3              shorthand for -f mp3
  -v, --mp4              shorthand for -f mp4
  -n, --dry-run          print resolved yt-dlp argv, don't download
  -h, --help             print help with the codex box header
  --version              print "fetchit 1.0"
```
