package main

import (
	"archive/tar"
	"archive/zip"
	"bufio"
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
	"time"
	"unicode"
	"unicode/utf8"

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
	title := ">_ fetchit"
	line1 := "v1.0 stable"
	line2 := "customized backend-engine"

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

// ---------------------------------------------------------------------------
// Dynamic Naming Engine
// ---------------------------------------------------------------------------

// FileMeta menyimpan data mentah lagu yang ditarik dari metadatanya
type FileMeta struct {
	Title       string
	Artist      string
	Album       string
	TrackNumber int
	Extension   string // contoh: ".mp3", "mp3", atau ".%(ext)s"
}

// RenameOptions menyimpan status konfigurasi naming milikmu
type RenameOptions struct {
	IncludeArtist    bool
	IncludeAlbum     bool
	IncludeTitle     bool
	IsPlaylist       bool
	IncludeNumbering bool
}

// DefaultRenameOptions mengembalikan konfigurasi default yang direkomendasikan
func DefaultRenameOptions(isPlaylist bool) RenameOptions {
	return RenameOptions{
		IncludeArtist:    true,
		IncludeAlbum:     false,
		IncludeTitle:     true,
		IsPlaylist:       isPlaylist,
		IncludeNumbering: isPlaylist,
	}
}

// Windows-reserved device names (case-insensitive base name)
var reservedDeviceNames = map[string]bool{
	"CON": true, "PRN": true, "AUX": true, "NUL": true,
	"COM1": true, "COM2": true, "COM3": true, "COM4": true,
	"COM5": true, "COM6": true, "COM7": true, "COM8": true, "COM9": true,
	"LPT1": true, "LPT2": true, "LPT3": true, "LPT4": true,
	"LPT5": true, "LPT6": true, "LPT7": true, "LPT8": true, "LPT9": true,
}

// sanitizeBaseName membersihkan karakter ilegal lintas OS (Windows, Linux, macOS),
// menghapus karakter kontrol tersembunyi, menormalkan spasi,
// menghapus trailing dot/space yang dilarang Windows, dan menangani nama DOS reserved.
func sanitizeBaseName(name string) string {
	// 1. Bersihkan karakter terlarang OS: < > : " / \ | ? *
	invalidChars := []string{"<", ">", ":", "\"", "/", "\\", "|", "?", "*"}
	clean := name
	for _, ch := range invalidChars {
		clean = strings.ReplaceAll(clean, ch, "_")
	}

	// 2. Bersihkan karakter kontrol non-printable (0x00 - 0x1f, 0x7f)
	var b strings.Builder
	for _, r := range clean {
		if r < 32 || r == 127 {
			continue
		}
		b.WriteRune(r)
	}
	clean = b.String()

	// 3. Normalisasi spasi dan underscore yang bertumpuk
	clean = strings.Join(strings.Fields(clean), " ")

	// 4. Hapus spasi dan titik di akhir nama (Windows melarang trailing dot/space)
	clean = strings.TrimRight(clean, " ._")
	clean = strings.TrimLeft(clean, " ")

	// 5. Fallback pengamanan jika nama kosong setelah disanitasi
	if clean == "" {
		clean = "Track"
	}

	// 6. Tangani nama file reserved Windows (CON, PRN, AUX, NUL, COM1-9, LPT1-9)
	upperBase := strings.ToUpper(clean)
	if reservedDeviceNames[upperBase] {
		clean = "_" + clean
	}

	// 7. Batasi panjang maksimal (240 bytes) agar aman di batas 255 bytes Linux/Windows
	if len(clean) > 240 {
		for len(clean) > 240 {
			_, size := utf8.DecodeLastRuneInString(clean)
			if size == 0 {
				break
			}
			clean = clean[:len(clean)-size]
		}
		clean = strings.TrimRight(clean, " ._")
	}

	return clean
}

var (
	// ErrNoNamerInput indicates that FileMeta contained no usable title, artist, or album.
	ErrNoNamerInput = errors.New("custom namer received no metadata input")
	// ErrNoNamingFields indicates that all naming field options (artist, album, title) were false.
	ErrNoNamingFields = errors.New("custom namer received no active naming fields")
)

// generateFilename akan merakit nama file secara dinamis sesuai konfigurasi
func generateFilename(meta FileMeta, opts RenameOptions) (string, error) {
	var namerErr error

	// Fallback pengamanan jika tidak mencentang apa pun
	if !opts.IncludeTitle && !opts.IncludeArtist && !opts.IncludeAlbum {
		opts.IncludeTitle = true
		namerErr = ErrNoNamingFields
	}

	// Rapikan setiap input metadata
	artist := strings.TrimSpace(meta.Artist)
	album := strings.TrimSpace(meta.Album)
	title := strings.TrimSpace(meta.Title)

	var parts []string
	// Urutan: Artist - Album - Title
	if opts.IncludeArtist && artist != "" {
		parts = append(parts, artist)
	}
	if opts.IncludeAlbum && album != "" {
		parts = append(parts, album)
	}
	if opts.IncludeTitle && title != "" {
		parts = append(parts, title)
	}

	var baseName string
	if len(parts) == 0 {
		namerErr = ErrNoNamerInput
		if meta.TrackNumber > 0 {
			baseName = fmt.Sprintf("Track %d", meta.TrackNumber)
		} else {
			baseName = "Track"
		}
	} else {
		// Gabungkan bagian-bagian yang terpilih
		baseName = strings.Join(parts, " - ")
	}

	// Jika ini playlist dan kamu mencentang penomoran di depan
	if opts.IsPlaylist && opts.IncludeNumbering && meta.TrackNumber > 0 {
		// Format dua digit (01, 02, dst)
		baseName = fmt.Sprintf("%02d - %s", meta.TrackNumber, baseName)
	}

	// Sanitasi: Bersihkan karakter yang dilarang oleh OS
	baseName = sanitizeBaseName(baseName)

	// Format ekstensi dengan titik pemisah jika belum ada
	ext := strings.TrimSpace(meta.Extension)
	if ext != "" && !strings.HasPrefix(ext, ".") {
		ext = "." + ext
	}

	return baseName + ext, namerErr
}

// resolvePath normalizes, unquotes, expands env vars and ~, and returns an absolute clean path.
func resolvePath(p string) string {
	clean := strings.TrimSpace(p)
	clean = strings.Trim(clean, "\"'")
	if clean == "" {
		return ""
	}
	clean = os.ExpandEnv(clean)

	if clean == "~" || strings.HasPrefix(clean, "~/") || strings.HasPrefix(clean, `~\`) {
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			if clean == "~" {
				clean = home
			} else {
				clean = filepath.Join(home, clean[2:])
			}
		}
	}

	if abs, err := filepath.Abs(clean); err == nil {
		return filepath.Clean(abs)
	}
	return filepath.Clean(clean)
}

// moveFile moves a file from src to dest. It attempts os.Rename first, and falls back to
// copy+remove if crossing filesystem or drive volume boundaries (common on Windows).
func moveFile(src, dest string) error {
	if err := os.Rename(src, dest); err == nil {
		return nil
	}

	srcFile, err := os.Open(src)
	if err != nil {
		return err
	}
	defer srcFile.Close()

	srcInfo, err := srcFile.Stat()
	if err != nil {
		return err
	}

	destFile, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, srcInfo.Mode())
	if err != nil {
		return err
	}

	if _, err := io.Copy(destFile, srcFile); err != nil {
		destFile.Close()
		_ = os.Remove(dest)
		return err
	}

	if err := destFile.Sync(); err != nil {
		destFile.Close()
		_ = os.Remove(dest)
		return err
	}

	if err := destFile.Close(); err != nil {
		_ = os.Remove(dest)
		return err
	}

	// Windows requires closing srcFile before deleting it
	srcFile.Close()

	if err := os.Remove(src); err != nil {
		return fmt.Errorf("copied to %s but failed to remove original %s: %w", dest, src, err)
	}

	return nil
}

// ---------------------------------------------------------------------------
// Configuration & Dependency Management
// ---------------------------------------------------------------------------

type Config struct {
	Format            string
	Quality           string
	OutputDir         string
	Verbose           bool
	Debug             bool
	Bootstrap         bool
	Clean             bool
	AssumeYes         bool
	DryRun            bool
	URLs              []string
	NamingOpts        RenameOptions
	ExplicitNumbering bool
	Country           string
	CFClearance       string
	UserAgent         string
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

// ---------------------------------------------------------------------------
// Terminal Progress UI
// ---------------------------------------------------------------------------

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

// ---------------------------------------------------------------------------
// Interactive Wizard – checkbox-style UI
// ---------------------------------------------------------------------------

// runCheckbox presents a single-select radio-style checkbox list.
// The cursor row is highlighted in accent blue with a filled [✓].
// All other rows show a dim [ ] unchecked box.
// Returns the selected option value.
func runCheckbox(prompt string, options []string, defaultIdx int) (string, error) {
	if !isTerminalStdin || !isTerminalStdout {
		fmt.Printf("  • %s\r\n", prompt)
		for i, opt := range options {
			marker := "[ ]"
			if i == defaultIdx {
				marker = "[✓]"
			}
			fmt.Printf("    %s %d) %s\r\n", marker, i+1, opt)
		}
		fmt.Print("  Choice [1]: ")
		var input string
		fmt.Scanln(&input)
		input = strings.TrimSpace(input)
		idx := defaultIdx
		if input != "" {
			if v, err := strconv.Atoi(input); err == nil && v >= 1 && v <= len(options) {
				idx = v - 1
			}
		}
		res := options[idx]
		fmt.Printf("  %s✓%s %s: %s%s%s\r\n", colGreen, colReset, prompt, colAccent, res, colReset)
		return res, nil
	}

	oldState, err := term.MakeRaw(int(os.Stdin.Fd()))
	if err != nil {
		return options[defaultIdx], err
	}
	defer term.Restore(int(os.Stdin.Fd()), oldState)

	cursor := defaultIdx
	if cursor < 0 || cursor >= len(options) {
		cursor = 0
	}
	numOptions := len(options)

	draw := func() {
		out := fmt.Sprintf("\r  %s•%s %s%s%s\r\n", colBold, colReset, colBold, prompt, colReset)
		for i, opt := range options {
			if i == cursor {
				// Checked, full accent row
				out += fmt.Sprintf("\r    %s[✓] %s%s\r\n", colAccent, opt, colReset)
			} else {
				out += fmt.Sprintf("\r    %s[ ] %s%s\r\n", colMuted, opt, colReset)
			}
		}
		os.Stdout.WriteString(out)
	}

	clearMenu := func() {
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
			return options[cursor], err
		}
		if n == 1 {
			switch buf[0] {
			case 3: // Ctrl+C
				clearMenu()
				return "", errors.New("interrupted")
			case 13, 10: // Enter
				chosen := options[cursor]
				clearMenu()
				term.Restore(int(os.Stdin.Fd()), oldState)
				fmt.Printf("  %s✓%s %s: %s%s%s\r\n", colGreen, colReset, prompt, colAccent, chosen, colReset)
				return chosen, nil
			}
		} else if n >= 3 && buf[0] == 27 && buf[1] == 91 {
			switch buf[2] {
			case 'A': // Up
				cursor = (cursor - 1 + numOptions) % numOptions
			case 'B': // Down
				cursor = (cursor + 1) % numOptions
			}
			clearMenu()
			draw()
		}
	}
}

// runWizardText reads a plain text answer from the user.
func runWizardText(prompt, defaultValue string) (string, error) {
	// Restore normal mode if in TTY (MakeRaw not active here, but just in case)
	if defaultValue != "" {
		fmt.Printf("  %s•%s %s %s[%s]%s: ", colBold, colReset, prompt, colMuted, defaultValue, colReset)
	} else {
		fmt.Printf("  %s•%s %s: ", colBold, colReset, prompt)
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
	fmt.Printf("  %s✓%s %s: %s%s%s\r\n", colGreen, colReset, prompt, colAccent, text, colReset)
	return text, nil
}

// runMultiCheckbox presents a multi-select togglable checkbox list.
// Space toggles, Enter confirms. Returns indices of checked items.
func runMultiCheckbox(prompt string, options []string, checked []bool) ([]bool, error) {
	if !isTerminalStdin || !isTerminalStdout {
		fmt.Printf("  • %s (space=toggle, enter=confirm)\r\n", prompt)
		for i, opt := range options {
			marker := "[ ]"
			if i < len(checked) && checked[i] {
				marker = "[✓]"
			}
			fmt.Printf("    %s %d) %s\r\n", marker, i+1, opt)
		}
		fmt.Print("  Toggles (e.g. 1 3): ")
		var input string
		reader := bufio.NewReader(os.Stdin)
		input, _ = reader.ReadString('\n')
		input = strings.TrimSpace(input)
		for _, tok := range strings.Fields(input) {
			if v, err := strconv.Atoi(tok); err == nil && v >= 1 && v <= len(options) {
				checked[v-1] = !checked[v-1]
			}
		}
		return checked, nil
	}

	oldState, err := term.MakeRaw(int(os.Stdin.Fd()))
	if err != nil {
		return checked, err
	}
	defer term.Restore(int(os.Stdin.Fd()), oldState)

	cursor := 0
	numOptions := len(options)

	draw := func() {
		out := fmt.Sprintf("\r  %s•%s %s%s%s %s(↑↓ move · space toggle · enter confirm)%s\r\n",
			colBold, colReset, colBold, prompt, colReset, colMuted, colReset)
		for i, opt := range options {
			isChecked := i < len(checked) && checked[i]
			box := "[ ]"
			if isChecked {
				box = "[✓]"
			}
			if i == cursor {
				if isChecked {
					out += fmt.Sprintf("\r    %s%s %s%s\r\n", colAccent, box, opt, colReset)
				} else {
					out += fmt.Sprintf("\r    %s%s%s %s%s\r\n", colAccent, box, colReset, opt, colReset)
				}
			} else {
				if isChecked {
					out += fmt.Sprintf("\r    %s%s %s%s\r\n", colAccent, box, opt, colReset)
				} else {
					out += fmt.Sprintf("\r    %s%s %s%s\r\n", colMuted, box, opt, colReset)
				}
			}
		}
		os.Stdout.WriteString(out)
	}

	clearMenu := func() {
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
			return checked, err
		}
		if n == 1 {
			switch buf[0] {
			case 3: // Ctrl+C
				clearMenu()
				return checked, errors.New("interrupted")
			case 32: // Space – toggle
				if cursor < len(checked) {
					checked[cursor] = !checked[cursor]
				}
				clearMenu()
				draw()
			case 13, 10: // Enter – confirm
				clearMenu()
				term.Restore(int(os.Stdin.Fd()), oldState)
				// Print summary of what was selected
				var selected []string
				for i, opt := range options {
					if i < len(checked) && checked[i] {
						selected = append(selected, opt)
					}
				}
				if len(selected) == 0 {
					selected = []string{"(none)"}
				}
				fmt.Printf("  %s✓%s %s: %s%s%s\r\n", colGreen, colReset, prompt, colAccent, strings.Join(selected, ", "), colReset)
				return checked, nil
			}
		} else if n >= 3 && buf[0] == 27 && buf[1] == 91 {
			switch buf[2] {
			case 'A': // Up
				cursor = (cursor - 1 + numOptions) % numOptions
			case 'B': // Down
				cursor = (cursor + 1) % numOptions
			}
			clearMenu()
			draw()
		}
	}
}

func interactiveWizard(cfg *Config) error {
	fmt.Printf("\r\n")

	// ── Step 1: URL ─────────────────────────────────────────────────────────
	urlInput, err := runWizardText("Paste the link (YouTube / Spotify / Qobuz)", "")
	if err != nil {
		return err
	}
	if urlInput == "" {
		return errors.New("no URL provided")
	}
	cfg.URLs = []string{urlInput}

	fmt.Printf("\r\n")

	// If Qobuz URL is detected, it is audio-first (Hi-Res FLAC / MP3)
	if isQobuzURL(urlInput) {
		logSubKV("service", "Qobuz (Hi-Res via Lucida backend)")
		fmtChoice, err := runCheckbox("Format", []string{"flac (Hi-Res 24-bit lossless)", "mp3 (320k)"}, 0)
		if err != nil {
			return err
		}
		if strings.HasPrefix(fmtChoice, "flac") {
			cfg.Format = "flac"
			cfg.Quality = "best"
		} else {
			cfg.Format = "mp3"
			cfg.Quality = "320k"
		}
		fmt.Printf("\r\n")
	} else {
		// ── Step 2: Media type ───────────────────────────────────────────────────
		mediaType, err := runCheckbox("Download type", []string{"video", "audio"}, 0)
		if err != nil {
			return err
		}

		fmt.Printf("\r\n")

		// ── Step 3: Format ───────────────────────────────────────────────────────
		var fmtOptions []string
		if mediaType == "video" {
			fmtOptions = []string{"mp4", "mkv", "webm", "mov", "original"}
		} else {
			fmtOptions = []string{"mp3", "m4a", "opus", "flac", "wav", "aac", "original"}
		}
		fmtChoice, err := runCheckbox("Format", fmtOptions, 0)
		if err != nil {
			return err
		}

		// "original" means we skip format conversion
		if fmtChoice == "original" {
			if mediaType == "video" {
				cfg.Format = "bestvideo+bestaudio/best"
			} else {
				cfg.Format = "bestaudio/best"
			}
		} else {
			cfg.Format = fmtChoice
		}

		fmt.Printf("\r\n")

		// ── Step 4: Quality ───────────────────────────────────────────────────────
		var qualOptions []string
		if mediaType == "video" {
			qualOptions = []string{"best", "4k", "2k", "1080p", "720p", "480p", "360p", "original"}
		} else {
			if fmtChoice == "flac" || fmtChoice == "wav" || fmtChoice == "original" {
				// lossless / original → no re-encoding quality choice
				cfg.Quality = "best"
				qualOptions = nil
			} else {
				qualOptions = []string{"best", "320k", "256k", "192k", "128k", "original"}
			}
		}

		if qualOptions != nil {
			qualChoice, err := runCheckbox("Quality", qualOptions, 0)
			if err != nil {
				return err
			}
			if qualChoice == "original" {
				cfg.Quality = "best"
			} else {
				cfg.Quality = strings.TrimSuffix(qualChoice, "p") // strip trailing 'p' for video labels
			}
			fmt.Printf("\r\n")
		}
	}

	// ── Step 5: Filename options (multi-checkbox) ────────────────────────────
	namingLabels := []string{"include artist", "include album", "include title", "track numbering (playlists)"}
	namingDefaults := []bool{
		cfg.NamingOpts.IncludeArtist,
		cfg.NamingOpts.IncludeAlbum,
		cfg.NamingOpts.IncludeTitle,
		cfg.NamingOpts.IncludeNumbering,
	}
	namingResult, err := runMultiCheckbox("Filename — include fields", namingLabels, namingDefaults)
	if err != nil {
		return err
	}
	cfg.NamingOpts.IncludeArtist = namingResult[0]
	cfg.NamingOpts.IncludeAlbum = namingResult[1]
	cfg.NamingOpts.IncludeTitle = namingResult[2]
	cfg.NamingOpts.IncludeNumbering = namingResult[3]

	// Guard: at least title must be on
	if !cfg.NamingOpts.IncludeTitle && !cfg.NamingOpts.IncludeArtist && !cfg.NamingOpts.IncludeAlbum {
		cfg.NamingOpts.IncludeTitle = true
		if cfg.Verbose {
			logWarn("custom namer received no active fields — failsafe fallback triggered: forced IncludeTitle=true")
		} else {
			logWarn("no fields selected — failsafe: defaulting to title")
		}
	}

	fmt.Printf("\r\n")

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
  --no-artist            omit artist from output filename
  --album                include album name in output filename
  --no-numbering         omit track numbering in playlist downloads
  -h, --help             print help with the codex box header
  --version              print "fetchit 1.0"

no args will routed to interactive wizard.`)
}

// ---------------------------------------------------------------------------
// Main Entry Point
// ---------------------------------------------------------------------------

func main() {
	initColors()

	homeDir, _ := os.UserHomeDir()
	defaultDownloads := filepath.Join(homeDir, "Downloads")

	cfg := &Config{
		Format:     "mp4",
		Quality:    "best",
		OutputDir:  defaultDownloads,
		NamingOpts: DefaultRenameOptions(false),
	}

	var showHelp bool
	var showVersion bool
	var mp3Flag bool
	var mp4Flag bool
	var noArtistFlag bool
	var includeAlbumFlag bool
	var numberingFlag bool
	var noNumberingFlag bool

	fs := flag.NewFlagSet("fetchit", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	fs.StringVar(&cfg.Format, "f", cfg.Format, "")
	fs.StringVar(&cfg.Format, "format", cfg.Format, "")
	fs.StringVar(&cfg.Quality, "q", cfg.Quality, "")
	fs.StringVar(&cfg.Quality, "quality", cfg.Quality, "")
	fs.StringVar(&cfg.OutputDir, "o", cfg.OutputDir, "")
	fs.StringVar(&cfg.OutputDir, "output", cfg.OutputDir, "")
	fs.StringVar(&cfg.Country, "country", "auto", "")
	fs.StringVar(&cfg.CFClearance, "cf-clearance", "", "")
	fs.StringVar(&cfg.UserAgent, "user-agent", "", "")
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
	fs.BoolVar(&noArtistFlag, "no-artist", false, "")
	fs.BoolVar(&includeAlbumFlag, "album", false, "")
	fs.BoolVar(&includeAlbumFlag, "include-album", false, "")
	fs.BoolVar(&numberingFlag, "numbering", false, "")
	fs.BoolVar(&noNumberingFlag, "no-numbering", false, "")
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

	if noArtistFlag {
		cfg.NamingOpts.IncludeArtist = false
	}
	if includeAlbumFlag {
		cfg.NamingOpts.IncludeAlbum = true
	}
	if noNumberingFlag {
		cfg.NamingOpts.IncludeNumbering = false
		cfg.ExplicitNumbering = true
	} else if numberingFlag {
		cfg.NamingOpts.IncludeNumbering = true
		cfg.ExplicitNumbering = true
	}

	if !cfg.NamingOpts.IncludeTitle && !cfg.NamingOpts.IncludeArtist && !cfg.NamingOpts.IncludeAlbum {
		cfg.NamingOpts.IncludeTitle = true
		if cfg.Verbose {
			logWarn("custom namer: no fields enabled, failsafe forced title")
		} else {
			logWarn("custom namer: failsafe enabled (title)")
		}
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

	// Pre-check Lucida availability once if any URL is a Qobuz link
	lucidaChecked := false
	lucidaCFBlocked := false
	for _, u := range cfg.URLs {
		if isQobuzURL(u) && !lucidaChecked {
			lucidaChecked = true
			lucidaClient := newLucidaHTTPClient(cfg)
			if err := checkLucidaAvailability(lucidaClient, cfg); err != nil {
				var cfErr lucidaCFError
				if errors.As(err, &cfErr) {
					lucidaCFBlocked = true
					logErr("lucida.to is blocked by Cloudflare — Qobuz downloads are unavailable.")
					if cfg.Verbose {
						logSub("Pass --cf-clearance <token> and --user-agent <ua> to bypass the captcha wall.")
						logSub("Get cf_clearance from your browser's DevTools → Application → Cookies → lucida.to")
					} else {
						logSub("")
					}
				} else if cfg.Verbose {
					logWarn(fmt.Sprintf("lucida.to connectivity check: %v — will try anyway", err))
				}
			}
			break
		}
	}

	for idx, url := range cfg.URLs {
		if isSpotifyURL(url) {
			logPhase(fmt.Sprintf("[%d/%d] Spotify: %s", idx+1, len(cfg.URLs), url))
			saved, spErr := runSpotifyDownload(cfg, binDir, url)
			if spErr != nil {
				failedCount++
			}
			allSavedFiles = append(allSavedFiles, saved...)
			continue
		}

		if isQobuzURL(url) {
			logPhase(fmt.Sprintf("[%d/%d] Qobuz: %s", idx+1, len(cfg.URLs), url))
			if lucidaCFBlocked {
				logErr("skipped — lucida.to failed")
				failedCount++
				continue
			}
			saved, qbErr := runQobuzDownload(cfg, binDir, url)
			if qbErr != nil {
				failedCount++
			}
			allSavedFiles = append(allSavedFiles, saved...)
			continue
		}

		logPhase(fmt.Sprintf("[%d/%d] Resolving %s", idx+1, len(cfg.URLs), url))

		info, err := resolveMedia(binDir, url, cfg.Debug)
		if err != nil {
			if cfg.Verbose {
				logWarn(fmt.Sprintf("could not resolve: %v", err))
			} else {
				logWarn("could not resolve — skipping")
			}
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
		cleanOutputDir := resolvePath(cfg.OutputDir)
		fmt.Printf("Save to [%s]: ", cleanOutputDir)
		reader := bufio.NewReader(os.Stdin)
		newDirInput, _ := reader.ReadString('\n')
		newDirInput = strings.TrimSpace(newDirInput)

		if newDirInput != "" {
			targetDir := resolvePath(newDirInput)
			if targetDir != "" && targetDir != cleanOutputDir {
				if err := os.MkdirAll(targetDir, 0755); err != nil {
					if cfg.Verbose {
						logErr(fmt.Sprintf("failed to create destination directory [%s]: %v", targetDir, err))
					} else {
						logErr(fmt.Sprintf("cannot create destination directory: %s", targetDir))
					}
				} else {
					if cfg.Verbose {
						logSubKV("dest-dir", targetDir)
					}

					movedCount := 0
					failCount := 0
					dirsToCheck := make(map[string]bool)

					for _, src := range allSavedFiles {
						cleanSrc := resolvePath(src)
						if _, err := os.Stat(cleanSrc); err != nil {
							if cfg.Verbose {
								logWarn(fmt.Sprintf("skip missing source file: %s", cleanSrc))
							}
							continue
						}

						rel, err := filepath.Rel(cleanOutputDir, cleanSrc)
						if err != nil || strings.HasPrefix(rel, "..") {
							rel = filepath.Base(cleanSrc)
						}

						dest := filepath.Join(targetDir, rel)
						destDir := filepath.Dir(dest)
						if err := os.MkdirAll(destDir, 0755); err != nil {
							failCount++
							if cfg.Verbose {
								logErr(fmt.Sprintf("failed to create target folder %s: %v", destDir, err))
							}
							continue
						}

						if cfg.Verbose {
							logSub(fmt.Sprintf("moving %s → %s", filepath.Base(cleanSrc), dest))
						}

						if err := moveFile(cleanSrc, dest); err != nil {
							failCount++
							if cfg.Verbose {
								logErr(fmt.Sprintf("failed to move %s: %v", filepath.Base(cleanSrc), err))
							}
						} else {
							movedCount++
							srcParent := filepath.Dir(cleanSrc)
							if srcParent != cleanOutputDir {
								dirsToCheck[srcParent] = true
							}
						}
					}

					// Clean up empty source subdirectories
					for dir := range dirsToCheck {
						if entries, err := os.ReadDir(dir); err == nil && len(entries) == 0 {
							_ = os.Remove(dir)
							if cfg.Verbose {
								logSubKV("cleaned", dir)
							}
						}
					}

					if failCount == 0 && movedCount > 0 {
						logOK(fmt.Sprintf("moved %d file(s) → %s", movedCount, targetDir))
					} else if movedCount > 0 && failCount > 0 {
						if cfg.Verbose {
							logWarn(fmt.Sprintf("relocation partially completed: %d moved, %d failed", movedCount, failCount))
						} else {
							logWarn(fmt.Sprintf("moved %d file(s) → %s (%d failed)", movedCount, targetDir, failCount))
						}
					} else if movedCount == 0 && failCount > 0 {
						if cfg.Verbose {
							logErr(fmt.Sprintf("relocation failed: 0 of %d file(s) moved to %s", len(allSavedFiles), targetDir))
						} else {
							logErr(fmt.Sprintf("failed to move files to %s", targetDir))
						}
					}
				}
			}
		}

		fmt.Print("\nPress Enter to exit…")
		_, _ = reader.ReadString('\n')
	}
}
