package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ---------------------------------------------------------------------------
// Spotify URL detection
// ---------------------------------------------------------------------------

var spotifyURLRe = regexp.MustCompile(
	`(?:https?://)?(?:open\.)?spotify\.com/(?:intl-[a-z]{2,}/)?(?P<type>track|playlist|album)/(?P<id>[a-zA-Z0-9]+)`,
)

func isSpotifyURL(url string) bool {
	return spotifyURLRe.MatchString(url)
}

type spotifyURLInfo struct {
	Type string // "track", "playlist", or "album"
	ID   string
}

func parseSpotifyURL(url string) (*spotifyURLInfo, error) {
	m := spotifyURLRe.FindStringSubmatch(url)
	if m == nil {
		return nil, fmt.Errorf("not a valid Spotify URL: %s", url)
	}
	typeIdx := spotifyURLRe.SubexpIndex("type")
	idIdx := spotifyURLRe.SubexpIndex("id")
	return &spotifyURLInfo{Type: m[typeIdx], ID: m[idIdx]}, nil
}

// ---------------------------------------------------------------------------
// Embedded Python helper — fetches metadata from Spotify embed pages
//
// This is a minimal, self-contained script that uses only the `requests`
// library (no pip packages from the sunnify repo). It talks directly to
// Spotify's public embed endpoints — the same technique used by
// sunnypatell/sunnify-spotify-downloader — but trimmed to the bare
// metadata extraction needed by fetchit.
//
// The script outputs JSON lines to stdout. Each line is one track:
//   {"id":"...","title":"...","artists":"...","album":"...","duration_ms":...}
//
// For playlists/albums with >100 tracks it uses Spotify's spclient API
// as a fallback (same anonymous-token trick from the sunnify project).
// ---------------------------------------------------------------------------

const spotifyHelperPy = `#!/usr/bin/env python3
"""Spotify metadata extractor for fetchit.

Extracts track metadata from Spotify embed pages.
Outputs one JSON object per line to stdout.

Usage:
    python spotify_helper.py track   <track_id>
    python spotify_helper.py playlist <playlist_id>
    python spotify_helper.py album   <album_id>
"""
import json, re, sys, time

try:
    import requests
except ImportError:
    sys.stderr.write("ERROR: Python 'requests' library is required.\n")
    sys.stderr.write("Install with: pip install requests\n")
    sys.exit(1)

UA = (
    "Mozilla/5.0 (Windows NT 10.0; Win64; x64) "
    "AppleWebKit/537.36 (KHTML, like Gecko) "
    "Chrome/120.0.0.0 Safari/537.36"
)
SOCIAL_UA = "facebookexternalhit/1.1"
HEADERS = {
    "accept": "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8",
    "accept-language": "en-US,en;q=0.9",
    "user-agent": UA,
}
NEXT_DATA_RE = re.compile(r'<script id="__NEXT_DATA__"[^>]*>([^<]+)</script>')
OG_DESC_RE = re.compile(
    r'<meta\s+(?=[^>]*\bproperty="og:description")[^>]*\bcontent="([^"]*)"',
    re.IGNORECASE,
)
EMBED_PLAYLIST = "https://open.spotify.com/embed/playlist/{}"
EMBED_ALBUM    = "https://open.spotify.com/embed/album/{}"
EMBED_TRACK    = "https://open.spotify.com/embed/track/{}"
TRACK_PAGE     = "https://open.spotify.com/track/{}"
SPCLIENT       = "https://spclient.wg.spotify.com/playlist/v2/playlist/{}"

session = requests.Session()
_token = None
_token_exp = 0


def _resolve_path(data, keys):
    for k in keys:
        if not isinstance(data, dict):
            return None
        data = data.get(k)
    return data


def _fetch_embed(url):
    global _token, _token_exp
    for attempt in range(3):
        try:
            r = session.get(url, headers=HEADERS, timeout=30)
        except (requests.Timeout, requests.ConnectionError) as e:
            if attempt < 2:
                time.sleep(1.5 * (2 ** attempt))
                continue
            raise
        if r.status_code == 429:
            time.sleep(2 * (2 ** attempt))
            continue
        if r.status_code != 200:
            raise RuntimeError(f"HTTP {r.status_code} for {url}")
        break
    else:
        raise RuntimeError(f"Failed after retries: {url}")

    m = NEXT_DATA_RE.search(r.text)
    if not m:
        raise RuntimeError("No __NEXT_DATA__ found")
    data = json.loads(m.group(1))

    # cache token
    for path in [
        ("props", "pageProps", "state", "settings", "session"),
        ("props", "pageProps", "settings", "session"),
        ("props", "pageProps", "session"),
    ]:
        sess = _resolve_path(data, path)
        if isinstance(sess, dict) and "accessToken" in sess:
            _token = sess["accessToken"]
            _token_exp = sess.get("accessTokenExpirationTimestampMs", 0) / 1000
            break
    return data


def _extract_entity(data):
    for path in [
        ("props", "pageProps", "state", "data", "entity"),
        ("props", "pageProps", "data", "entity"),
        ("props", "pageProps", "entity"),
    ]:
        e = _resolve_path(data, path)
        if isinstance(e, dict):
            return e
    # fallback: search for trackList
    def _find(d, key, depth=6):
        if not isinstance(d, dict) or depth <= 0:
            return None
        if key in d:
            return d
        for v in d.values():
            r = _find(v, key, depth - 1)
            if r:
                return r
        return None
    c = _find(data, "trackList")
    if c:
        return c
    raise RuntimeError("Cannot find entity in embed data")


def _fetch_album_from_page(track_id):
    try:
        h = dict(HEADERS)
        h["user-agent"] = SOCIAL_UA
        r = session.get(TRACK_PAGE.format(track_id), headers=h, timeout=15)
        if r.status_code != 200:
            return None
        m = OG_DESC_RE.search(r.text)
        if not m:
            return None
        import html as _h
        parts = _h.unescape(m.group(1)).split(" \u00b7 ")
        return parts[1].strip() if len(parts) >= 2 else None
    except Exception:
        return None


def _parse_track(t, tid):
    title = t.get("title") or t.get("name") or "Unknown"
    artists = t.get("subtitle") or t.get("artists") or ""
    if isinstance(artists, list):
        artists = ", ".join(a.get("name", "") for a in artists if isinstance(a, dict))
    dur = t.get("duration")
    album = None
    if isinstance(t.get("album"), dict):
        album = t["album"].get("name")
    return {
        "id": tid,
        "title": str(title),
        "artists": str(artists),
        "album": album,
        "duration_ms": int(dur) if dur else None,
    }


def _fetch_track_meta(tid):
    try:
        data = _fetch_embed(EMBED_TRACK.format(tid))
        entity = _extract_entity(data)
    except Exception:
        return {"id": tid, "title": f"Track {tid}", "artists": "Unknown", "album": None, "duration_ms": None}

    title = entity.get("name") or entity.get("title") or "Unknown"
    artists_data = entity.get("artists", [])
    if isinstance(artists_data, list):
        artists = ", ".join(a.get("name", "") for a in artists_data if isinstance(a, dict))
    else:
        artists = entity.get("subtitle", "")

    album = _fetch_album_from_page(tid)

    rd = entity.get("releaseDate")
    dur = entity.get("duration")

    return {
        "id": tid,
        "title": str(title),
        "artists": str(artists),
        "album": album,
        "duration_ms": int(dur) if dur else None,
    }


def handle_track(tid):
    info = _fetch_track_meta(tid)
    print(json.dumps(info), flush=True)


def handle_collection(cid, content_type):
    if content_type == "album":
        url = EMBED_ALBUM.format(cid)
    else:
        url = EMBED_PLAYLIST.format(cid)

    data = _fetch_embed(url)
    entity = _extract_entity(data)
    track_list = entity.get("trackList", [])

    # Print playlist/album name to stderr for the Go side
    name = entity.get("name") or entity.get("title") or "Unknown"
    sys.stderr.write(f"PLAYLIST_NAME:{name}\n")
    sys.stderr.flush()

    embed_ids = set()
    album_name = entity.get("name") if content_type == "album" else None

    for t in track_list:
        if not isinstance(t, dict):
            continue
        uri = t.get("uri", "")
        tid = uri.split(":")[-1] if uri.startswith("spotify:track:") else ""
        if not tid:
            continue
        embed_ids.add(tid)
        info = _parse_track(t, tid)
        if album_name and not info["album"]:
            info["album"] = album_name
        print(json.dumps(info), flush=True)

    # For playlists, try spclient for tracks beyond 100
    if content_type != "playlist" or not _token:
        return

    try:
        spc_url = SPCLIENT.format(cid)
        h = {"Authorization": f"Bearer {_token}", "Accept": "application/json"}
        r = session.get(spc_url, headers=h, timeout=30)
        if r.status_code != 200:
            return
        spc = r.json()
        items = spc.get("contents", {}).get("items", [])
        pending = []
        for item in items:
            uri = item.get("uri", "")
            if not uri.startswith("spotify:track:"):
                continue
            tid = uri.split(":")[-1]
            if tid not in embed_ids:
                pending.append(tid)

        # Fetch remaining tracks (sequentially to avoid rate limits)
        for tid in pending:
            info = _fetch_track_meta(tid)
            print(json.dumps(info), flush=True)
            time.sleep(0.3)  # gentle rate limit
    except Exception:
        pass


if __name__ == "__main__":
    if len(sys.argv) < 3:
        sys.stderr.write("Usage: spotify_helper.py <track|playlist|album> <id>\n")
        sys.exit(1)
    kind = sys.argv[1]
    sid = sys.argv[2]
    try:
        if kind == "track":
            handle_track(sid)
        else:
            handle_collection(sid, kind)
    except Exception as e:
        sys.stderr.write(f"ERROR: {e}\n")
        sys.exit(1)
`

// ---------------------------------------------------------------------------
// Python detection
// ---------------------------------------------------------------------------

func findPython() (string, error) {
	// Try common names
	for _, name := range []string{"python3", "python"} {
		path, err := exec.LookPath(name)
		if err == nil {
			// Verify it's Python 3
			out, err := exec.Command(path, "--version").Output()
			if err == nil && strings.Contains(string(out), "Python 3") {
				return path, nil
			}
		}
	}
	return "", fmt.Errorf("python3 not found in PATH — install Python 3.8+ from https://python.org")
}

func ensurePythonRequests(pythonBin string) error {
	cmd := exec.Command(pythonBin, "-c", "import requests")
	if err := cmd.Run(); err != nil {
		logSub("Installing Python 'requests' package…")
		install := exec.Command(pythonBin, "-m", "pip", "install", "--user", "--quiet", "requests")
		install.Stdout = os.Stdout
		install.Stderr = os.Stderr
		if err := install.Run(); err != nil {
			return fmt.Errorf("failed to install 'requests': %w (run: pip install requests)", err)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Write the helper script to the bin dir
// ---------------------------------------------------------------------------

func ensureSpotifyHelper(binDir string) (string, error) {
	helperPath := filepath.Join(binDir, "spotify_helper.py")
	// Always overwrite to ensure latest version
	if err := os.WriteFile(helperPath, []byte(spotifyHelperPy), 0644); err != nil {
		return "", fmt.Errorf("failed to write spotify helper: %w", err)
	}
	return helperPath, nil
}

// ---------------------------------------------------------------------------
// Track info from the Python helper
// ---------------------------------------------------------------------------

type SpotifyTrack struct {
	ID         string `json:"id"`
	Title      string `json:"title"`
	Artists    string `json:"artists"`
	Album      string `json:"album"`
	DurationMs *int   `json:"duration_ms"`
}

func (t *SpotifyTrack) SearchQuery() string {
	q := t.Artists + " - " + t.Title
	if q == " - " || q == "Unknown - Unknown" {
		return ""
	}
	return q
}

func (t *SpotifyTrack) DisplayTitle() string {
	if t.Artists != "" && t.Artists != "Unknown" {
		return t.Artists + " – " + t.Title
	}
	return t.Title
}

func (t *SpotifyTrack) DurationString() string {
	if t.DurationMs == nil || *t.DurationMs <= 0 {
		return ""
	}
	sec := *t.DurationMs / 1000
	return fmt.Sprintf("%d:%02d", sec/60, sec%60)
}

// ---------------------------------------------------------------------------
// Fetch Spotify metadata via the Python helper
// ---------------------------------------------------------------------------

func fetchSpotifyTracks(pythonBin, helperPath string, info *spotifyURLInfo) ([]SpotifyTrack, string, error) {
	cmd := exec.Command(pythonBin, helperPath, info.Type, info.ID)

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return nil, "", err
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return nil, "", err
	}

	if err := cmd.Start(); err != nil {
		return nil, "", fmt.Errorf("failed to start spotify helper: %w", err)
	}

	var tracks []SpotifyTrack
	playlistName := ""

	// Read stderr for playlist name and errors
	var stderrBuf bytes.Buffer
	go func() {
		scanner := bufio.NewScanner(stderrPipe)
		for scanner.Scan() {
			line := scanner.Text()
			if strings.HasPrefix(line, "PLAYLIST_NAME:") {
				rawName := strings.TrimPrefix(line, "PLAYLIST_NAME:")
				decodedName, err := strconv.Unquote(`"` + rawName + `"`)
				if err == nil {
					playlistName = decodedName
				} else {
					playlistName = rawName
				}
			} else {
				stderrBuf.WriteString(line + "\n")
			}
		}
	}()

	// Read stdout for track JSON
	scanner := bufio.NewScanner(stdoutPipe)
	for scanner.Scan() {
		var track SpotifyTrack
		if err := json.Unmarshal(scanner.Bytes(), &track); err == nil {
			tracks = append(tracks, track)
		}
	}

	if err := cmd.Wait(); err != nil {
		errText := strings.TrimSpace(stderrBuf.String())
		if errText != "" {
			return tracks, playlistName, fmt.Errorf("%s", errText)
		}
		return tracks, playlistName, err
	}

	return tracks, playlistName, nil
}

// ---------------------------------------------------------------------------
// Download a single Spotify track via yt-dlp YouTube search
// ---------------------------------------------------------------------------

func downloadSpotifyTrack(cfg *Config, binDir string, track SpotifyTrack, trackNum, totalTracks int, subDir string) ([]string, error) {
	query := track.SearchQuery()
	if query == "" {
		return nil, fmt.Errorf("no search query for track %s", track.ID)
	}

	ytDlp := filepath.Join(binDir, ytDlpBinaryName())
	searchURL := fmt.Sprintf("ytsearch1:%s", query)

	// Determine output directory
	outDir := cfg.OutputDir
	if subDir != "" {
		outDir = filepath.Join(outDir, subDir)
	}
	_ = os.MkdirAll(outDir, 0755)

	// Build output template using dynamic naming system
	meta := FileMeta{
		Title:       track.Title,
		Artist:      track.Artists,
		Album:       track.Album,
		TrackNumber: trackNum,
		Extension:   ".%(ext)s",
	}
	opts := cfg.NamingOpts
	opts.IsPlaylist = (totalTracks > 1)
	if totalTracks <= 1 && !cfg.ExplicitNumbering {
		opts.IncludeNumbering = false
	}
	outputFilename, namerErr := generateFilename(meta, opts)
	if namerErr != nil {
		if cfg.Verbose {
			logWarn(fmt.Sprintf("custom namer: %v — failsafe filename applied: %s", namerErr, outputFilename))
		} else {
			logWarn(fmt.Sprintf("namer: no input, using failsafe %s", outputFilename))
		}
	}
	outputTemplate := filepath.Join(outDir, outputFilename)

	// Build a temp file to capture the final path
	tmpFileList, err := os.CreateTemp("", "fetchit-spotify-*.txt")
	if err != nil {
		return nil, err
	}
	tmpFilePath := tmpFileList.Name()
	tmpFileList.Close()
	defer os.Remove(tmpFilePath)

	// Determine audio format
	formatLower := strings.ToLower(cfg.Format)
	if !isAudioFormat(formatLower) {
		formatLower = "mp3" // Spotify downloads default to audio
	}

	args := []string{
		"--newline",
		"--ffmpeg-location", binDir,
		"--output-na-placeholder", "",
		"--no-keep-video",
		"--no-write-thumbnail",
		"-o", outputTemplate,
		"--print-to-file", "after_move:filepath", tmpFilePath,
		"--no-playlist",
		"--extract-audio",
		"--audio-format", formatLower,
	}

	// Audio quality
	qLower := strings.ToLower(cfg.Quality)
	if formatLower != "flac" && formatLower != "wav" {
		if qLower == "best" || qLower == "" {
			args = append(args, "--audio-quality", "0")
		} else {
			kbps := strings.TrimSuffix(qLower, "k")
			args = append(args, "--audio-quality", kbps+"K")
		}
	}

	// Metadata
	args = append(args, "--embed-metadata")
	if formatLower != "wav" {
		args = append(args, "--embed-thumbnail")
	}

	// Progress or verbose
	if !isTerminalStdout || cfg.Verbose {
		if !isTerminalStdout {
			args = append(args, "--no-progress")
		}
	} else {
		args = append(args, "--progress-template",
			"download:PROGRESS:%(progress._percent_str)s|%(progress._speed_str)s|%(progress._eta_str)s")
	}

	args = append(args, searchURL)

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
	lastStatus = "searching YouTube…"

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
			lastStatus = "downloading audio…"
		case strings.HasPrefix(line, "[ExtractAudio]"):
			lastStatus = "extracting audio…"
		case strings.HasPrefix(line, "[EmbedThumbnail]"):
			lastStatus = "embedding cover art…"
		case strings.HasPrefix(line, "[Metadata]"):
			lastStatus = "writing metadata…"
		case strings.HasPrefix(line, "[MoveFiles]"):
			lastStatus = "moving final file…"
		case strings.HasPrefix(line, "[ThumbnailsConvertor]"):
			lastStatus = "converting cover…"
		default:
			if cfg.Verbose {
				logSub(line)
			}
		}
	}

	go func() {
		s := bufio.NewScanner(stderrPipe)
		for s.Scan() {
			processLine(s.Text())
		}
	}()

	stdoutScanner := bufio.NewScanner(stdoutPipe)
	for stdoutScanner.Scan() {
		processLine(stdoutScanner.Text())
	}

	waitErr := cmd.Wait()
	close(stopTicker)
	tickerWG.Wait()
	clearLine()

	// Read saved files
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

	return savedFiles, waitErr
}

// ---------------------------------------------------------------------------
// Main Spotify download orchestrator
// ---------------------------------------------------------------------------

func runSpotifyDownload(cfg *Config, binDir, url string) ([]string, error) {
	spInfo, err := parseSpotifyURL(url)
	if err != nil {
		return nil, err
	}

	// Ensure Python + requests
	pythonBin, err := findPython()
	if err != nil {
		return nil, err
	}

	if err := ensurePythonRequests(pythonBin); err != nil {
		return nil, err
	}

	// Ensure helper script
	helperPath, err := ensureSpotifyHelper(binDir)
	if err != nil {
		return nil, err
	}

	// Fetch metadata
	logPhase(fmt.Sprintf("Fetching Spotify %s metadata…", spInfo.Type))

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
					fmt.Printf("\r  %s%s%s querying Spotify…", colAccent, spinnerChars[idx%len(spinnerChars)], colReset)
				}
				idx++
				time.Sleep(100 * time.Millisecond)
			}
		}
	}()

	tracks, playlistName, fetchErr := fetchSpotifyTracks(pythonBin, helperPath, spInfo)
	close(stopSpinner)
	wgSpinner.Wait()

	if fetchErr != nil && len(tracks) == 0 {
		return nil, fmt.Errorf("failed to fetch Spotify metadata: %w", fetchErr)
	}

	if len(tracks) == 0 {
		return nil, fmt.Errorf("no tracks found for Spotify %s %s", spInfo.Type, spInfo.ID)
	}

	// Display info
	if playlistName != "" {
		logSubKV("name", playlistName)
	}
	logSubKV("tracks", fmt.Sprintf("%d", len(tracks)))
	logSubKV("engine", "yt-dlp (YouTube search)")

	// Determine subdirectory for playlists/albums
	subDir := ""
	if spInfo.Type != "track" && playlistName != "" {
		subDir = sanitizeFilename(playlistName)
	}

	// If format is a video format, switch to mp3 for Spotify
	formatLower := strings.ToLower(cfg.Format)
	if !isAudioFormat(formatLower) {
		logSub("Spotify mode: using mp3 format (audio-only)")
		cfg.Format = "mp3"
	}

	// Download each track
	var allSaved []string
	failCount := 0
	for i, track := range tracks {
		trackLabel := fmt.Sprintf("[%d/%d]", i+1, len(tracks))
		dispTitle := track.DisplayTitle()
		if len(dispTitle) > 50 {
			dispTitle = dispTitle[:47] + "…"
		}
		logPhase(fmt.Sprintf("%s %s", trackLabel, dispTitle))

		dur := track.DurationString()
		if dur != "" {
			logSubKV("duration", dur)
		}
		if track.Album != "" {
			logSubKV("album", track.Album)
		}

		saved, err := downloadSpotifyTrack(cfg, binDir, track, i+1, len(tracks), subDir)
		if err != nil {
			logErr(fmt.Sprintf("failed: %v", err))
			failCount++
		} else if len(saved) > 0 {
			for _, f := range saved {
				logOK(fmt.Sprintf("saved: %s", filepath.Base(f)))
			}
		}
		allSaved = append(allSaved, saved...)
	}

	if failCount > 0 {
		return allSaved, fmt.Errorf("%d/%d tracks failed", failCount, len(tracks))
	}
	return allSaved, nil
}

// sanitizeFilename makes a string safe for use as a directory/file name
func sanitizeFilename(name string) string {
	return sanitizeBaseName(name)
}

// ---------------------------------------------------------------------------
// HTTP helper for cover art (used by metadata enrichment — future use)
// ---------------------------------------------------------------------------

func downloadCoverArt(url, destPath string) error {
	resp, err := http.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	f, err := os.Create(destPath)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(f, resp.Body)
	return err
}
