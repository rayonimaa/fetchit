package modules

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/term"
)

// ANSI color palette
var (
	ColReset  = "\033[0m"
	ColDim    = "\033[2m"
	ColBold   = "\033[1m"
	ColAccent = "\033[38;5;39m"
	ColGreen  = "\033[38;5;42m"
	ColAmber  = "\033[38;5;214m"
	ColRed    = "\033[38;5;203m"
	ColGutter = "\033[38;5;240m"
	ColText   = "\033[38;5;252m"
	ColMuted  = "\033[38;5;245m"

	colReset  = ColReset
	colDim    = ColDim
	colBold   = ColBold
	colAccent = ColAccent
	colGreen  = ColGreen
	colAmber  = ColAmber
	colRed    = ColRed
	colGutter = ColGutter
	colText   = ColText
	colMuted  = ColMuted
)

var (
	IsTerminalStdout = false
	IsTerminalStdin  = false
	isTerminalStdout = false
	isTerminalStdin  = false
	SpinnerChars     = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}
	spinnerChars     = SpinnerChars
)

func InitColors() {
	IsTerminalStdout = term.IsTerminal(int(os.Stdout.Fd()))
	IsTerminalStdin = term.IsTerminal(int(os.Stdin.Fd()))
	isTerminalStdout = IsTerminalStdout
	isTerminalStdin = IsTerminalStdin

	noColor := os.Getenv("NO_COLOR") != ""
	termEnv := os.Getenv("TERM")

	if !IsTerminalStdout || noColor || termEnv == "dumb" {
		ColReset = ""
		ColDim = ""
		ColBold = ""
		ColAccent = ""
		ColGreen = ""
		ColAmber = ""
		ColRed = ""
		ColGutter = ""
		ColText = ""
		ColMuted = ""

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

func init() {
	InitColors()
}

// RuneWidth returns 2 for East Asian Wide/Fullwidth runes, 0 for combining marks, 1 otherwise.
func RuneWidth(r rune) int {
	if unicode.Is(unicode.Mn, r) || unicode.Is(unicode.Me, r) || unicode.Is(unicode.Cc, r) || unicode.Is(unicode.Cf, r) {
		return 0
	}
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

var runeWidth = RuneWidth

func StringWidth(s string) int {
	w := 0
	for _, r := range s {
		w += RuneWidth(r)
	}
	return w
}

var stringWidth = StringWidth

func TruncateString(s string, maxCols int) string {
	w := 0
	var res []rune
	for _, r := range s {
		rw := RuneWidth(r)
		if w+rw > maxCols {
			if maxCols >= 3 && len(res) > 0 {
				return string(res) + "…"
			}
			return string(res)
		}
		res = append(res, r)
		w += rw
	}
	return string(res)
}

var truncateString = TruncateString

// Log styles
func LogPhase(msg string) {
	fmt.Printf("  • %s%s%s\n", colBold, msg, colReset)
}
var logPhase = LogPhase

func LogSub(msg string) {
	fmt.Printf("    %s%s%s\n", colDim, msg, colReset)
}
var logSub = LogSub

func LogSubKV(label, val string) {
	paddedLabel := label
	if len(paddedLabel) < 9 {
		paddedLabel = paddedLabel + strings.Repeat(" ", 9-len(paddedLabel))
	}
	fmt.Printf("  │ %s%s %s%s%s\n", colMuted, paddedLabel, colText, val, colReset)
}
var logSubKV = LogSubKV

func LogTool(argv string) {
	fmt.Printf("  ↳ %s%s%s\n", colDim, argv, colReset)
}
var logTool = LogTool

func LogOK(msg string) {
	fmt.Printf("  ✓ %s%s%s\n", colText, msg, colReset)
}
var logOK = LogOK

func LogErr(msg string) {
	fmt.Printf("  ✗ %s%s%s\n", colText, msg, colReset)
}
var logErr = LogErr

func LogWarn(msg string) {
	fmt.Printf("  △ %s%s%s\n", colText, msg, colReset)
}
var logWarn = LogWarn

// Terminal Progress UI
func DrawProgressBar(spinnerIdx int, pct float64, speed, eta, status string) {
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

	line := fmt.Sprintf("\r\033[K  %s%s%s  %s%s%s   %s  · %s  · eta %s  %s│%s %s%s%s",
		colAccent, sp, colReset,
		colAccent, bar, colReset,
		pctStr, speed, eta,
		colGutter, colReset,
		colMuted, status, colReset)
	fmt.Print(line)
}
var drawProgressBar = DrawProgressBar

func ClearLine() {
	if isTerminalStdout {
		fmt.Print("\r\033[K")
	}
}
var clearLine = ClearLine

func ParsePercent(s string) float64 {
	s = strings.TrimSpace(s)
	s = strings.TrimSuffix(s, "%")
	val, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return -1
	}
	return val
}
var parsePercent = ParsePercent

// ---------------------------------------------------------------------------
// Interactive Wizard Helpers
// ---------------------------------------------------------------------------

func RunCheckbox(prompt string, options []string, defaultIdx int) (string, error) {
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
var runCheckbox = RunCheckbox

func RunWizardText(prompt, defaultValue string) (string, error) {
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
var runWizardText = RunWizardText

func RunMultiCheckbox(prompt string, options []string, checked []bool) ([]bool, error) {
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
		if input != "" {
			parts := strings.Fields(input)
			for _, p := range parts {
				if idx, err := strconv.Atoi(p); err == nil && idx >= 1 && idx <= len(checked) {
					checked[idx-1] = !checked[idx-1]
				}
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
		out := fmt.Sprintf("\r  %s•%s %s%s%s %s(space=toggle, enter=confirm)%s\r\n", colBold, colReset, colBold, prompt, colReset, colMuted, colReset)
		for i, opt := range options {
			box := "[ ]"
			if i < len(checked) && checked[i] {
				box = "[✓]"
			}
			if i == cursor {
				out += fmt.Sprintf("\r  %s›%s %s%s %s%s\r\n", colAccent, colReset, colAccent, box, opt, colReset)
			} else {
				if i < len(checked) && checked[i] {
					out += fmt.Sprintf("\r    %s%s %s%s\r\n", colGreen, box, opt, colReset)
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
				return nil, errors.New("interrupted")
			case 32: // Space
				if cursor >= 0 && cursor < len(checked) {
					checked[cursor] = !checked[cursor]
				}
				clearMenu()
				draw()
			case 13, 10: // Enter
				clearMenu()
				term.Restore(int(os.Stdin.Fd()), oldState)
				var summary []string
				for i, ok := range checked {
					if ok && i < len(options) {
						summary = append(summary, options[i])
					}
				}
				fmt.Printf("  %s✓%s %s: %s%s%s\r\n", colGreen, colReset, prompt, colAccent, strings.Join(summary, ", "), colReset)
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
var runMultiCheckbox = RunMultiCheckbox

// ---------------------------------------------------------------------------
// Dynamic Naming Engine Models & Helpers
// ---------------------------------------------------------------------------

type FileMeta struct {
	Title       string
	Artist      string
	Album       string
	TrackNumber int
	Extension   string
}

type RenameOptions struct {
	IncludeArtist    bool
	IncludeAlbum     bool
	IncludeTitle     bool
	IsPlaylist       bool
	IncludeNumbering bool
}

func DefaultRenameOptions(isPlaylist bool) RenameOptions {
	return RenameOptions{
		IncludeArtist:    true,
		IncludeAlbum:     false,
		IncludeTitle:     true,
		IsPlaylist:       isPlaylist,
		IncludeNumbering: isPlaylist,
	}
}

var reservedDeviceNames = map[string]bool{
	"CON": true, "PRN": true, "AUX": true, "NUL": true,
	"COM1": true, "COM2": true, "COM3": true, "COM4": true,
	"COM5": true, "COM6": true, "COM7": true, "COM8": true, "COM9": true,
	"LPT1": true, "LPT2": true, "LPT3": true, "LPT4": true,
	"LPT5": true, "LPT6": true, "LPT7": true, "LPT8": true, "LPT9": true,
}

func SanitizeBaseName(name string) string {
	invalidChars := []string{"<", ">", ":", "\"", "/", "\\", "|", "?", "*"}
	clean := name
	for _, ch := range invalidChars {
		clean = strings.ReplaceAll(clean, ch, "_")
	}

	var b strings.Builder
	for _, r := range clean {
		if r < 32 || r == 127 {
			continue
		}
		b.WriteRune(r)
	}
	clean = b.String()

	clean = strings.Join(strings.Fields(clean), " ")
	clean = strings.TrimRight(clean, " ._")
	clean = strings.TrimLeft(clean, " ")

	if clean == "" {
		clean = "Track"
	}

	upperBase := strings.ToUpper(clean)
	if reservedDeviceNames[upperBase] {
		clean = "_" + clean
	}

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

var sanitizeBaseName = SanitizeBaseName

var (
	ErrNoNamerInput   = errors.New("custom namer received no metadata input")
	ErrNoNamingFields = errors.New("custom namer received no active naming fields")
)

func GenerateFilename(meta FileMeta, opts RenameOptions) (string, error) {
	var namerErr error

	if !opts.IncludeTitle && !opts.IncludeArtist && !opts.IncludeAlbum {
		opts.IncludeTitle = true
		namerErr = ErrNoNamingFields
	}

	artist := strings.TrimSpace(meta.Artist)
	album := strings.TrimSpace(meta.Album)
	title := strings.TrimSpace(meta.Title)

	var parts []string
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
		baseName = strings.Join(parts, " - ")
	}

	if opts.IsPlaylist && opts.IncludeNumbering && meta.TrackNumber > 0 {
		baseName = fmt.Sprintf("%02d - %s", meta.TrackNumber, baseName)
	}

	baseName = SanitizeBaseName(baseName)

	ext := strings.TrimSpace(meta.Extension)
	if ext != "" && !strings.HasPrefix(ext, ".") {
		ext = "." + ext
	}

	return baseName + ext, namerErr
}

var generateFilename = GenerateFilename

// ---------------------------------------------------------------------------
// File & Path Utilities
// ---------------------------------------------------------------------------

func ResolvePath(p string) string {
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

var resolvePath = ResolvePath

func MoveFile(src, dest string) error {
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

	srcFile.Close()

	if err := os.Remove(src); err != nil {
		return fmt.Errorf("copied to %s but failed to remove original %s: %w", dest, src, err)
	}

	return nil
}

var moveFile = MoveFile

// ---------------------------------------------------------------------------
// Config & Binary Management
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

func GetBinDir() (string, error) {
	if custom := os.Getenv("FETCHIT_HOME"); custom != "" {
		return filepath.Join(custom, "bin"), nil
	}
	cacheDir, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(cacheDir, "fetchit", "bin"), nil
}

var getBinDir = GetBinDir

func YtDlpBinaryName() string {
	if runtime.GOOS == "windows" {
		return "yt-dlp.exe"
	}
	return "yt-dlp"
}

var ytDlpBinaryName = YtDlpBinaryName

func FFmpegBinaryName() string {
	if runtime.GOOS == "windows" {
		return "ffmpeg.exe"
	}
	return "ffmpeg"
}

var ffmpegBinaryName = FFmpegBinaryName

func FFprobeBinaryName() string {
	if runtime.GOOS == "windows" {
		return "ffprobe.exe"
	}
	return "ffprobe"
}

var ffprobeBinaryName = FFprobeBinaryName
