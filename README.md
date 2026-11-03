# fetchit

Commandline-based video/audio downloader. One binary, no installs. Auto-downloads song/playlist/album with metadata on first run.

## Download

Pre-compiled static binaries are available for each platform:

- [Windows (x86)](https://github.com/example/fetchit/releases/latest/download/fetchit-windows-amd64.exe)
- [Windows (arm64)](https://github.com/example/fetchit/releases/latest/download/fetchit-windows-arm64.exe)

- [Linux (x86)](https://github.com/example/fetchit/releases/latest/download/fetchit-linux-amd64)
- [Linux (arm64)](https://github.com/example/fetchit/releases/latest/download/fetchit-linux-arm64)

- [macOS (Apple Silicon)](https://github.com/example/fetchit/releases/latest/download/fetchit-darwin-arm64)
- [macOS (x86)](https://github.com/example/fetchit/releases/latest/download/fetchit-darwin-amd64)

## Usage Examples

```bash
# Interactive wizard (no arguments)
fetchit

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
```

## Command Reference

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

## First Run Note

The first run required downloads ~75 MB (`yt-dlp` + `ffmpeg`) into your OS cache directory:

- Windows: `%LOCALAPPDATA%\fetchit\bin`
- Linux: `~/.cache/fetchit/bin`
- macOS: `~/Library/Caches/fetchit/bin`

Subsequent runs are instant.

### Credits

Fetchit project's use dependencies from community such as [yt-dlp](github.com/yt-dlp/yt-dlp) and [ffmpeg](https://git.ffmpeg.org/ffmpeg.git)

### Disclaimer

> **Educational Purpose & Legal Compliance Notice**  
> This tool was written strictly for educational and research purposes. The author will not be responsible if you use this program in bad faith. By using it, you assume full responsibility and accept all associated platform terms.
>
> - You are accepting the **Qobuz API Terms of Use** and must comply with **Spotify** and **YouTube** Terms of Service when resolving media.
> - All product names, logos, and brands (including _Qobuz, Spotify, YouTube, yt-dlp,_ and _FFmpeg_) are property of their respective owners. Their use here does not imply endorsement, affiliation, or sponsorship.
