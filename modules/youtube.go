package modules

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"
)

// isAudioFormat checks if the requested format is an audio container.
func isAudioFormat(fmtStr string) bool {
	switch strings.ToLower(fmtStr) {
	case "mp3", "m4a", "opus", "flac", "wav", "aac":
		return true
	default:
		return false
	}
}

// buildVideoFormatString generates the yt-dlp video format selector according to requested container and quality.
func buildVideoFormatString(fmtStr, quality string) string {
	f := strings.ToLower(fmtStr)
	var heightFilter string
	q := strings.ToLower(quality)
	switch q {
	case "best", "":
		heightFilter = ""
	case "8k":
		heightFilter = "[height<=4320]"
	case "4k":
		heightFilter = "[height<=2160]"
	case "2k", "1440":
		heightFilter = "[height<=1440]"
	default:
		num := ""
		for _, r := range q {
			if unicode.IsDigit(r) {
				num += string(r)
			}
		}
		if num != "" {
			heightFilter = fmt.Sprintf("[height<=%s]", num)
		}
	}

	vcodec := ""
	acodec := ""
	switch f {
	case "mp4":
		vcodec = "[vcodec^=avc1]"
		acodec = "[acodec^=mp4a]"
	case "webm":
		vcodec = "[vcodec^=vp9]"
		acodec = "[acodec^=opus]"
	}

	if vcodec != "" {
		return fmt.Sprintf("bestvideo%s%s+bestaudio%s/bestvideo%s+bestaudio/best%s/best",
			heightFilter, vcodec, acodec, heightFilter, heightFilter)
	}
	return fmt.Sprintf("bestvideo%s+bestaudio/best%s/best", heightFilter, heightFilter)
}

// checkIsPlaylist checks whether the specified URL points to a playlist with more than one item.
func checkIsPlaylist(binDir, url string, verbose bool) bool {
	lower := strings.ToLower(url)
	if !strings.Contains(lower, "list=") && !strings.Contains(lower, "/playlist") && !strings.Contains(lower, "/album") && !strings.Contains(lower, "/sets/") {
		return false
	}

	ytDlp := filepath.Join(binDir, ytDlpBinaryName())
	cmd := exec.Command(ytDlp, "--flat-playlist", "--skip-download", "--print", "%(playlist_count)s", url)
	out, err := cmd.Output()
	if err != nil {
		return false
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	for _, l := range lines {
		l = strings.TrimSpace(l)
		if count, err := strconv.Atoi(l); err == nil && count > 1 {
			return true
		}
	}
	return false
}

// ResolveInfo contains metadata resolved for a video or playlist.
type ResolveInfo struct {
	Title         string
	Duration      string
	Channel       string
	PlaylistTitle string
}

// resolveMedia fetches metadata from yt-dlp without downloading the media.
func resolveMedia(binDir, url string, debug bool) (*ResolveInfo, error) {
	ytDlp := filepath.Join(binDir, ytDlpBinaryName())
	args := []string{
		"--no-warnings",
		"--skip-download",
		"--ffmpeg-location", binDir,
		"--print", "%(title)s",
		"--print", "%(duration_string)s",
		"--print", "%(uploader,channel|unknown)s",
		"--print", "%(playlist_title)s",
		url,
	}
	cmd := exec.Command(ytDlp, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		errMsg := strings.TrimSpace(stderr.String())
		if errMsg == "" {
			errMsg = err.Error()
		}
		if debug {
			logErr(fmt.Sprintf("resolve failed stderr dump:\n%s", errMsg))
		}
		return nil, errors.New(errMsg)
	}

	lines := strings.Split(string(out), "\n")
	info := &ResolveInfo{}
	if len(lines) > 0 {
		info.Title = strings.TrimSpace(lines[0])
	}
	if len(lines) > 1 {
		info.Duration = strings.TrimSpace(lines[1])
	}
	if len(lines) > 2 {
		info.Channel = strings.TrimSpace(lines[2])
	}
	if len(lines) > 3 {
		info.PlaylistTitle = strings.TrimSpace(lines[3])
	}
	return info, nil
}

// buildYtdlpOutputTemplate constructs a yt-dlp -o template string that
// honours the user's NamingOpts (artist / album / title / numbering).
//
// Uses yt-dlp conditional field formatting %(field&PREFIX%sSUFFIX|)s together with
// --output-na-placeholder "" so that absent metadata fields omit their separators cleanly
// and never leave trailing/leading/double " - " artifacts.
func buildYtdlpOutputTemplate(opts RenameOptions, isPL bool, outDir string) string {
	var b strings.Builder
	if isPL && opts.IncludeNumbering {
		b.WriteString("%(playlist_index&%02d - |)s")
	}
	if opts.IncludeArtist {
		b.WriteString("%(uploader,channel)s%(uploader,channel& - |)s")
	}
	if opts.IncludeAlbum {
		b.WriteString("%(album)s%(album& - |)s")
	}
	if opts.IncludeTitle || (!opts.IncludeArtist && !opts.IncludeAlbum) {
		b.WriteString("%(title)s")
	}
	b.WriteString(".%(ext)s")

	name := b.String()
	if isPL {
		return filepath.Join(outDir, "%(playlist_title|Playlist)s", name)
	}
	return filepath.Join(outDir, name)
}

// cleanupIntermediateFiles removes any leftover intermediate/pre-converted media files
// (e.g. .part, temporary webm/mp4 from audio extraction, unmerged streams)
// associated with the downloaded file.
func cleanupIntermediateFiles(finalPath string) {
	if finalPath == "" {
		return
	}
	dir := filepath.Dir(finalPath)
	base := filepath.Base(finalPath)
	ext := filepath.Ext(finalPath)
	stem := strings.TrimSuffix(base, ext)

	// 1. Clean up .part / .ytdl appended to the full filename (e.g. "Song.mp3.part")
	for _, suffix := range []string{".part", ".ytdl"} {
		partCandidate := filepath.Join(dir, base+suffix)
		if fi, err := os.Stat(partCandidate); err == nil && !fi.IsDir() {
			_ = os.Remove(partCandidate)
		}
	}

	// 2. Clean up intermediate formats sharing the same stem (e.g. "Song.webm", "Song.part")
	intermediateExts := []string{
		".part", ".ytdl",
		".webm", ".mp4", ".m4a", ".opus", ".ogg",
		".temp.mp4", ".temp.webm", ".temp.m4a",
		".jpg", ".webp", ".png",
	}

	for _, ie := range intermediateExts {
		if strings.EqualFold(ext, ie) {
			continue // Do not delete the target file itself
		}
		candidate := filepath.Join(dir, stem+ie)
		if fi, err := os.Stat(candidate); err == nil && !fi.IsDir() {
			_ = os.Remove(candidate)
		}
	}
}

// runDownload downloads media from a YouTube or generic yt-dlp supported URL.
func runDownload(cfg *Config, binDir, url string) ([]string, error) {
	ytDlp := filepath.Join(binDir, ytDlpBinaryName())

	isPL := checkIsPlaylist(binDir, url, cfg.Verbose)

	tmpFileList, err := os.CreateTemp("", "fetchit-files-*.txt")
	if err != nil {
		return nil, err
	}
	tmpFilePath := tmpFileList.Name()
	tmpFileList.Close()
	defer os.Remove(tmpFilePath)

	// Respect user's NamingOpts for the output template
	namingOpts := cfg.NamingOpts
	namingOpts.IsPlaylist = isPL
	outputTemplate := buildYtdlpOutputTemplate(namingOpts, isPL, cfg.OutputDir)

	args := []string{
		"--newline",
		"--ffmpeg-location", binDir,
		"--output-na-placeholder", "",
		"--no-keep-video",
		"--no-write-thumbnail",
		"-o", outputTemplate,
		"--print-to-file", "after_move:filepath", tmpFilePath,
	}

	if isPL {
		args = append(args, "--yes-playlist")
	} else {
		args = append(args, "--no-playlist")
	}

	formatLower := strings.ToLower(cfg.Format)

	// "original" format: pass raw yt-dlp selector strings set by the wizard, no conversion.
	isOriginalVideo := formatLower == "bestvideo+bestaudio/best"
	isOriginalAudio := formatLower == "bestaudio/best"

	switch {
	case isOriginalVideo:
		args = append(args, "-f", "bestvideo+bestaudio/best")
	case isOriginalAudio:
		args = append(args, "-f", "bestaudio/best", "--extract-audio")
	case isAudioFormat(formatLower):
		args = append(args, "--extract-audio", "--audio-format", formatLower)
		qLower := strings.ToLower(cfg.Quality)
		if formatLower != "flac" && formatLower != "wav" {
			if qLower == "best" || qLower == "" {
				args = append(args, "--audio-quality", "0")
			} else {
				kbps := strings.TrimSuffix(qLower, "k")
				args = append(args, "--audio-quality", kbps+"K")
			}
		}
	default:
		// Standard video with format conversion
		formatStr := buildVideoFormatString(formatLower, cfg.Quality)
		args = append(args, "-f", formatStr, "--merge-output-format", formatLower)
	}

	// Metadata
	args = append(args, "--embed-metadata")
	if formatLower != "wav" && !isOriginalVideo && !isOriginalAudio {
		args = append(args, "--embed-thumbnail")
	}

	// Progress template or non-tty
	if !isTerminalStdout || cfg.Verbose {
		if !isTerminalStdout {
			args = append(args, "--no-progress")
		}
	} else {
		args = append(args, "--progress-template", "download:PROGRESS:%(progress._percent_str)s|%(progress._speed_str)s|%(progress._eta_str)s")
	}

	args = append(args, url)

	if cfg.Verbose {
		logTool(fmt.Sprintf("%s %s", ytDlp, strings.Join(args, " ")))
	}

	if cfg.DryRun {
		logSubKV("dry-run", fmt.Sprintf("%s %s", ytDlp, strings.Join(args, " ")))
		return nil, nil
	}

	cmd := exec.Command(ytDlp, args...)

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}

	if err := cmd.Start(); err != nil {
		return nil, err
	}

	var spinnerIdx int32
	var lastPct float64 = -1
	var lastSpeed, lastEta, lastStatus string
	lastStatus = "fetching media…"

	stopTicker := make(chan struct{})
	var tickerWG sync.WaitGroup
	if isTerminalStdout && !cfg.Verbose {
		tickerWG.Add(1)
		go func() {
			defer tickerWG.Done()
			ticker := time.NewTicker(100 * time.Millisecond)
			defer ticker.Stop()
			for {
				select {
				case <-stopTicker:
					return
				case <-ticker.C:
					idx := atomic.AddInt32(&spinnerIdx, 1)
					drawProgressBar(int(idx), lastPct, lastSpeed, lastEta, lastStatus)
				}
			}
		}()
	}

	processLine := func(line string) {
		line = strings.TrimSpace(line)
		if line == "" {
			return
		}

		if strings.HasPrefix(line, "PROGRESS:") {
			parts := strings.Split(strings.TrimPrefix(line, "PROGRESS:"), "|")
			if len(parts) >= 3 {
				lastPct = parsePercent(parts[0])
				lastSpeed = strings.TrimSpace(parts[1])
				lastEta = strings.TrimSpace(parts[2])
			}
			return
		}

		if strings.HasPrefix(line, "ERROR:") || strings.HasPrefix(line, "[error]") {
			clearLine()
			logErr(line)
			return
		}
		if strings.HasPrefix(line, "WARNING:") || strings.HasPrefix(line, "[warning]") {
			if strings.Contains(line, "JavaScript runtime") {
				return
			}
			clearLine()
			logWarn(line)
			return
		}

		switch {
		case strings.HasPrefix(line, "[download] Destination:"):
			lastStatus = "saving segments…"
		case strings.HasPrefix(line, "[Merger]"):
			lastStatus = "merging streams…"
		case strings.HasPrefix(line, "[ExtractAudio]"):
			lastStatus = "extracting audio…"
		case strings.HasPrefix(line, "[VideoConvertor]"):
			lastStatus = "converting format…"
		case strings.HasPrefix(line, "[EmbedThumbnail]"):
			lastStatus = "embedding cover art…"
		case strings.HasPrefix(line, "[Metadata]"):
			lastStatus = "writing metadata…"
		case strings.HasPrefix(line, "[MoveFiles]"):
			lastStatus = "moving final file…"
		case strings.HasPrefix(line, "[FixupM3u8]"):
			lastStatus = "fixing m3u8…"
		case strings.HasPrefix(line, "[ThumbnailsConvertor]"):
			lastStatus = "converting cover…"
		default:
			if cfg.Verbose {
				logSub(line)
			}
		}
	}

	go func() {
		scanner := bufio.NewScanner(stderrPipe)
		for scanner.Scan() {
			processLine(scanner.Text())
		}
	}()

	scanner := bufio.NewScanner(stdoutPipe)
	for scanner.Scan() {
		processLine(scanner.Text())
	}

	waitErr := cmd.Wait()
	close(stopTicker)
	tickerWG.Wait()
	clearLine()

	// Read registered downloaded files
	var savedFiles []string
	if data, err := os.ReadFile(tmpFilePath); err == nil {
		lines := strings.Split(string(data), "\n")
		seen := make(map[string]bool)
		for _, l := range lines {
			l = strings.TrimSpace(l)
			if l != "" && !seen[l] {
				if _, err := os.Stat(l); err == nil {
					savedFiles = append(savedFiles, l)
					seen[l] = true
					cleanupIntermediateFiles(l)
				}
			}
		}
	}

	if waitErr != nil {
		if len(savedFiles) > 0 {
			logWarn(fmt.Sprintf("partial: %d file(s) saved", len(savedFiles)))
		}
		return savedFiles, waitErr
	}

	return savedFiles, nil
}

var (
	CheckIsPlaylist          = checkIsPlaylist
	ResolveMedia             = resolveMedia
	BuildYtdlpOutputTemplate = buildYtdlpOutputTemplate
	CleanupIntermediateFiles = cleanupIntermediateFiles
	RunDownload              = runDownload
)

