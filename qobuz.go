package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"time"
)

// ---------------------------------------------------------------------------
// Qobuz URL Detection
// ---------------------------------------------------------------------------

var qobuzURLRe = regexp.MustCompile(
	`(?i)^(?:https?://)?(?:[a-z0-9\-]+\.)?qobuz\.com/(?:[a-z]{2}-[a-z]{2}/)?(?:album|track|playlist|interpreter)/[a-zA-Z0-9_\-]+`,
)

// isQobuzURL checks if the provided URL is a Qobuz track, album, playlist, or interpreter link.
func isQobuzURL(rawURL string) bool {
	return qobuzURLRe.MatchString(rawURL)
}

// ---------------------------------------------------------------------------
// Lucida Models
// ---------------------------------------------------------------------------

type LucidaArtist struct {
	Name string `json:"name"`
}

type LucidaCoverArtwork struct {
	URL string `json:"url"`
}

type LucidaTrack struct {
	Title        string               `json:"title"`
	URL          string               `json:"url"`
	Artists      []LucidaArtist       `json:"artists"`
	Producers    []string             `json:"producers"`
	CSRF         string               `json:"csrf"`
	CSRFFallback string               `json:"csrfFallback"`
	CoverArtwork []LucidaCoverArtwork `json:"coverArtwork"`
	ReleaseDate  string               `json:"releaseDate"`
	TrackNumber  int                  `json:"trackNumber"`
}

type LucidaAlbumMeta struct {
	Title        string               `json:"title"`
	CoverArtwork []LucidaCoverArtwork `json:"coverArtwork"`
	Artists      []LucidaArtist       `json:"artists"`
	TrackCount   int                  `json:"trackCount"`
	ReleaseDate  string               `json:"releaseDate"`
	Tracks       []LucidaTrack        `json:"tracks"`
}

type LucidaInfoWrapper struct {
	Type         string               `json:"type"` // "album", "track", "playlist", "artist"
	Title        string               `json:"title"`
	CoverArtwork []LucidaCoverArtwork `json:"coverArtwork"`
	Artists      []LucidaArtist       `json:"artists"`
	TrackCount   int                  `json:"trackCount"`
	ReleaseDate  string               `json:"releaseDate"`
	Tracks       []LucidaTrack        `json:"tracks"`
	URL          string               `json:"url"`
	Album        *LucidaAlbumMeta     `json:"album"`
	Producers    []string             `json:"producers"`
	Name         string               `json:"name"`
}

type LucidaPageData struct {
	Info            LucidaInfoWrapper `json:"info"`
	OriginalService string            `json:"originalService"`
	Token           string            `json:"token"`
	TokenExpiry     int64             `json:"tokenExpiry"`
}

type LucidaAccount struct {
	ID   string `json:"id"`
	Type string `json:"type"`
}

type LucidaToken struct {
	Expiry    int64   `json:"expiry"`
	Primary   *string `json:"primary"`
	Secondary *string `json:"secondary"`
}

type LucidaUpload struct {
	Enabled bool `json:"enabled"`
}

type LucidaTrackDownloadRequest struct {
	Account   LucidaAccount `json:"account"`
	Compat    bool          `json:"compat"`
	Downscale string        `json:"downscale"`
	Handoff   bool          `json:"handoff"`
	Metadata  bool          `json:"metadata"`
	Private   bool          `json:"private"`
	Token     LucidaToken   `json:"token"`
	Upload    LucidaUpload  `json:"upload"`
	URL       string        `json:"url"`
}

type LucidaTrackDownloadResponse struct {
	Handoff string `json:"handoff"`
	Server  string `json:"server"`
	Error   string `json:"error"`
}

type LucidaDownloadStatusResponse struct {
	Status  string `json:"status"`
	Message string `json:"message"`
}

// ---------------------------------------------------------------------------
// Lucida API Client
// ---------------------------------------------------------------------------

// newLucidaHTTPClient creates an HTTP client configured with browser headers and optional Cloudflare clearance.
func newLucidaHTTPClient(cfg *Config) *http.Client {
	return &http.Client{
		Timeout: 45 * time.Second,
	}
}

// applyLucidaHeaders attaches browser-like User-Agent and CF clearance cookies if configured.
func applyLucidaHeaders(req *http.Request, cfg *Config) {
	ua := cfg.UserAgent
	if ua == "" {
		ua = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36"
	}
	req.Header.Set("User-Agent", ua)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")

	if cfg.CFClearance != "" {
		req.Header.Set("Cookie", fmt.Sprintf("cf_clearance=%s", cfg.CFClearance))
	}
}

// lucidaCFError is a sentinel error for Cloudflare captcha blocks.
type lucidaCFError struct{}

func (lucidaCFError) Error() string {
	return "Cloudflare captcha required by lucida.to"
}

// checkLucidaAvailability verifies if lucida.to is accessible or challenged by Cloudflare.
// Returns a lucidaCFError if a captcha wall is detected, or a plain error for other failures.
func checkLucidaAvailability(client *http.Client, cfg *Config) error {
	req, err := http.NewRequestWithContext(context.Background(), "GET", "https://lucida.to/", nil)
	if err != nil {
		return err
	}
	applyLucidaHeaders(req, cfg)

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("cannot reach lucida.to: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusOK {
		return nil
	}
	if resp.StatusCode == http.StatusForbidden {
		return lucidaCFError{}
	}
	return fmt.Errorf("lucida.to returned HTTP %d", resp.StatusCode)
}

// parseLucidaEnclosedJSON extracts the JSON page data embedded within Lucida's HTML response.
func parseLucidaEnclosedJSON(html string) (*LucidaPageData, error) {
	const startMarker = `,{"type":"data","data":`
	const endMarker = `,"uses":{"url":1}}];`

	startIdx := strings.Index(html, startMarker)
	if startIdx == -1 {
		return nil, errors.New("could not find data start marker in Lucida response")
	}
	startIdx += len(startMarker)

	endRel := strings.Index(html[startIdx:], endMarker)
	if endRel == -1 {
		return nil, errors.New("could not find data end marker in Lucida response")
	}
	endIdx := startIdx + endRel

	rawJSON := html[startIdx:endIdx]

	var pageData LucidaPageData
	if err := json.Unmarshal([]byte(rawJSON), &pageData); err != nil {
		return nil, fmt.Errorf("failed to parse Lucida page JSON: %w", err)
	}
	return &pageData, nil
}

// resolveLucidaMetadata fetches and parses album/track metadata from lucida.to.
func resolveLucidaMetadata(client *http.Client, cfg *Config, qobuzURL string) (*LucidaPageData, error) {
	country := cfg.Country
	if country == "" {
		country = "auto"
	}

	endpoint := fmt.Sprintf("https://lucida.to/?url=%s&country=%s", url.QueryEscape(qobuzURL), url.QueryEscape(country))

	req, err := http.NewRequestWithContext(context.Background(), "GET", endpoint, nil)
	if err != nil {
		return nil, err
	}
	applyLucidaHeaders(req, cfg)

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusForbidden {
		return nil, lucidaCFError{}
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Lucida resolver returned HTTP %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	html := string(body)

	// Check known error messages from Lucida HTML
	knownErrors := []string{
		"An error occured trying to process your request.",
		"Message: \"Cannot contact any valid server\"",
		"An error occurred. Had an issue getting that item, try again.",
	}
	for _, ke := range knownErrors {
		if strings.Contains(html, ke) {
			return nil, fmt.Errorf("Lucida error: %s", ke)
		}
	}

	return parseLucidaEnclosedJSON(html)
}

// requestLucidaTrackHandoff dispatches a stream fetch request to Lucida API load endpoint.
func requestLucidaTrackHandoff(client *http.Client, cfg *Config, track *LucidaTrack, tokenExpiry int64) (*LucidaTrackDownloadResponse, error) {
	country := cfg.Country
	if country == "" {
		country = "auto"
	}

	var csrfPtr *string
	if track.CSRF != "" {
		csrfPtr = &track.CSRF
	}

	reqBody := LucidaTrackDownloadRequest{
		Account: LucidaAccount{
			ID:   country,
			Type: "country",
		},
		Compat:    false,
		Downscale: "original",
		Handoff:   true,
		Metadata:  true,
		Private:   false,
		Token: LucidaToken{
			Expiry:  tokenExpiry,
			Primary: csrfPtr,
		},
		Upload: LucidaUpload{Enabled: false},
		URL:    track.URL,
	}

	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return nil, err
	}

	endpoint := "https://lucida.to/api/load?url=%2Fapi%2Ffetch%2Fstream%2Fv2"
	req, err := http.NewRequestWithContext(context.Background(), "POST", endpoint, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, err
	}
	applyLucidaHeaders(req, cfg)
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d when requesting track stream", resp.StatusCode)
	}

	respBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var dlResp LucidaTrackDownloadResponse
	if err := json.Unmarshal(respBytes, &dlResp); err != nil {
		return nil, fmt.Errorf("failed to parse track handoff response: %w", err)
	}

	if dlResp.Error != "" {
		return nil, fmt.Errorf("Lucida API error: %s", dlResp.Error)
	}
	if dlResp.Handoff == "" || dlResp.Server == "" {
		return nil, errors.New("empty handoff or server received from Lucida")
	}

	return &dlResp, nil
}

// pollLucidaDownloadStatus polls until Lucida finishes fetching the track from Qobuz.
func pollLucidaDownloadStatus(client *http.Client, cfg *Config, server, handoff string) error {
	statusURL := fmt.Sprintf("https://%s.lucida.to/api/fetch/request/%s", server, handoff)
	maxRetries := 60 // max 2 minutes polling
	for i := 0; i < maxRetries; i++ {
		req, err := http.NewRequestWithContext(context.Background(), "GET", statusURL, nil)
		if err != nil {
			return err
		}
		applyLucidaHeaders(req, cfg)

		resp, err := client.Do(req)
		if err != nil {
			time.Sleep(2 * time.Second)
			continue
		}

		if resp.StatusCode == http.StatusOK {
			var st LucidaDownloadStatusResponse
			_ = json.NewDecoder(resp.Body).Decode(&st)
			resp.Body.Close()

			if st.Status == "completed" {
				return nil
			}
			if st.Status == "error" || st.Status == "failed" {
				return fmt.Errorf("Lucida processing failed: %s", st.Message)
			}
		} else {
			resp.Body.Close()
		}

		time.Sleep(2 * time.Second)
	}

	return errors.New("timed out waiting for Lucida track conversion")
}

// streamLucidaAudio downloads the final processed audio stream directly into destPath.
func streamLucidaAudio(client *http.Client, cfg *Config, server, handoff, destPath string, spinnerIdx *int32) error {
	dlURL := fmt.Sprintf("https://%s.lucida.to/api/fetch/request/%s/download", server, handoff)
	req, err := http.NewRequestWithContext(context.Background(), "GET", dlURL, nil)
	if err != nil {
		return err
	}
	applyLucidaHeaders(req, cfg)

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d when downloading audio stream", resp.StatusCode)
	}

	tmpFile := destPath + ".part"
	_ = os.MkdirAll(filepath.Dir(tmpFile), 0755)

	out, err := os.OpenFile(tmpFile, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0755)
	if err != nil {
		return err
	}
	defer func() {
		out.Close()
		_ = os.Remove(tmpFile)
	}()

	totalSize := resp.ContentLength
	var downloaded int64
	buf := make([]byte, 64*1024)

	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			if _, wErr := out.Write(buf[:n]); wErr != nil {
				return wErr
			}
			downloaded += int64(n)
			if totalSize > 0 && isTerminalStdout && !cfg.Verbose {
				pct := float64(downloaded) / float64(totalSize) * 100.0
				idx := atomic.AddInt32(spinnerIdx, 1)
				drawProgressBar(int(idx), pct, "", "", "downloading Hi-Res FLAC…")
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				break
			}
			return readErr
		}
	}

	out.Close()
	clearLine()

	return os.Rename(tmpFile, destPath)
}

// downloadQobuzCoverArt retrieves the highest resolution cover art available from Qobuz CDN.
func downloadQobuzCoverArt(client *http.Client, cfg *Config, rawCoverURL, destCoverPath string) error {
	if rawCoverURL == "" {
		return nil
	}
	coverURL := rawCoverURL
	if strings.HasSuffix(coverURL, ".jpg") {
		if lastUnderscore := strings.LastIndex(coverURL, "_"); lastUnderscore != -1 {
			coverURL = coverURL[:lastUnderscore+1] + "org.jpg"
		}
	}

	req, err := http.NewRequestWithContext(context.Background(), "GET", coverURL, nil)
	if err != nil {
		return err
	}
	applyLucidaHeaders(req, cfg)

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d for cover art", resp.StatusCode)
	}

	f, err := os.OpenFile(destCoverPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		return err
	}
	defer f.Close()

	_, err = io.Copy(f, resp.Body)
	return err
}

// embedCoverArtFFmpeg embeds the cover image into a FLAC or MP3 audio file using the cached FFmpeg.
func embedCoverArtFFmpeg(binDir, audioPath, coverPath string) error {
	if _, err := os.Stat(coverPath); err != nil {
		return nil // No cover to embed
	}
	ffmpegBin := filepath.Join(binDir, ffmpegBinaryName())
	tmpOut := audioPath + ".tagged.flac"

	// ffmpeg -y -i input.flac -i cover.jpg -map 0:a -map 1:v -c:a copy -c:v copy -disposition:v:0 attached_pic out.flac
	cmd := exec.Command(ffmpegBin,
		"-y",
		"-i", audioPath,
		"-i", coverPath,
		"-map", "0:a",
		"-map", "1:v",
		"-c:a", "copy",
		"-c:v", "copy",
		"-disposition:v:0", "attached_pic",
		tmpOut,
	)

	if err := cmd.Run(); err != nil {
		_ = os.Remove(tmpOut)
		return err
	}

	_ = os.Remove(audioPath)
	return os.Rename(tmpOut, audioPath)
}

// ---------------------------------------------------------------------------
// Main Qobuz Download Orchestrator
// ---------------------------------------------------------------------------

// runQobuzDownload handles Qobuz URL downloads via the Lucida backend.
func runQobuzDownload(cfg *Config, binDir, qobuzURL string) ([]string, error) {
	client := newLucidaHTTPClient(cfg)

	// Step 1: Check Lucida availability — abort early on Cloudflare captcha
	logPhase("Connecting to Lucida…")
	if err := checkLucidaAvailability(client, cfg); err != nil {
		var cfErr lucidaCFError
		if errors.As(err, &cfErr) {
			// Friendly short message for casual use
			logErr("lucida.to is blocked by Cloudflare — cannot download Qobuz tracks right now.")
			if cfg.Verbose {
				logSub("Pass --cf-clearance <token> and --user-agent <ua> to bypass the captcha wall.")
				logSub("Get cf_clearance from your browser's DevTools → Application → Cookies → lucida.to")
			} else {
				logSub("")
			}
			return nil, fmt.Errorf("Cloudflare captcha required by lucida.to")
		}
		// Non-captcha connectivity error — warn but try anyway (might be a fluke)
		if cfg.Verbose {
			logWarn(fmt.Sprintf("lucida.to connectivity check failed: %v", err))
			logSub("Attempting to proceed anyway…")
		} else {
			logWarn("lucida.to connection check failed — trying anyway…")
		}
	} else {
		logOK("lucida.to connected")
	}

	// Step 2: Resolve album / track metadata
	logPhase(fmt.Sprintf("Resolving Qobuz metadata: %s", qobuzURL))
	pageData, err := resolveLucidaMetadata(client, cfg, qobuzURL)
	if err != nil {
		var cfErr lucidaCFError
		if errors.As(err, &cfErr) {
			logErr("lucida.to is blocked by Cloudflare — cannot resolve this Qobuz item.")
			if cfg.Verbose {
				logSub("Pass --cf-clearance <token> and --user-agent <ua> to bypass the captcha wall.")
			} else {
				logSub("")
			}
			return nil, fmt.Errorf("Cloudflare captcha required by lucida.to")
		}
		if cfg.Verbose {
			return nil, fmt.Errorf("failed to resolve Qobuz item: %w", err)
		}
		return nil, fmt.Errorf("could not fetch Qobuz metadata")
	}

	info := pageData.Info
	if info.Type == "artist" {
		return nil, fmt.Errorf("cannot download artist profile page: %s", info.Name)
	}

	var tracksToDownload []LucidaTrack
	albumTitle := info.Title
	primaryArtist := "Unknown"
	coverURL := ""

	if len(info.CoverArtwork) > 0 {
		coverURL = info.CoverArtwork[0].URL
	}
	if len(info.Artists) > 0 {
		primaryArtist = info.Artists[0].Name
	}

	switch info.Type {
	case "album", "playlist":
		tracksToDownload = info.Tracks
		for i := range tracksToDownload {
			if tracksToDownload[i].TrackNumber == 0 {
				tracksToDownload[i].TrackNumber = i + 1
			}
		}
	case "track":
		t := LucidaTrack{
			Title:        info.Title,
			URL:          info.URL,
			Artists:      info.Artists,
			Producers:    info.Producers,
			CSRF:         pageData.Token,
			CoverArtwork: info.CoverArtwork,
			ReleaseDate:  info.ReleaseDate,
			TrackNumber:  1,
		}
		if info.Album != nil {
			if albumTitle == "" {
				albumTitle = info.Album.Title
			}
			if len(info.Album.Artists) > 0 && primaryArtist == "Unknown" {
				primaryArtist = info.Album.Artists[0].Name
			}
			if coverURL == "" && len(info.Album.CoverArtwork) > 0 {
				coverURL = info.Album.CoverArtwork[0].URL
			}
		}
		tracksToDownload = []LucidaTrack{t}
	}

	if len(tracksToDownload) == 0 {
		return nil, errors.New("no downloadable tracks found in Qobuz metadata")
	}

	isMultiTrack := len(tracksToDownload) > 1
	logSubKV("album", truncateString(albumTitle, 40))
	logSubKV("artist", truncateString(primaryArtist, 40))
	logSubKV("tracks", fmt.Sprintf("%d", len(tracksToDownload)))
	logSubKV("format", "Hi-Res FLAC (Lossless 24-bit)")

	// Prepare output directory
	outDir := cfg.OutputDir
	if isMultiTrack && albumTitle != "" {
		outDir = filepath.Join(outDir, sanitizeFilename(albumTitle))
	}
	_ = os.MkdirAll(outDir, 0755)

	var savedFiles []string

	// Download cover art if available
	coverFilePath := filepath.Join(outDir, "cover.jpg")
	if coverURL != "" {
		if err := downloadQobuzCoverArt(client, cfg, coverURL, coverFilePath); err == nil {
			savedFiles = append(savedFiles, coverFilePath)
		}
	}

	var failCount int
	var spinnerIdx int32

	for i, track := range tracksToDownload {
		trackNum := track.TrackNumber
		if trackNum == 0 {
			trackNum = i + 1
		}

		artistName := primaryArtist
		if len(track.Artists) > 0 {
			var names []string
			for _, a := range track.Artists {
				names = append(names, a.Name)
			}
			artistName = strings.Join(names, ", ")
		}

		trackLabel := fmt.Sprintf("[%d/%d]", i+1, len(tracksToDownload))
		logPhase(fmt.Sprintf("%s %s – %s", trackLabel, artistName, track.Title))

		// Apply Dynamic File Naming Engine
		meta := FileMeta{
			Title:       track.Title,
			Artist:      artistName,
			Album:       albumTitle,
			TrackNumber: trackNum,
			Extension:   ".flac",
		}
		namingOpts := cfg.NamingOpts
		namingOpts.IsPlaylist = isMultiTrack
		if !isMultiTrack && !cfg.ExplicitNumbering {
			namingOpts.IncludeNumbering = false
		}
		finalFilename, namerErr := generateFilename(meta, namingOpts)
		if namerErr != nil {
			if cfg.Verbose {
				logWarn(fmt.Sprintf("custom namer: %v — failsafe filename applied: %s", namerErr, finalFilename))
			} else {
				logWarn(fmt.Sprintf("namer: no input, using failsafe %s", finalFilename))
			}
		}
		destPath := filepath.Join(outDir, finalFilename)

		if cfg.DryRun {
			logSubKV("dry-run", destPath)
			continue
		}

		// Skip if already exists
		if fi, err := os.Stat(destPath); err == nil && fi.Size() > 0 {
			logOK(fmt.Sprintf("already exists: %s", finalFilename))
			savedFiles = append(savedFiles, destPath)
			continue
		}

		// Request handoff
		handoffResp, err := requestLucidaTrackHandoff(client, cfg, &track, pageData.TokenExpiry)
		if err != nil {
			if cfg.Verbose {
				logErr(fmt.Sprintf("handoff failed: %v", err))
			} else {
				logErr("track handoff failed — skipping")
			}
			failCount++
			continue
		}

		// Poll status
		if err := pollLucidaDownloadStatus(client, cfg, handoffResp.Server, handoffResp.Handoff); err != nil {
			if cfg.Verbose {
				logErr(fmt.Sprintf("processing error: %v", err))
			} else {
				logErr("server processing failed — skipping")
			}
			failCount++
			continue
		}

		// Stream audio
		if err := streamLucidaAudio(client, cfg, handoffResp.Server, handoffResp.Handoff, destPath, &spinnerIdx); err != nil {
			if cfg.Verbose {
				logErr(fmt.Sprintf("download failed: %v", err))
			} else {
				logErr("audio download failed — skipping")
			}
			failCount++
			continue
		}

		// Embed cover art with FFmpeg
		if _, err := os.Stat(coverFilePath); err == nil {
			_ = embedCoverArtFFmpeg(binDir, destPath, coverFilePath)
		}

		cleanupIntermediateFiles(destPath)
		logOK(fmt.Sprintf("saved: %s", finalFilename))
		savedFiles = append(savedFiles, destPath)
	}

	if failCount > 0 && len(savedFiles) == 0 {
		if cfg.Verbose {
			return savedFiles, fmt.Errorf("%d/%d tracks failed", failCount, len(tracksToDownload))
		}
		return savedFiles, fmt.Errorf("%d tracks could not be downloaded (run with -V for details)", failCount)
	}

	return savedFiles, nil
}
