package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// ---------------------------------------------------------------------------
// Tidal URL Detection & Parsing
// ---------------------------------------------------------------------------

var tidalURLRe = regexp.MustCompile(
	`(?i)^(?:https?://)?(?:[a-z0-9\-]+\.)?tidal\.com/(?:[a-z]{2}/)?(?:browse/)?(?P<type>track|album|playlist|video|mix)/(?P<id>[a-zA-Z0-9_\-]+)`,
)

// isTidalURL returns true if the input URL matches Tidal web formats.
func isTidalURL(rawURL string) bool {
	return tidalURLRe.MatchString(rawURL)
}

type tidalURLInfo struct {
	Type string // "track", "album", "playlist", "video", "mix"
	ID   string
}

// parseTidalURL extracts resource type and ID from a Tidal URL.
func parseTidalURL(rawURL string) (*tidalURLInfo, error) {
	m := tidalURLRe.FindStringSubmatch(rawURL)
	if m == nil {
		return nil, fmt.Errorf("not a valid Tidal URL: %s", rawURL)
	}
	typeIdx := tidalURLRe.SubexpIndex("type")
	idIdx := tidalURLRe.SubexpIndex("id")
	return &tidalURLInfo{
		Type: strings.ToLower(m[typeIdx]),
		ID:   m[idIdx],
	}, nil
}

// ---------------------------------------------------------------------------
// Lucida Integration (Primary Proxy Engine)
// ---------------------------------------------------------------------------

type lucidaArtist struct {
	Name string `json:"name"`
}

type lucidaCoverArtwork struct {
	URL string `json:"url"`
}

type lucidaTrack struct {
	Title        string               `json:"title"`
	URL          string               `json:"url"`
	Artists      []lucidaArtist       `json:"artists"`
	Producers    []string             `json:"producers"`
	CSRF         string               `json:"csrf"`
	CSRFFallback string               `json:"csrfFallback"`
	CoverArtwork []lucidaCoverArtwork `json:"coverArtwork"`
	ReleaseDate  string               `json:"releaseDate"`
	TrackNumber  int                  `json:"trackNumber"`
}

type lucidaAlbumMeta struct {
	Title        string               `json:"title"`
	CoverArtwork []lucidaCoverArtwork `json:"coverArtwork"`
	Artists      []lucidaArtist       `json:"artists"`
	TrackCount   int                  `json:"trackCount"`
	ReleaseDate  string               `json:"releaseDate"`
	Tracks       []lucidaTrack        `json:"tracks"`
}

type lucidaInfoWrapper struct {
	Type         string               `json:"type"` // "album", "track", "playlist", "artist"
	Title        string               `json:"title"`
	CoverArtwork []lucidaCoverArtwork `json:"coverArtwork"`
	Artists      []lucidaArtist       `json:"artists"`
	TrackCount   int                  `json:"trackCount"`
	ReleaseDate  string               `json:"releaseDate"`
	Tracks       []lucidaTrack        `json:"tracks"`
	URL          string               `json:"url"`
	Album        *lucidaAlbumMeta     `json:"album"`
	Producers    []string             `json:"producers"`
	Name         string               `json:"name"`
}

type lucidaPageData struct {
	Info            lucidaInfoWrapper `json:"info"`
	OriginalService string            `json:"originalService"`
	Token           string            `json:"token"`
	TokenExpiry     int64             `json:"tokenExpiry"`
}

type lucidaAccount struct {
	ID   string `json:"id"`
	Type string `json:"type"`
}

type lucidaToken struct {
	Expiry    int64   `json:"expiry"`
	Primary   *string `json:"primary"`
	Secondary *string `json:"secondary"`
}

type lucidaUpload struct {
	Enabled bool `json:"enabled"`
}

type lucidaTrackDownloadRequest struct {
	Account   lucidaAccount `json:"account"`
	Compat    bool          `json:"compat"`
	Downscale string        `json:"downscale"`
	Handoff   bool          `json:"handoff"`
	Metadata  bool          `json:"metadata"`
	Private   bool          `json:"private"`
	Token     lucidaToken   `json:"token"`
	Upload    lucidaUpload  `json:"upload"`
	URL       string        `json:"url"`
}

type lucidaTrackDownloadResponse struct {
	Handoff string `json:"handoff"`
	Server  string `json:"server"`
	Error   string `json:"error"`
}

type lucidaDownloadStatusResponse struct {
	Status  string `json:"status"`
	Message string `json:"message"`
}

func newLucidaHTTPClient(cfg *Config) *http.Client {
	return &http.Client{
		Timeout: 45 * time.Second,
	}
}

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

type lucidaCFError struct{}

func (lucidaCFError) Error() string {
	return "Cloudflare captcha required by lucida.to"
}

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

func parseLucidaEnclosedJSON(html string) (*lucidaPageData, error) {
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

	var pageData lucidaPageData
	if err := json.Unmarshal([]byte(rawJSON), &pageData); err != nil {
		return nil, fmt.Errorf("failed to parse Lucida page JSON: %w", err)
	}
	return &pageData, nil
}

func resolveLucidaMetadata(client *http.Client, cfg *Config, targetURL string) (*lucidaPageData, error) {
	country := cfg.Country
	if country == "" {
		country = "auto"
	}

	endpoint := fmt.Sprintf("https://lucida.to/?url=%s&country=%s", url.QueryEscape(targetURL), url.QueryEscape(country))
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

func requestLucidaTrackHandoff(client *http.Client, cfg *Config, track *lucidaTrack, tokenExpiry int64) (*lucidaTrackDownloadResponse, error) {
	country := cfg.Country
	if country == "" {
		country = "auto"
	}

	var csrfPtr *string
	if track.CSRF != "" {
		csrfPtr = &track.CSRF
	}

	reqBody := lucidaTrackDownloadRequest{
		Account: lucidaAccount{
			ID:   country,
			Type: "country",
		},
		Compat:    false,
		Downscale: "original",
		Handoff:   true,
		Metadata:  true,
		Private:   false,
		Token: lucidaToken{
			Expiry:  tokenExpiry,
			Primary: csrfPtr,
		},
		Upload: lucidaUpload{Enabled: false},
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

	var dlResp lucidaTrackDownloadResponse
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

func pollLucidaDownloadStatus(client *http.Client, cfg *Config, server, handoff string) error {
	statusURL := fmt.Sprintf("https://%s.lucida.to/api/fetch/request/%s", server, handoff)
	maxRetries := 60
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
			var st lucidaDownloadStatusResponse
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
				drawProgressBar(int(idx), pct, "", "", "downloading FLAC stream…")
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

func downloadTidalCoverArt(client *http.Client, cfg *Config, coverURL, destCoverPath string) error {
	if coverURL == "" {
		return nil
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

func embedCoverArtFFmpeg(binDir, audioPath, coverPath string) error {
	if _, err := os.Stat(coverPath); err != nil {
		return nil
	}
	ffmpegBin := filepath.Join(binDir, ffmpegBinaryName())
	ext := filepath.Ext(audioPath)
	tmpOut := audioPath + ".tagged" + ext

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

func transcodeAudio(binDir, srcPath, targetFormat string) (string, error) {
	currentExt := strings.TrimPrefix(filepath.Ext(srcPath), ".")
	if strings.EqualFold(currentExt, targetFormat) {
		return srcPath, nil
	}

	destPath := strings.TrimSuffix(srcPath, filepath.Ext(srcPath)) + "." + targetFormat
	ffmpegBin := filepath.Join(binDir, ffmpegBinaryName())

	var args []string
	if targetFormat == "mp3" {
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

// tryLucidaTidalDownload attempts downloading Tidal content via Lucida.
func tryLucidaTidalDownload(cfg *Config, binDir string, client *http.Client, tidalURL string) ([]string, error) {
	if err := checkLucidaAvailability(client, cfg); err != nil {
		return nil, fmt.Errorf("lucida check failed: %w", err)
	}

	pageData, err := resolveLucidaMetadata(client, cfg, tidalURL)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve metadata via Lucida: %w", err)
	}

	info := pageData.Info
	var tracksToDownload []lucidaTrack

	isPlaylistMode := false
	var albumOrPlaylistTitle string

	switch strings.ToLower(info.Type) {
	case "album":
		isPlaylistMode = true
		albumOrPlaylistTitle = info.Title
		tracksToDownload = info.Tracks
	case "playlist":
		isPlaylistMode = true
		albumOrPlaylistTitle = info.Title
		tracksToDownload = info.Tracks
	case "track":
		tracksToDownload = []lucidaTrack{{
			Title:        info.Title,
			URL:          info.URL,
			Artists:      info.Artists,
			CoverArtwork: info.CoverArtwork,
			ReleaseDate:  info.ReleaseDate,
			TrackNumber:  1,
		}}
	default:
		if len(info.Tracks) > 0 {
			tracksToDownload = info.Tracks
			isPlaylistMode = len(info.Tracks) > 1
		} else {
			return nil, fmt.Errorf("unsupported or empty Lucida container: %s", info.Type)
		}
	}

	if len(tracksToDownload) == 0 {
		return nil, errors.New("no tracks found in Tidal item via Lucida")
	}

	var savedFiles []string
	var spinnerIdx int32

	for i, trk := range tracksToDownload {
		trackNum := trk.TrackNumber
		if trackNum == 0 {
			trackNum = i + 1
		}

		var artistNames []string
		for _, a := range trk.Artists {
			if a.Name != "" {
				artistNames = append(artistNames, a.Name)
			}
		}
		artistStr := strings.Join(artistNames, ", ")

		namingOpts := cfg.NamingOpts
		if isPlaylistMode && !cfg.ExplicitNumbering {
			namingOpts.IncludeNumbering = true
		}

		meta := FileMeta{
			Title:       trk.Title,
			Artist:      artistStr,
			Album:       albumOrPlaylistTitle,
			TrackNumber: trackNum,
			Extension:   "flac",
		}
		baseName := generateFilename(meta, namingOpts)
		destPath := filepath.Join(cfg.OutputDir, baseName)

		logPhase(fmt.Sprintf("[%d/%d] Fetching (Lucida): %s", i+1, len(tracksToDownload), trk.Title))

		handoffResp, err := requestLucidaTrackHandoff(client, cfg, &trk, pageData.TokenExpiry)
		if err != nil {
			logWarn(fmt.Sprintf("handoff failed for '%s': %v", trk.Title, err))
			continue
		}

		if err := pollLucidaDownloadStatus(client, cfg, handoffResp.Server, handoffResp.Handoff); err != nil {
			logWarn(fmt.Sprintf("processing error for '%s': %v", trk.Title, err))
			continue
		}

		if err := streamLucidaAudio(client, cfg, handoffResp.Server, handoffResp.Handoff, destPath, &spinnerIdx); err != nil {
			logWarn(fmt.Sprintf("streaming failed for '%s': %v", trk.Title, err))
			continue
		}

		// Artwork
		coverURL := ""
		if len(trk.CoverArtwork) > 0 {
			coverURL = trk.CoverArtwork[0].URL
		} else if len(info.CoverArtwork) > 0 {
			coverURL = info.CoverArtwork[0].URL
		}
		if coverURL != "" {
			tmpCover := destPath + ".cover.jpg"
			if err := downloadTidalCoverArt(client, cfg, coverURL, tmpCover); err == nil {
				_ = embedCoverArtFFmpeg(binDir, destPath, tmpCover)
				_ = os.Remove(tmpCover)
			}
		}

		// Transcode if mp3 requested
		if cfg.Format == "mp3" {
			if converted, tErr := transcodeAudio(binDir, destPath, "mp3"); tErr == nil {
				destPath = converted
			}
		}

		logOK(fmt.Sprintf("Saved: %s", filepath.Base(destPath)))
		savedFiles = append(savedFiles, destPath)
	}

	return savedFiles, nil
}

// ---------------------------------------------------------------------------
// Official Tidal API Client (Fallback Engine)
// ---------------------------------------------------------------------------

const (
	tidalClientID     = "fX2JxdmntZWK0ixT"
	tidalClientSecret = "1Nn9AfDAjxrgJFJbKNWLeAyKGVGmINuXPPLHVXAvxAg="
	tidalAuthBaseURL  = "https://auth.tidal.com/v1/oauth2"
	tidalAPIBaseURL   = "https://listen.tidal.com/v1"
)

type TidalSession struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int64  `json:"expires_in"`
	ExpiresAt    int64  `json:"expires_at"`
	UserID       int64  `json:"user_id,omitempty"`
}

type TidalDeviceAuthResponse struct {
	DeviceCode              string `json:"deviceCode"`
	UserCode                string `json:"userCode"`
	VerificationURI         string `json:"verificationUri"`
	VerificationURIComplete string `json:"verificationUriComplete"`
	ExpiresIn               int64  `json:"expiresIn"`
	Interval                int    `json:"interval"`
}

type TidalTokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int64  `json:"expires_in"`
	User         struct {
		UserID int64 `json:"userId"`
	} `json:"user"`
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
	SubStatus        int    `json:"sub_status"`
}

type TidalArtist struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
	Type string `json:"type"`
}

type TidalAlbumMeta struct {
	ID          int64         `json:"id"`
	Title       string        `json:"title"`
	Cover       string        `json:"cover"`
	ReleaseDate string        `json:"releaseDate"`
	Artists     []TidalArtist `json:"artists"`
}

type TidalTrackItem struct {
	ID           int64          `json:"id"`
	Title        string         `json:"title"`
	Duration     int            `json:"duration"`
	TrackNumber  int            `json:"trackNumber"`
	VolumeNumber int            `json:"volumeNumber"`
	Artist       TidalArtist    `json:"artist"`
	Artists      []TidalArtist  `json:"artists"`
	Album        TidalAlbumMeta `json:"album"`
}

type TidalAlbumResponse struct {
	ID          int64          `json:"id"`
	Title       string         `json:"title"`
	Cover       string         `json:"cover"`
	ReleaseDate string         `json:"releaseDate"`
	Artists     []TidalArtist  `json:"artists"`
	Items       TidalTrackList `json:"tracks"`
}

type TidalTrackList struct {
	Limit              int              `json:"limit"`
	Offset             int              `json:"offset"`
	TotalNumberOfItems int              `json:"totalNumberOfItems"`
	Items              []TidalTrackItem `json:"items"`
}

type TidalPlaylistItem struct {
	Item TidalTrackItem `json:"item"`
	Type string         `json:"type"`
}

type TidalPlaylistResponse struct {
	UUID        string `json:"uuid"`
	Title       string `json:"title"`
	Description string `json:"description"`
	Image       string `json:"image"`
	SquareImage string `json:"squareImage"`
}

type TidalPlaylistItemsResponse struct {
	Limit              int                 `json:"limit"`
	Offset             int                 `json:"offset"`
	TotalNumberOfItems int                 `json:"totalNumberOfItems"`
	Items              []TidalPlaylistItem `json:"items"`
}

type TidalPlaybackInfo struct {
	TrackID          int64  `json:"trackId"`
	AssetPresentation string `json:"assetPresentation"`
	AudioMode        string `json:"audioMode"`
	AudioQuality     string `json:"audioQuality"`
	ManifestMimeType string `json:"manifestMimeType"`
	Manifest         string `json:"manifest"` // Base64 encoded
}

type TidalBTSManifest struct {
	MimeType       string   `json:"mimeType"`
	Codecs         string   `json:"codecs"`
	EncryptionType string   `json:"encryptionType"`
	URLs           []string `json:"urls"`
}

// DASH XML structures
type DashMPD struct {
	XMLName xml.Name `xml:"MPD"`
	Period  struct {
		AdaptationSet struct {
			Representation struct {
				SegmentTemplate struct {
					Initialization string `xml:"initialization,attr"`
					Media          string `xml:"media,attr"`
					StartNumber    int    `xml:"startNumber,attr"`
					SegmentTimeline struct {
						S []struct {
							D int64 `xml:"d,attr"`
							R int   `xml:"r,attr"`
						} `xml:"S"`
					} `xml:"SegmentTimeline"`
				} `xml:"SegmentTemplate"`
			} `xml:"Representation"`
		} `xml:"AdaptationSet"`
	} `xml:"Period"`
}

func getTidalSessionPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "tidal_session.json"
	}
	dir := filepath.Join(home, ".fetchit")
	_ = os.MkdirAll(dir, 0755)
	return filepath.Join(dir, "tidal_session.json")
}

func loadTidalSession() (*TidalSession, error) {
	path := getTidalSessionPath()
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var sess TidalSession
	if err := json.Unmarshal(data, &sess); err != nil {
		return nil, err
	}
	return &sess, nil
}

func saveTidalSession(sess *TidalSession) error {
	path := getTidalSessionPath()
	data, err := json.MarshalIndent(sess, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0600)
}

func basicAuthHeader(clientID, clientSecret string) string {
	raw := fmt.Sprintf("%s:%s", clientID, clientSecret)
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(raw))
}

func refreshTidalToken(sess *TidalSession) (*TidalSession, error) {
	formData := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {sess.RefreshToken},
		"client_id":     {tidalClientID},
	}

	req, err := http.NewRequest("POST", tidalAuthBaseURL+"/token", strings.NewReader(formData.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Authorization", basicAuthHeader(tidalClientID, tidalClientSecret))

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("token refresh HTTP %d", resp.StatusCode)
	}

	var tr TidalTokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&tr); err != nil {
		return nil, err
	}

	sess.AccessToken = tr.AccessToken
	if tr.RefreshToken != "" {
		sess.RefreshToken = tr.RefreshToken
	}
	sess.ExpiresIn = tr.ExpiresIn
	sess.ExpiresAt = time.Now().Unix() + tr.ExpiresIn
	if tr.User.UserID != 0 {
		sess.UserID = tr.User.UserID
	}

	_ = saveTidalSession(sess)
	return sess, nil
}

func performDeviceAuthorization() (*TidalSession, error) {
	formData := url.Values{
		"client_id": {tidalClientID},
		"scope":     {"r_usr w_usr w_sub"},
	}

	req, err := http.NewRequest("POST", tidalAuthBaseURL+"/device_authorization", strings.NewReader(formData.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("device auth request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("device auth returned HTTP %d", resp.StatusCode)
	}

	var dResp TidalDeviceAuthResponse
	if err := json.NewDecoder(resp.Body).Decode(&dResp); err != nil {
		return nil, err
	}

	verifyLink := dResp.VerificationURIComplete
	if verifyLink == "" {
		verifyLink = fmt.Sprintf("https://%s/%s", dResp.VerificationURI, dResp.UserCode)
	}

	fmt.Printf("\n%s╭────────────────────────────────────────────────────────────╮%s\n", colAccent, colReset)
	fmt.Printf("%s│%s %sTIDAL Official Device Authorization Required%s             %s│%s\n", colAccent, colReset, colBold, colReset, colAccent, colReset)
	fmt.Printf("%s│%s 1. Open: %s%-47s%s %s│%s\n", colAccent, colReset, colAccent, verifyLink, colReset, colAccent, colReset)
	fmt.Printf("%s│%s 2. Enter code if prompted: %s%-31s%s %s│%s\n", colAccent, colReset, colBold, dResp.UserCode, colReset, colAccent, colReset)
	fmt.Printf("%s╰────────────────────────────────────────────────────────────╯%s\n\n", colAccent, colReset)

	logPhase("Waiting for authorization in browser…")

	pollInterval := time.Duration(dResp.Interval) * time.Second
	if pollInterval < 2*time.Second {
		pollInterval = 5 * time.Second
	}
	deadline := time.Now().Add(time.Duration(dResp.ExpiresIn) * time.Second)

	for time.Now().Before(deadline) {
		time.Sleep(pollInterval)

		pollForm := url.Values{
			"client_id":   {tidalClientID},
			"device_code": {dResp.DeviceCode},
			"grant_type":  {"urn:ietf:params:oauth:grant-type:device_code"},
			"scope":       {"r_usr w_usr w_sub"},
		}

		pReq, err := http.NewRequest("POST", tidalAuthBaseURL+"/token", strings.NewReader(pollForm.Encode()))
		if err != nil {
			continue
		}
		pReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		pReq.Header.Set("Authorization", basicAuthHeader(tidalClientID, tidalClientSecret))

		pResp, err := client.Do(pReq)
		if err != nil {
			continue
		}

		var tr TidalTokenResponse
		_ = json.NewDecoder(pResp.Body).Decode(&tr)
		pResp.Body.Close()

		if pResp.StatusCode == http.StatusOK && tr.AccessToken != "" {
			logOK("TIDAL device authorized successfully!")
			sess := &TidalSession{
				AccessToken:  tr.AccessToken,
				RefreshToken: tr.RefreshToken,
				TokenType:    tr.TokenType,
				ExpiresIn:    tr.ExpiresIn,
				ExpiresAt:    time.Now().Unix() + tr.ExpiresIn,
				UserID:       tr.User.UserID,
			}
			_ = saveTidalSession(sess)
			return sess, nil
		}

		// sub_status 1002 means authorization pending
		if tr.SubStatus == 1002 || strings.Contains(tr.Error, "authorization_pending") {
			continue
		}

		if tr.Error != "" && !strings.Contains(tr.Error, "pending") {
			return nil, fmt.Errorf("TIDAL auth failed: %s (%s)", tr.Error, tr.ErrorDescription)
		}
	}

	return nil, errors.New("device authorization timed out")
}

func getTidalAccessToken() (string, error) {
	sess, err := loadTidalSession()
	if err == nil && sess != nil && sess.AccessToken != "" {
		if time.Now().Unix() < sess.ExpiresAt-60 {
			return sess.AccessToken, nil
		}
		if sess.RefreshToken != "" {
			if refreshed, rErr := refreshTidalToken(sess); rErr == nil {
				return refreshed.AccessToken, nil
			}
		}
	}

	newSess, err := performDeviceAuthorization()
	if err != nil {
		return "", err
	}
	return newSess.AccessToken, nil
}

func fetchTidalTrack(client *http.Client, token, country string, trackID string) (*TidalTrackItem, error) {
	if country == "" || country == "auto" {
		country = "US"
	}
	endpoint := fmt.Sprintf("%s/tracks/%s?countryCode=%s", tidalAPIBaseURL, trackID, country)
	req, err := http.NewRequest("GET", endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("track endpoint HTTP %d", resp.StatusCode)
	}

	var track TidalTrackItem
	if err := json.NewDecoder(resp.Body).Decode(&track); err != nil {
		return nil, err
	}
	return &track, nil
}

func fetchTidalAlbumTracks(client *http.Client, token, country, albumID string) (string, []TidalTrackItem, error) {
	if country == "" || country == "auto" {
		country = "US"
	}
	endpoint := fmt.Sprintf("%s/albums/%s?countryCode=%s", tidalAPIBaseURL, albumID, country)
	req, err := http.NewRequest("GET", endpoint, nil)
	if err != nil {
		return "", nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := client.Do(req)
	if err != nil {
		return "", nil, err
	}
	defer resp.Body.Close()

	var album TidalAlbumResponse
	if err := json.NewDecoder(resp.Body).Decode(&album); err != nil {
		return "", nil, err
	}

	// Fetch tracks
	tracksEndpoint := fmt.Sprintf("%s/albums/%s/tracks?countryCode=%s&limit=100", tidalAPIBaseURL, albumID, country)
	tReq, err := http.NewRequest("GET", tracksEndpoint, nil)
	if err != nil {
		return album.Title, nil, err
	}
	tReq.Header.Set("Authorization", "Bearer "+token)

	tResp, err := client.Do(tReq)
	if err != nil {
		return album.Title, nil, err
	}
	defer tResp.Body.Close()

	var trackList TidalTrackList
	if err := json.NewDecoder(tResp.Body).Decode(&trackList); err != nil {
		return album.Title, nil, err
	}

	return album.Title, trackList.Items, nil
}

func fetchTidalPlaylistTracks(client *http.Client, token, country, playlistUUID string) (string, []TidalTrackItem, error) {
	if country == "" || country == "auto" {
		country = "US"
	}
	endpoint := fmt.Sprintf("%s/playlists/%s?countryCode=%s", tidalAPIBaseURL, playlistUUID, country)
	req, err := http.NewRequest("GET", endpoint, nil)
	if err != nil {
		return "", nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := client.Do(req)
	if err != nil {
		return "", nil, err
	}
	defer resp.Body.Close()

	var pl TidalPlaylistResponse
	if err := json.NewDecoder(resp.Body).Decode(&pl); err != nil {
		return "", nil, err
	}

	var tracks []TidalTrackItem
	offset := 0
	limit := 100

	for {
		itemsEndpoint := fmt.Sprintf("%s/playlists/%s/items?countryCode=%s&limit=%d&offset=%d", tidalAPIBaseURL, playlistUUID, country, limit, offset)
		iReq, err := http.NewRequest("GET", itemsEndpoint, nil)
		if err != nil {
			break
		}
		iReq.Header.Set("Authorization", "Bearer "+token)

		iResp, err := client.Do(iReq)
		if err != nil {
			break
		}

		var itemsResp TidalPlaylistItemsResponse
		_ = json.NewDecoder(iResp.Body).Decode(&itemsResp)
		iResp.Body.Close()

		if len(itemsResp.Items) == 0 {
			break
		}

		for _, item := range itemsResp.Items {
			if item.Item.ID != 0 {
				tracks = append(tracks, item.Item)
			}
		}

		offset += len(itemsResp.Items)
		if offset >= itemsResp.TotalNumberOfItems || len(itemsResp.Items) < limit {
			break
		}
	}

	return pl.Title, tracks, nil
}

func mapTidalQuality(cfgQuality, cfgFormat string) string {
	if cfgFormat == "mp3" || cfgQuality == "128" {
		return "LOW"
	}
	if cfgQuality == "320" {
		return "HIGH"
	}
	if strings.EqualFold(cfgQuality, "hi-res") || strings.EqualFold(cfgQuality, "hires") {
		return "HI_RES_LOSSLESS"
	}
	return "LOSSLESS"
}

func parseDashManifest(manifestXML string) ([]string, error) {
	var mpd DashMPD
	if err := xml.Unmarshal([]byte(manifestXML), &mpd); err != nil {
		return nil, fmt.Errorf("xml unmarshal error: %w", err)
	}

	tpl := mpd.Period.AdaptationSet.Representation.SegmentTemplate
	initPath := tpl.Initialization
	mediaTemplate := tpl.Media
	startNum := tpl.StartNumber
	if startNum <= 0 {
		startNum = 1
	}

	if initPath == "" || mediaTemplate == "" {
		return nil, errors.New("missing initialization or media in SegmentTemplate")
	}

	var segmentURLs []string
	segmentURLs = append(segmentURLs, initPath)

	currentNum := startNum
	for _, s := range tpl.SegmentTimeline.S {
		reps := s.R
		if reps < 0 {
			reps = 0
		}
		for i := 0; i <= reps; i++ {
			urlStr := strings.ReplaceAll(mediaTemplate, "$Number$", strconv.Itoa(currentNum))
			segmentURLs = append(segmentURLs, urlStr)
			currentNum++
		}
	}

	return segmentURLs, nil
}

func downloadOfficialTidalTrack(client *http.Client, cfg *Config, token string, track *TidalTrackItem, destPath string, spinnerIdx *int32) error {
	quality := mapTidalQuality(cfg.Quality, cfg.Format)
	endpoint := fmt.Sprintf("%s/tracks/%d/playbackinfo?audioquality=%s&playbackmode=STREAM&assetpresentation=FULL",
		tidalAPIBaseURL, track.ID, quality)

	req, err := http.NewRequest("GET", endpoint, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("playbackinfo HTTP %d", resp.StatusCode)
	}

	var pb TidalPlaybackInfo
	if err := json.NewDecoder(resp.Body).Decode(&pb); err != nil {
		return err
	}

	manifestBytes, err := base64.StdEncoding.DecodeString(pb.Manifest)
	if err != nil {
		return fmt.Errorf("manifest base64 decode failed: %w", err)
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

	manifestStr := string(manifestBytes)

	// Check if manifest is BTS JSON or DASH XML
	if strings.HasPrefix(strings.TrimSpace(manifestStr), "{") {
		var bts TidalBTSManifest
		if err := json.Unmarshal(manifestBytes, &bts); err != nil {
			return fmt.Errorf("failed to parse BTS manifest: %w", err)
		}
		if len(bts.URLs) == 0 {
			return errors.New("empty stream URLs in BTS manifest")
		}

		streamReq, err := http.NewRequest("GET", bts.URLs[0], nil)
		if err != nil {
			return err
		}
		sResp, err := client.Do(streamReq)
		if err != nil {
			return err
		}
		defer sResp.Body.Close()

		if sResp.StatusCode != http.StatusOK {
			return fmt.Errorf("stream fetch HTTP %d", sResp.StatusCode)
		}

		totalSize := sResp.ContentLength
		var downloaded int64
		buf := make([]byte, 64*1024)
		for {
			n, rErr := sResp.Body.Read(buf)
			if n > 0 {
				if _, wErr := out.Write(buf[:n]); wErr != nil {
					return wErr
				}
				downloaded += int64(n)
				if totalSize > 0 && isTerminalStdout && !cfg.Verbose {
					pct := float64(downloaded) / float64(totalSize) * 100.0
					idx := atomic.AddInt32(spinnerIdx, 1)
					drawProgressBar(int(idx), pct, "", "", "downloading TIDAL stream…")
				}
			}
			if rErr != nil {
				if errors.Is(rErr, io.EOF) {
					break
				}
				return rErr
			}
		}
	} else {
		// DASH MPD XML
		segments, err := parseDashManifest(manifestStr)
		if err != nil {
			return fmt.Errorf("failed to parse DASH manifest: %w", err)
		}

		totalSegs := len(segments)
		for sIdx, segURL := range segments {
			segReq, err := http.NewRequest("GET", segURL, nil)
			if err != nil {
				return err
			}
			segResp, err := client.Do(segReq)
			if err != nil {
				return err
			}

			if segResp.StatusCode != http.StatusOK {
				segResp.Body.Close()
				return fmt.Errorf("segment %d HTTP %d", sIdx, segResp.StatusCode)
			}

			_, cErr := io.Copy(out, segResp.Body)
			segResp.Body.Close()
			if cErr != nil {
				return cErr
			}

			if isTerminalStdout && !cfg.Verbose {
				pct := float64(sIdx+1) / float64(totalSegs) * 100.0
				idx := atomic.AddInt32(spinnerIdx, 1)
				drawProgressBar(int(idx), pct, "", "", fmt.Sprintf("chunk %d/%d…", sIdx+1, totalSegs))
			}
		}
	}

	out.Close()
	clearLine()

	return os.Rename(tmpFile, destPath)
}

func getTidalCoverURL(coverUUID string) string {
	if coverUUID == "" {
		return ""
	}
	formatted := strings.ReplaceAll(coverUUID, "-", "/")
	return fmt.Sprintf("https://resources.tidal.com/images/%s/1280x1280.jpg", formatted)
}

// runOfficialTidalDownload handles Tidal downloads using Tidal's official API.
func runOfficialTidalDownload(cfg *Config, binDir, tidalURL string) ([]string, error) {
	client := &http.Client{Timeout: 45 * time.Second}

	token, err := getTidalAccessToken()
	if err != nil {
		return nil, fmt.Errorf("official TIDAL authentication failed: %w", err)
	}

	parsed, err := parseTidalURL(tidalURL)
	if err != nil {
		return nil, err
	}

	var tracksToDownload []TidalTrackItem
	var albumOrPlaylistTitle string
	isPlaylistMode := false

	country := cfg.Country
	if country == "" || country == "auto" {
		country = "US"
	}

	switch parsed.Type {
	case "track":
		trk, err := fetchTidalTrack(client, token, country, parsed.ID)
		if err != nil {
			return nil, fmt.Errorf("failed to fetch track: %w", err)
		}
		tracksToDownload = []TidalTrackItem{*trk}
	case "album":
		isPlaylistMode = true
		title, trks, err := fetchTidalAlbumTracks(client, token, country, parsed.ID)
		if err != nil {
			return nil, fmt.Errorf("failed to fetch album: %w", err)
		}
		albumOrPlaylistTitle = title
		tracksToDownload = trks
	case "playlist":
		isPlaylistMode = true
		title, trks, err := fetchTidalPlaylistTracks(client, token, country, parsed.ID)
		if err != nil {
			return nil, fmt.Errorf("failed to fetch playlist: %w", err)
		}
		albumOrPlaylistTitle = title
		tracksToDownload = trks
	default:
		return nil, fmt.Errorf("unsupported Tidal type: %s", parsed.Type)
	}

	if len(tracksToDownload) == 0 {
		return nil, errors.New("no tracks retrieved from TIDAL")
	}

	var savedFiles []string
	var spinnerIdx int32

	for i, trk := range tracksToDownload {
		trackNum := trk.TrackNumber
		if trackNum == 0 {
			trackNum = i + 1
		}

		var artistNames []string
		for _, a := range trk.Artists {
			if a.Name != "" {
				artistNames = append(artistNames, a.Name)
			}
		}
		if len(artistNames) == 0 && trk.Artist.Name != "" {
			artistNames = append(artistNames, trk.Artist.Name)
		}
		artistStr := strings.Join(artistNames, ", ")

		namingOpts := cfg.NamingOpts
		if isPlaylistMode && !cfg.ExplicitNumbering {
			namingOpts.IncludeNumbering = true
		}

		meta := FileMeta{
			Title:       trk.Title,
			Artist:      artistStr,
			Album:       albumOrPlaylistTitle,
			TrackNumber: trackNum,
			Extension:   "flac",
		}
		baseName := generateFilename(meta, namingOpts)
		destPath := filepath.Join(cfg.OutputDir, baseName)

		logPhase(fmt.Sprintf("[%d/%d] Fetching (Official API): %s", i+1, len(tracksToDownload), trk.Title))

		if err := downloadOfficialTidalTrack(client, cfg, token, &trk, destPath, &spinnerIdx); err != nil {
			logWarn(fmt.Sprintf("download failed for '%s': %v", trk.Title, err))
			continue
		}

		// Artwork
		coverUUID := trk.Album.Cover
		if coverUUID != "" {
			coverURL := getTidalCoverURL(coverUUID)
			tmpCover := destPath + ".cover.jpg"
			if err := downloadTidalCoverArt(client, cfg, coverURL, tmpCover); err == nil {
				_ = embedCoverArtFFmpeg(binDir, destPath, tmpCover)
				_ = os.Remove(tmpCover)
			}
		}

		// Transcode if mp3 requested
		if cfg.Format == "mp3" {
			if converted, tErr := transcodeAudio(binDir, destPath, "mp3"); tErr == nil {
				destPath = converted
			}
		}

		logOK(fmt.Sprintf("Saved: %s", filepath.Base(destPath)))
		savedFiles = append(savedFiles, destPath)
	}

	return savedFiles, nil
}

// ---------------------------------------------------------------------------
// Main Tidal Orchestrator (Lucida first, fallback to Official API)
// ---------------------------------------------------------------------------

// runTidalDownload handles Tidal downloads by trying Lucida first, falling back to Official API.
func runTidalDownload(cfg *Config, binDir, rawURL string) ([]string, error) {
	lucidaClient := newLucidaHTTPClient(cfg)

	// Step 1: Attempt via Lucida proxy
	logPhase(fmt.Sprintf("Attempting Lucida engine for %s…", rawURL))
	saved, err := tryLucidaTidalDownload(cfg, binDir, lucidaClient, rawURL)
	if err == nil && len(saved) > 0 {
		return saved, nil
	}

	// Step 2: Fallback to Official Tidal API
	var cfErr lucidaCFError
	if errors.As(err, &cfErr) {
		logWarn("lucida.to is blocked by Cloudflare captcha")
	} else if err != nil {
		logWarn(fmt.Sprintf("Lucida unavailable (%v)", err))
	}
	logPhase("Falling back to official TIDAL API engine…")

	return runOfficialTidalDownload(cfg, binDir, rawURL)
}
