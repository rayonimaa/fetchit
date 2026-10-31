package main

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"

	"github.com/ulikunitz/xz"
	"golang.org/x/term"
)

// ANSI color palette
var (
	colReset  = "\033[0m"
	colDim    = "\033[2m"
	colBold   = "\033[1m"
	colAccent = "\033[38;5;39m"
	colGreen  = "\033[38;5;42m"
	colAmber  = "\033[38;5;214m"
	colRed    = "\033[38;5;203m"
	colGutter = "\033[38;5;240m"
	colText   = "\033[38;5;252m"
	colMuted  = "\033[38;5;245m"
)

var (
	isTerminalStdout bool
	isTerminalStdin  bool
	spinnerChars     = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}
)

func initColors() {
	isTerminalStdout = term.IsTerminal(int(os.Stdout.Fd()))
	isTerminalStdin = term.IsTerminal(int(os.Stdin.Fd()))

	noColor := os.Getenv("NO_COLOR") != ""
	termEnv := os.Getenv("TERM")

	if !isTerminalStdout || noColor || termEnv == "dumb" {
		colReset = ""
		colDim = ""
		colBold = ""
		colAccent = ""
		colGreen = ""
		colAmber = ""
		colRed = ""
		colGutter = ""
		colText = ""
		colMuted = ""
	}
}

// runeWidth returns 2 for East Asian Wide/Fullwidth runes, 0 for combining marks, 1 otherwise.
func runeWidth(r rune) int {
	if unicode.Is(unicode.Mn, r) || unicode.Is(unicode.Me, r) || unicode.Is(unicode.Cc, r) || unicode.Is(unicode.Cf, r) {
		return 0
	}
	// Broad East Asian ranges: CJK Unified Ideographs, Hangul, Fullwidth ASCII, etc.
	if (r >= 0x1100 && r <= 0x115F) ||
		(r >= 0x2E80 && r <= 0xA4CF && r != 0x303F) ||
		(r >= 0xAC00 && r <= 0xD7A3) ||
		(r >= 0xF900 && r <= 0xFAFF) ||
		(r >= 0xFE10 && r <= 0xFE19) ||
		(r >= 0xFE30 && r <= 0xFE6F) ||
		(r >= 0xFF00 && r <= 0xFF60) ||
		(r >= 0xFFE0 && r <= 0xFFE6) ||
		(r >= 0x20000 && r <= 0x2FFFD) ||
		(r >= 0x30000 && r <= 0x3FFFD) {
		return 2
	}
	return 1
}

func stringWidth(s string) int {
	w := 0
	for _, r := range s {
		w += runeWidth(r)
	}
	return w
}

func truncateString(s string, maxCols int) string {
	w := 0
	var res []rune
	for _, r := range s {
		rw := runeWidth(r)
		if w+rw > maxCols {
			if maxCols >= 3 && len(res) > 0 {
				// add ellipsis if room
				return string(res) + "…"
			}
			return string(res)
		}
		res = append(res, r)
		w += rw
	}
	return string(res)
}

// Log styles
func logPhase(msg string) {
	fmt.Printf("  • %s%s%s\n", colBold, msg, colReset)
}

func logSub(msg string) {
	fmt.Printf("  │ %s%s%s\n", colMuted, msg, colReset)
}

func logSubKV(label, val string) {
	paddedLabel := label
	if len(paddedLabel) < 9 {
		paddedLabel = paddedLabel + strings.Repeat(" ", 9-len(paddedLabel))
	}
	fmt.Printf("  │ %s%s %s%s%s\n", colMuted, paddedLabel, colText, val, colReset)
}

func logTool(argv string) {
	fmt.Printf("  ↳ %s%s%s\n", colDim, argv, colReset)
}

func logOK(msg string) {
	fmt.Printf("  ✓ %s%s%s\n", colText, msg, colReset)
}

func logErr(msg string) {
	fmt.Printf("  ✗ %s%s%s\n", colText, msg, colReset)
}

func logWarn(msg string) {
	fmt.Printf("  △ %s%s%s\n", colText, msg, colReset)
}

func printHeader(outDir string) {
	shortDir := outDir
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		if strings.HasPrefix(shortDir, home) {
			shortDir = "~" + strings.TrimPrefix(shortDir, home)
		}
	}

	w := 54
	title := ">_ fetchit (v1.0)"
	line1 := "engine:    yt-dlp + ffmpeg"
	line2 := "directory: " + shortDir

	maxInner := w - 4 // border takes 2 chars on each side + 1 space each: "│ " ... " │"
	if stringWidth(line2) > maxInner {
		line2 = truncateString(line2, maxInner)
	}

	pad := func(s string) string {
		sw := stringWidth(s)
		space := (w - 2) - 2 - sw
		if space < 0 {
			space = 0
		}
		return s + strings.Repeat(" ", space)
	}

	fmt.Printf("%s╭%s╮%s\n", colGutter, strings.Repeat("─", w-2), colReset)
	fmt.Printf("%s│%s %s%s%s %s│%s\n", colGutter, colReset, colBold, pad(title), colReset, colGutter, colReset)
	fmt.Printf("%s│%s %s %s│%s\n", colGutter, colReset, strings.Repeat(" ", w-4), colGutter, colReset)
	fmt.Printf("%s│%s %s%s%s %s│%s\n", colGutter, colReset, colMuted, pad(line1), colReset, colGutter, colReset)
	fmt.Printf("%s│%s %s%s%s %s│%s\n", colGutter, colReset, colMuted, pad(line2), colReset, colGutter, colReset)
	fmt.Printf("%s╰%s╯%s\n\n", colGutter, strings.Repeat("─", w-2), colReset)
}

type Config struct {
	Format    string
	Quality   string
	OutputDir string
	Verbose   bool
	Debug     bool
	Bootstrap bool
	Clean     bool
	AssumeYes bool
	DryRun    bool
	URLs      []string
}

func getBinDir() (string, error) {
	if custom := os.Getenv("FETCHIT_HOME"); custom != "" {
		return filepath.Join(custom, "bin"), nil
	}
	cacheDir, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(cacheDir, "fetchit", "bin"), nil
}

func ytDlpBinaryName() string {
	if runtime.GOOS == "windows" {
		return "yt-dlp.exe"
	}
	return "yt-dlp"
}

func ffmpegBinaryName() string {
	if runtime.GOOS == "windows" {
		return "ffmpeg.exe"
	}
	return "ffmpeg"
}

func ffprobeBinaryName() string {
	if runtime.GOOS == "windows" {
		return "ffprobe.exe"
	}
	return "ffprobe"
}

func ytDlpDownloadURL() (string, error) {
	base := "https://github.com/yt-dlp/yt-dlp/releases/latest/download/"
	switch runtime.GOOS {
	case "windows":
		return base + "yt-dlp.exe", nil
	case "linux":
		if runtime.GOARCH == "arm64" {
			return base + "yt-dlp_linux_aarch64", nil
		}
		return base + "yt-dlp_linux", nil
	case "darwin":
		return base + "yt-dlp_macos", nil
	default:
		return "", fmt.Errorf("unsupported OS: %s", runtime.GOOS)
	}
}

func ffmpegDownloadURL() (string, string, error) {
	variant := "lgpl"
	if os.Getenv("FETCHIT_GPL") == "1" {
		variant = "gpl"
	}

	switch runtime.GOOS {
	case "windows":
		url := fmt.Sprintf("https://github.com/BtbN/FFmpeg-Builds/releases/download/latest/ffmpeg-master-latest-win64-%s.zip", variant)
		return url, "zip", nil
	case "linux":
		arch := "linux64"
		if runtime.GOARCH == "arm64" {
			arch = "linuxarm64"
		}
		url := fmt.Sprintf("https://github.com/BtbN/FFmpeg-Builds/releases/download/latest/ffmpeg-master-latest-%s-%s.tar.xz", arch, variant)
		return url, "tar.xz", nil
	case "darwin":
		if runtime.GOARCH == "arm64" {
			return "https://www.osxexperts.net/ffmpeg7arm.zip", "zip", nil
		}
		return "https://evermeet.cx/ffmpeg/getrelease/zip", "zip", nil
	default:
		return "", "", fmt.Errorf("unsupported OS: %s", runtime.GOOS)
	}
}

func downloadFile(url, destPath string) error {
	resp, err := http.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d for %s", resp.StatusCode, url)
	}

	tmpFile := destPath + ".tmp"
	f, err := os.OpenFile(tmpFile, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0755)
	if err != nil {
		return err
	}
	_, err = io.Copy(f, resp.Body)
	f.Close()
	if err != nil {
		os.Remove(tmpFile)
		return err
	}
	return os.Rename(tmpFile, destPath)
}

func extractZip(zipPath, destDir string) error {
	r, err := zip.OpenReader(zipPath)
	if err != nil {
		return err
	}
	defer r.Close()

	ffmpegBin := ffmpegBinaryName()
	ffprobeBin := ffprobeBinaryName()

	for _, f := range r.File {
		base := filepath.Base(f.Name)
		if strings.EqualFold(base, ffmpegBin) || strings.EqualFold(base, ffprobeBin) ||
			base == "ffmpeg" || base == "ffprobe" {
			targetName := base
			if runtime.GOOS == "windows" && !strings.HasSuffix(strings.ToLower(targetName), ".exe") {
				targetName += ".exe"
			}
			outPath := filepath.Join(destDir, targetName)

			rc, err := f.Open()
			if err != nil {
				return err
			}
			outFile, err := os.OpenFile(outPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0755)
			if err != nil {
				rc.Close()
				return err
			}
			_, err = io.Copy(outFile, rc)
			rc.Close()
			outFile.Close()
			if err != nil {
				return err
			}
		}
	}
	return nil
}

func extractTarXz(tarXzPath, destDir string) error {
	f, err := os.Open(tarXzPath)
	if err != nil {
		return err
	}
	defer f.Close()

	xzReader, err := xz.NewReader(f)
	if err != nil {
		return err
	}

	tarReader := tar.NewReader(xzReader)
	ffmpegBin := ffmpegBinaryName()
	ffprobeBin := ffprobeBinaryName()

	for {
		header, err := tarReader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}

		base := filepath.Base(header.Name)
		if base == ffmpegBin || base == ffprobeBin || base == "ffmpeg" || base == "ffprobe" {
			targetName := base
			if runtime.GOOS == "windows" && !strings.HasSuffix(strings.ToLower(targetName), ".exe") {
				targetName += ".exe"
			}
			outPath := filepath.Join(destDir, targetName)
			outFile, err := os.OpenFile(outPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0755)
			if err != nil {
				return err
			}
			if _, err := io.Copy(outFile, tarReader); err != nil {
				outFile.Close()
				return err
			}
			outFile.Close()
		}
	}
	return nil
}

func ensureDependencies(binDir string, force bool) error {
	if force {
		_ = os.RemoveAll(binDir)
	}
	if err := os.MkdirAll(binDir, 0755); err != nil {
		return err
	}

	ytDlpPath := filepath.Join(binDir, ytDlpBinaryName())
	ffmpegPath := filepath.Join(binDir, ffmpegBinaryName())

	_, errYt := os.Stat(ytDlpPath)
	_, errFf := os.Stat(ffmpegPath)

	needYt := force || os.IsNotExist(errYt)
	needFf := force || os.IsNotExist(errFf)

	if !needYt && !needFf {
		return preflightFFmpeg(binDir)
	}

	logPhase("Bootstrapping dependencies (yt-dlp & ffmpeg)...")

	stopSpinner := make(chan struct{})
	var wgSpinner sync.WaitGroup
	wgSpinner.Add(1)

	go func() {
		defer wgSpinner.Done()
		idx := 0
		for {
			select {
			case <-stopSpinner:
				if isTerminalStdout {
					fmt.Print("\r\033[K")
				}
				return
			default:
				if isTerminalStdout {
					fmt.Printf("\r  %s%s%s fetching tools...", colAccent, spinnerChars[idx%len(spinnerChars)], colReset)
				}
				idx++
				time.Sleep(100 * time.Millisecond)
			}
		}
	}()

	var errDlYt, errDlFf error
	var wg sync.WaitGroup

	if needYt {
		wg.Add(1)
		go func() {
			defer wg.Done()
			url, err := ytDlpDownloadURL()
			if err != nil {
				errDlYt = err
				return
			}
			errDlYt = downloadFile(url, ytDlpPath)
			if errDlYt == nil {
				_ = os.Chmod(ytDlpPath, 0755)
			}
		}()
	}

	if needFf {
		wg.Add(1)
		go func() {
			defer wg.Done()
			url, archiveType, err := ffmpegDownloadURL()
			if err != nil {
				errDlFf = err
				return
			}
			archiveExt := ".zip"
			if archiveType == "tar.xz" {
				archiveExt = ".tar.xz"
			}
			tmpArchive := filepath.Join(binDir, "ffmpeg_archive"+archiveExt)
			if err := downloadFile(url, tmpArchive); err != nil {
				errDlFf = err
				return
			}
			defer os.Remove(tmpArchive)

			if archiveType == "tar.xz" {
				errDlFf = extractTarXz(tmpArchive, binDir)
			} else {
				errDlFf = extractZip(tmpArchive, binDir)
			}
			if errDlFf == nil {
				_ = os.Chmod(filepath.Join(binDir, ffmpegBinaryName()), 0755)
				_ = os.Chmod(filepath.Join(binDir, ffprobeBinaryName()), 0755)
			}
		}()
	}

	wg.Wait()
	close(stopSpinner)
	wgSpinner.Wait()

	if errDlYt != nil {
		return fmt.Errorf("failed to download yt-dlp: %w", errDlYt)
	}
	if errDlFf != nil {
		return fmt.Errorf("failed to download ffmpeg: %w", errDlFf)
	}

	logOK("yt-dlp ready")
	logOK("ffmpeg ready")

	return preflightFFmpeg(binDir)
}

func preflightFFmpeg(binDir string) error {
	markerPath := filepath.Join(binDir, ".ffmpeg_ok")
	if _, err := os.Stat(markerPath); err == nil {
		return nil
	}

	ffmpegPath := filepath.Join(binDir, ffmpegBinaryName())
	cmd := exec.Command(ffmpegPath, "-hide_banner", "-encoders")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("failed to run ffmpeg encoders preflight: %w", err)
	}

	outStr := string(out)
	if !strings.Contains(outStr, "aac") && !strings.Contains(outStr, "libmp3lame") {
		return errors.New("ffmpeg appears broken (no aac/libmp3lame encoders)")
	}

	_ = os.WriteFile(markerPath, []byte("ok"), 0644)
	return nil
}

func isAudioFormat(fmtStr string) bool {
	switch strings.ToLower(fmtStr) {
	case "mp3", "m4a", "opus", "flac", "wav", "aac":
		return true
	default:
		return false
	}
}

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

func checkIsPlaylist(binDir, url string, verbose bool) bool {
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

type ResolveInfo struct {
	Title         string
	Duration      string
	Channel       string
	PlaylistTitle string
}

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

func drawProgressBar(spinnerIdx int, pct float64, speed, eta, status string) {
	if !isTerminalStdout {
		return
	}
	totalBars := 20
	filled := int(float64(totalBars) * (pct / 100.0))
	if filled > totalBars {
		filled = totalBars
	}
	if filled < 0 {
		filled = 0
	}
	empty := totalBars - filled

	bar := strings.Repeat("█", filled) + strings.Repeat("░", empty)
	sp := spinnerChars[spinnerIdx%len(spinnerChars)]

	pctStr := fmt.Sprintf("%3.0f%%", pct)
	if pct < 0 {
		pctStr = " --%"
	}
	if speed == "" {
		speed = "--.-KiB/s"
	}
	if eta == "" {
		eta = "--:--"
	}
	if status == "" {
		status = "fetching media…"
	}

	// format: "  ⠙  ███████░░░░░░░░░░░░░   35%  · 2.4MiB/s  · eta 00:08  │ fetching media…"
	line := fmt.Sprintf("\r\033[K  %s%s%s  %s%s%s   %s  · %s  · eta %s  %s│%s %s%s%s",
		colAccent, sp, colReset,
		colAccent, bar, colReset,
		pctStr, speed, eta,
		colGutter, colReset,
		colMuted, status, colReset)
	fmt.Print(line)
}

func clearLine() {
	if isTerminalStdout {
		fmt.Print("\r\033[K")
	}
}

func parsePercent(s string) float64 {
	s = strings.TrimSpace(s)
	s = strings.TrimSuffix(s, "%")
	val, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return -1
	}
	return val
}

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

	var outputTemplate string
	if isPL {
		outputTemplate = filepath.Join(cfg.OutputDir, "%(playlist_title|Playlist)s", "%(playlist_index)s - %(title)s.%(ext)s")
	} else {
		outputTemplate = filepath.Join(cfg.OutputDir, "%(title)s.%(ext)s")
	}

	args := []string{
		"--newline",
		"--ffmpeg-location", binDir,
		"-o", outputTemplate,
		"--print-to-file", "after_move:filepath", tmpFilePath,
	}

	if isPL {
		args = append(args, "--yes-playlist")
	} else {
		args = append(args, "--no-playlist")
	}

	formatLower := strings.ToLower(cfg.Format)
	if isAudioFormat(formatLower) {
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
	} else {
		// Video
		formatStr := buildVideoFormatString(formatLower, cfg.Quality)
		args = append(args, "-f", formatStr, "--merge-output-format", formatLower)
	}

	// Metadata
	args = append(args, "--embed-metadata")
	if formatLower != "wav" {
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
		for _, l := range lines {
			l = strings.TrimSpace(l)
			if l != "" {
				if _, err := os.Stat(l); err == nil {
					savedFiles = append(savedFiles, l)
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

// Interactive Wizard
func runWizardMenu(prompt string, options []string) (string, error) {
	if !isTerminalStdin || !isTerminalStdout {
		fmt.Printf("  • %s\n", prompt)
		for i, opt := range options {
			fmt.Printf("    %d) %s\n", i+1, opt)
		}
		fmt.Print("  Choice [1]: ")
		var input string
		fmt.Scanln(&input)
		input = strings.TrimSpace(input)
		idx := 1
		if input != "" {
			if v, err := strconv.Atoi(input); err == nil && v >= 1 && v <= len(options) {
				idx = v
			}
		}
		res := options[idx-1]
		logOK(fmt.Sprintf("%s: %s", prompt, res))
		return res, nil
	}

	// TTY Raw Mode Arrow Menu
	oldState, err := term.MakeRaw(int(os.Stdin.Fd()))
	if err != nil {
		return options[0], err
	}
	defer term.Restore(int(os.Stdin.Fd()), oldState)

	selectedIndex := 0
	numOptions := len(options)

	draw := func() {
		// Output raw newlines: \r\n
		out := fmt.Sprintf("\r  • %s%s%s\r\n", colBold, prompt, colReset)
		for i, opt := range options {
			if i == selectedIndex {
				out += fmt.Sprintf("\r    %s▸ %s%s\r\n", colAccent, opt, colReset)
			} else {
				out += fmt.Sprintf("\r    %s  %s%s\r\n", colMuted, opt, colReset)
			}
		}
		os.Stdout.WriteString(out)
	}

	clearMenu := func() {
		// Move cursor up (numOptions + 1) lines and clear each
		linesToClear := numOptions + 1
		for i := 0; i < linesToClear; i++ {
			os.Stdout.WriteString("\033[A\r\033[K")
		}
	}

	draw()

	buf := make([]byte, 16)
	for {
		n, err := os.Stdin.Read(buf)
		if err != nil {
			return options[selectedIndex], err
		}
		if n == 1 {
			switch buf[0] {
			case 3: // Ctrl+C
				clearMenu()
				return "", errors.New("interrupted")
			case 13, 10: // Enter
				clearMenu()
				term.Restore(int(os.Stdin.Fd()), oldState)
				logOK(fmt.Sprintf("%s: %s", prompt, options[selectedIndex]))
				return options[selectedIndex], nil
			}
		} else if n >= 3 && buf[0] == 27 && buf[1] == 91 {
			// Escape sequence \033[
			switch buf[2] {
			case 'A': // Up
				selectedIndex = (selectedIndex - 1 + numOptions) % numOptions
			case 'B': // Down
				selectedIndex = (selectedIndex + 1) % numOptions
			}
			clearMenu()
			draw()
		}
	}
}

func runWizardText(prompt, defaultValue string) (string, error) {
	if defaultValue != "" {
		fmt.Printf("  • %s [%s]: ", prompt, defaultValue)
	} else {
		fmt.Printf("  • %s: ", prompt)
	}
	reader := bufio.NewReader(os.Stdin)
	text, err := reader.ReadString('\n')
	if err != nil && len(text) == 0 {
		return defaultValue, err
	}
	text = strings.TrimSpace(text)
	if text == "" {
		text = defaultValue
	}
	logOK(fmt.Sprintf("%s: %s", prompt, text))
	return text, nil
}

func interactiveWizard(cfg *Config) error {
	logPhase("Interactive Setup")

	mode, err := runWizardMenu("Select media type", []string{"video", "audio"})
	if err != nil {
		return err
	}

	if mode == "video" {
		fmtChoice, err := runWizardMenu("Select format", []string{"mp4", "mkv", "webm", "mov"})
		if err != nil {
			return err
		}
		cfg.Format = fmtChoice

		qChoice, err := runWizardMenu("Select quality", []string{"best", "8k", "4k", "2k", "1440", "1080", "720", "480", "360"})
		if err != nil {
			return err
		}
		cfg.Quality = qChoice
	} else {
		fmtChoice, err := runWizardMenu("Select format", []string{"mp3", "m4a", "opus", "flac", "wav", "aac"})
		if err != nil {
			return err
		}
		cfg.Format = fmtChoice

		if fmtChoice == "flac" || fmtChoice == "wav" {
			cfg.Quality = "best"
		} else {
			qChoice, err := runWizardMenu("Select quality (kbps)", []string{"best", "320", "256", "192", "128"})
			if err != nil {
				return err
			}
			cfg.Quality = qChoice
		}
	}

	dirChoice, err := runWizardText("Save directory", cfg.OutputDir)
	if err != nil {
		return err
	}
	cfg.OutputDir = dirChoice

	urlChoice, err := runWizardText("YouTube URL", "")
	if err != nil {
		return err
	}
	if urlChoice == "" {
		return errors.New("no URL provided")
	}
	cfg.URLs = []string{urlChoice}
	return nil
}

func printHelp() {
	home, _ := os.UserHomeDir()
	defaultDir := filepath.Join(home, "Downloads")
	printHeader(defaultDir)

	fmt.Println(`Usage:
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

No URLs → interactive wizard (arrow-key menus).`)
}

func main() {
	initColors()

	homeDir, _ := os.UserHomeDir()
	defaultDownloads := filepath.Join(homeDir, "Downloads")

	cfg := &Config{
		Format:    "mp4",
		Quality:   "best",
		OutputDir: defaultDownloads,
	}

	var showHelp bool
	var showVersion bool
	var mp3Flag bool
	var mp4Flag bool

	fs := flag.NewFlagSet("fetchit", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	fs.StringVar(&cfg.Format, "f", cfg.Format, "")
	fs.StringVar(&cfg.Format, "format", cfg.Format, "")
	fs.StringVar(&cfg.Quality, "q", cfg.Quality, "")
	fs.StringVar(&cfg.Quality, "quality", cfg.Quality, "")
	fs.StringVar(&cfg.OutputDir, "o", cfg.OutputDir, "")
	fs.StringVar(&cfg.OutputDir, "output", cfg.OutputDir, "")
	fs.BoolVar(&cfg.Verbose, "V", false, "")
	fs.BoolVar(&cfg.Verbose, "verbose", false, "")
	fs.BoolVar(&cfg.Debug, "debug", false, "")
	fs.BoolVar(&cfg.Bootstrap, "bootstrap", false, "")
	fs.BoolVar(&cfg.Clean, "clean", false, "")
	fs.BoolVar(&cfg.AssumeYes, "y", false, "")
	fs.BoolVar(&cfg.AssumeYes, "yes", false, "")
	fs.BoolVar(&mp3Flag, "m", false, "")
	fs.BoolVar(&mp3Flag, "mp3", false, "")
	fs.BoolVar(&mp4Flag, "v", false, "")
	fs.BoolVar(&mp4Flag, "mp4", false, "")
	fs.BoolVar(&cfg.DryRun, "n", false, "")
	fs.BoolVar(&cfg.DryRun, "dry-run", false, "")
	fs.BoolVar(&showHelp, "h", false, "")
	fs.BoolVar(&showHelp, "help", false, "")
	fs.BoolVar(&showVersion, "version", false, "")

	if err := fs.Parse(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	if showVersion {
		fmt.Println("fetchit 1.0")
		return
	}

	if showHelp {
		printHelp()
		return
	}

	if mp3Flag {
		cfg.Format = "mp3"
	} else if mp4Flag {
		cfg.Format = "mp4"
	}

	if cfg.Debug {
		cfg.Verbose = true
	}

	binDir, err := getBinDir()
	if err != nil {
		logErr(fmt.Sprintf("could not determine cache dir: %v", err))
		os.Exit(1)
	}

	if cfg.Clean {
		cacheRoot := filepath.Dir(binDir)
		logPhase(fmt.Sprintf("Cleaning cache directory: %s", cacheRoot))
		if err := os.RemoveAll(cacheRoot); err != nil {
			logErr(fmt.Sprintf("failed to clean cache: %v", err))
			os.Exit(1)
		}
		logOK("Cache directory removed")
		return
	}

	printHeader(cfg.OutputDir)

	if err := ensureDependencies(binDir, cfg.Bootstrap); err != nil {
		logErr(fmt.Sprintf("bootstrap failed: %v", err))
		os.Exit(1)
	}

	if cfg.Bootstrap && len(fs.Args()) == 0 {
		return
	}

	cfg.URLs = fs.Args()
	if len(cfg.URLs) == 0 {
		if err := interactiveWizard(cfg); err != nil {
			logErr(fmt.Sprintf("wizard failed: %v", err))
			os.Exit(1)
		}
	}

	_ = os.MkdirAll(cfg.OutputDir, 0755)

	startTime := time.Now()
	var allSavedFiles []string
	failedCount := 0

	for idx, url := range cfg.URLs {
		logPhase(fmt.Sprintf("[%d/%d] Resolving %s", idx+1, len(cfg.URLs), url))

		info, err := resolveMedia(binDir, url, cfg.Debug)
		if err != nil {
			logWarn(fmt.Sprintf("could not resolve %s: %v", url, err))
			failedCount++
			continue
		}

		if info.Title != "" {
			logSubKV("title", truncateString(info.Title, 40))
		}
		if info.Duration != "" && info.Duration != "NA" {
			logSubKV("duration", info.Duration)
		}
		if info.Channel != "" && info.Channel != "unknown" {
			logSubKV("channel", info.Channel)
		}

		saved, dlErr := runDownload(cfg, binDir, url)
		if dlErr != nil {
			failedCount++
		}
		allSavedFiles = append(allSavedFiles, saved...)
	}

	// Calculate total size
	var totalBytes int64
	for _, f := range allSavedFiles {
		if fi, err := os.Stat(f); err == nil {
			totalBytes += fi.Size()
		}
	}
	totalMiB := float64(totalBytes) / (1024 * 1024)
	elapsedSec := int(time.Since(startTime).Seconds())

	fmt.Printf("\n%s%s%s\n", colGutter, strings.Repeat("─", 54), colReset)
	failMsg := ""
	if failedCount > 0 {
		failMsg = fmt.Sprintf(" · %s%d failed%s", colRed, failedCount, colReset)
	}
	fmt.Printf("  %d files · %.1f MiB · %ds%s\n\n", len(allSavedFiles), totalMiB, elapsedSec, failMsg)

	// Post-download relocation prompt
	if !cfg.AssumeYes && isTerminalStdin && len(allSavedFiles) > 0 {
		fmt.Printf("Save to [%s]: ", cfg.OutputDir)
		reader := bufio.NewReader(os.Stdin)
		newDir, _ := reader.ReadString('\n')
		newDir = strings.TrimSpace(newDir)

		if newDir != "" && filepath.Clean(newDir) != filepath.Clean(cfg.OutputDir) {
			_ = os.MkdirAll(newDir, 0755)
			movedCount := 0
			for _, src := range allSavedFiles {
				rel, err := filepath.Rel(cfg.OutputDir, src)
				if err != nil {
					rel = filepath.Base(src)
				}
				dest := filepath.Join(newDir, rel)
				_ = os.MkdirAll(filepath.Dir(dest), 0755)
				if err := os.Rename(src, dest); err == nil {
					movedCount++
				}
			}
			logOK(fmt.Sprintf("moved %d files → %s", movedCount, newDir))
		}

		fmt.Print("\nPress Enter to exit…")
		_, _ = reader.ReadString('\n')
	}
}
