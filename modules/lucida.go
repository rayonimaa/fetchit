package modules

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
	"strings"
	"sync/atomic"
	"time"
)

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
// Lucida API Client Helpers
// ---------------------------------------------------------------------------

// newLucidaHTTPClient creates an HTTP client configured with reasonable timeouts.
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

// resolveLucidaMetadata fetches and parses media metadata from lucida.to.
func resolveLucidaMetadata(client *http.Client, cfg *Config, mediaURL string) (*LucidaPageData, error) {
	country := cfg.Country
	if country == "" {
		country = "auto"
	}

	endpoint := fmt.Sprintf("https://lucida.to/?url=%s&country=%s", url.QueryEscape(mediaURL), url.QueryEscape(country))

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
		return nil, fmt.Errorf("lucida returned HTTP %d", resp.StatusCode)
	}

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	html := string(bodyBytes)

	// Check if Lucida returned an explicit error toast in HTML
	if strings.Contains(html, `toast={"error":`) {
		errMarker := `toast={"error":"`
		if eIdx := strings.Index(html, errMarker); eIdx != -1 {
			eSub := html[eIdx+len(errMarker):]
			ke := eSub[:strings.Index(eSub, `"`)]
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

	var dlResp LucidaTrackDownloadResponse
	if err := json.NewDecoder(resp.Body).Decode(&dlResp); err != nil {
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

// pollLucidaDownloadStatus polls until Lucida finishes fetching the track.
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

	totalBytes := resp.ContentLength
	buf := make([]byte, 32*1024)
	var downloaded int64
	lastUpdate := time.Now()

	for {
		n, rErr := resp.Body.Read(buf)
		if n > 0 {
			if _, wErr := out.Write(buf[:n]); wErr != nil {
				return wErr
			}
			downloaded += int64(n)

			if isTerminalStdout && time.Since(lastUpdate) > 100*time.Millisecond {
				lastUpdate = time.Now()
				pct := 0.0
				if totalBytes > 0 {
					pct = float64(downloaded) / float64(totalBytes) * 100.0
				}
				idx := atomic.AddInt32(spinnerIdx, 1)
				drawProgressBar(int(idx), pct, "", "", "streaming…")
			}
		}
		if rErr != nil {
			if errors.Is(rErr, io.EOF) {
				break
			}
			return rErr
		}
	}

	clearLine()
	out.Close()

	_ = os.Remove(destPath)
	return os.Rename(tmpFile, destPath)
}

// downloadMediaCoverArt fetches cover art image from URL and writes to destPath.
func downloadMediaCoverArt(client *http.Client, cfg *Config, imageURL, destPath string) error {
	req, err := http.NewRequestWithContext(context.Background(), "GET", imageURL, nil)
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
		return fmt.Errorf("HTTP %d downloading cover", resp.StatusCode)
	}

	_ = os.MkdirAll(filepath.Dir(destPath), 0755)
	f, err := os.Create(destPath)
	if err != nil {
		return err
	}
	defer f.Close()

	_, err = io.Copy(f, resp.Body)
	return err
}

// ---------------------------------------------------------------------------
// FFmpeg Post-Processing Helpers
// ---------------------------------------------------------------------------

// embedCoverArtFFmpeg attaches cover artwork as a video stream to audio files.
func embedCoverArtFFmpeg(binDir, audioPath, coverPath string) error {
	if _, err := os.Stat(coverPath); err != nil {
		return nil // No cover to embed
	}
	ffmpegBin := filepath.Join(binDir, ffmpegBinaryName())
	ext := filepath.Ext(audioPath)
	tmpOut := strings.TrimSuffix(audioPath, ext) + ".tagged" + ext

	var cmd *exec.Cmd
	if strings.EqualFold(ext, ".mp3") {
		cmd = exec.Command(ffmpegBin,
			"-y",
			"-i", audioPath,
			"-i", coverPath,
			"-map", "0:a",
			"-map", "1:v",
			"-c", "copy",
			"-id3v2_version", "3",
			"-metadata:s:v", "title=Album cover",
			"-metadata:s:v", "comment=Cover (front)",
			tmpOut,
		)
	} else {
		cmd = exec.Command(ffmpegBin,
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
	}

	if err := cmd.Run(); err != nil {
		_ = os.Remove(tmpOut)
		return err
	}

	_ = os.Remove(audioPath)
	return os.Rename(tmpOut, audioPath)
}

// transcodeAudio converts an audio file to the target format (e.g. mp3) via FFmpeg.
func transcodeAudio(binDir, srcPath, targetFormat string) (string, error) {
	currentExt := strings.TrimPrefix(filepath.Ext(srcPath), ".")
	if strings.EqualFold(currentExt, targetFormat) {
		return srcPath, nil
	}

	destPath := strings.TrimSuffix(srcPath, filepath.Ext(srcPath)) + "." + targetFormat
	ffmpegBin := filepath.Join(binDir, ffmpegBinaryName())

	var args []string
	if strings.EqualFold(targetFormat, "mp3") {
		args = []string{"-y", "-i", srcPath, "-c:a", "libmp3lame", "-q:a", "0", destPath}
	} else {
		args = []string{"-y", "-i", srcPath, "-c:a", "copy", destPath}
	}

	cmd := exec.Command(ffmpegBin, args...)
	if err := cmd.Run(); err != nil {
		_ = os.Remove(destPath)
		return srcPath, fmt.Errorf("transcode to %s failed: %w", targetFormat, err)
	}

	_ = os.Remove(srcPath)
	return destPath, nil
}
