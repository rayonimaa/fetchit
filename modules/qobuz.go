package modules

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// ---------------------------------------------------------------------------
// Qobuz URL Detection & Parsing
// ---------------------------------------------------------------------------

var qobuzURLRe = regexp.MustCompile(
	`(?i)^(?:https?://)?(?:[a-z0-9\-]+\.)?qobuz\.com/(?:[a-z]{2}-[a-z]{2}/)?(?P<type>album|track|playlist|interpreter)/(?:[^/?#]+/)?(?P<id>[a-zA-Z0-9_\-]+)/?(?:[?#].*)?$`,
)

// isQobuzURL checks if the provided URL is a Qobuz track, album, playlist, or interpreter link.
func isQobuzURL(rawURL string) bool {
	return qobuzURLRe.MatchString(rawURL)
}

type qobuzURLInfo struct {
	Type string // "track", "album", "playlist", "interpreter"
	ID   string
}

// parseQobuzURL extracts resource type and ID from a Qobuz URL.
func parseQobuzURL(rawURL string) (*qobuzURLInfo, error) {
	m := qobuzURLRe.FindStringSubmatch(rawURL)
	if m == nil {
		return nil, fmt.Errorf("not a valid Qobuz URL: %s", rawURL)
	}
	typeIdx := qobuzURLRe.SubexpIndex("type")
	idIdx := qobuzURLRe.SubexpIndex("id")
	return &qobuzURLInfo{
		Type: strings.ToLower(m[typeIdx]),
		ID:   m[idIdx],
	}, nil
}

// ---------------------------------------------------------------------------
// Lucida Engine (Proxy)
// ---------------------------------------------------------------------------

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
	return downloadMediaCoverArt(client, cfg, coverURL, destCoverPath)
}

func tryLucidaQobuzDownload(cfg *Config, binDir string, client *http.Client, qobuzURL string) ([]string, error) {
	// Step 1: Check Lucida availability
	if err := checkLucidaAvailability(client, cfg); err != nil {
		return nil, fmt.Errorf("lucida availability check failed: %w", err)
	}

	// Step 2: Resolve album / track / playlist metadata
	pageData, err := resolveLucidaMetadata(client, cfg, qobuzURL)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve Qobuz metadata via Lucida: %w", err)
	}

	info := pageData.Info
	if info.Type == "artist" || info.Type == "interpreter" {
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
			// Fix: Lucida track JSON inside containers often lacks track.CSRF; fall back to pageData.Token
			if tracksToDownload[i].CSRF == "" {
				tracksToDownload[i].CSRF = pageData.Token
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
		return nil, errors.New("no downloadable tracks found in Qobuz metadata via Lucida")
	}

	isMultiTrack := len(tracksToDownload) > 1
	logSubKV("album/playlist", truncateString(albumTitle, 40))
	logSubKV("artist/curator", truncateString(primaryArtist, 40))
	logSubKV("tracks", fmt.Sprintf("%d", len(tracksToDownload)))
	logSubKV("format", "Hi-Res FLAC (Lossless 24-bit)")

	// Prepare output directory
	outDir := cfg.OutputDir
	if isMultiTrack && albumTitle != "" {
		outDir = filepath.Join(outDir, sanitizeFilename(albumTitle))
	}
	_ = os.MkdirAll(outDir, 0755)

	var savedAudioFiles []string
	coverFilePath := filepath.Join(outDir, "cover.jpg")
	coverDownloaded := false
	if coverURL != "" {
		if err := downloadQobuzCoverArt(client, cfg, coverURL, coverFilePath); err == nil {
			coverDownloaded = true
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

		ext := ".flac"
		if strings.ToLower(cfg.Format) == "mp3" {
			ext = ".mp3"
		}

		// Apply Dynamic File Naming Engine
		meta := FileMeta{
			Title:       track.Title,
			Artist:      artistName,
			Album:       albumTitle,
			TrackNumber: trackNum,
			Extension:   ext,
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
			savedAudioFiles = append(savedAudioFiles, destPath)
			continue
		}

		// Request handoff
		handoffResp, err := requestLucidaTrackHandoff(client, cfg, &track, pageData.TokenExpiry)
		if err != nil {
			failCount++
			if len(tracksToDownload) == 1 || (i == 0 && len(tracksToDownload) > 1) {
				return nil, fmt.Errorf("track handoff failed: %w", err)
			}
			if cfg.Verbose {
				logErr(fmt.Sprintf("handoff failed: %v", err))
			} else {
				logErr("track handoff failed — skipping")
			}
			continue
		}

		// Poll status
		if err := pollLucidaDownloadStatus(client, cfg, handoffResp.Server, handoffResp.Handoff); err != nil {
			failCount++
			if len(tracksToDownload) == 1 || (i == 0 && len(tracksToDownload) > 1) {
				return nil, fmt.Errorf("server processing failed: %w", err)
			}
			if cfg.Verbose {
				logErr(fmt.Sprintf("processing error: %v", err))
			} else {
				logErr("server processing failed — skipping")
			}
			continue
		}

		// Download to FLAC first if MP3 transcode will be performed
		downloadPath := destPath
		if strings.ToLower(cfg.Format) == "mp3" {
			downloadPath = strings.TrimSuffix(destPath, ".mp3") + ".flac"
		}

		// Stream audio
		if err := streamLucidaAudio(client, cfg, handoffResp.Server, handoffResp.Handoff, downloadPath, &spinnerIdx); err != nil {
			failCount++
			if len(tracksToDownload) == 1 || (i == 0 && len(tracksToDownload) > 1) {
				return nil, fmt.Errorf("audio stream failed: %w", err)
			}
			if cfg.Verbose {
				logErr(fmt.Sprintf("download failed: %v", err))
			} else {
				logErr("audio download failed — skipping")
			}
			continue
		}

		// Transcode to MP3 if requested
		if strings.ToLower(cfg.Format) == "mp3" {
			if converted, tErr := transcodeAudio(binDir, downloadPath, "mp3"); tErr == nil {
				destPath = converted
			} else {
				destPath = downloadPath
			}
		}

		// Embed cover art with FFmpeg
		if _, err := os.Stat(coverFilePath); err == nil {
			_ = embedCoverArtFFmpeg(binDir, destPath, coverFilePath)
		}

		cleanupIntermediateFiles(destPath)
		logOK(fmt.Sprintf("saved: %s", finalFilename))
		savedAudioFiles = append(savedAudioFiles, destPath)
	}

	if len(savedAudioFiles) == 0 {
		return nil, fmt.Errorf("%d/%d tracks failed via Lucida proxy", failCount, len(tracksToDownload))
	}

	var allFiles []string
	if coverDownloaded {
		allFiles = append(allFiles, coverFilePath)
	}
	allFiles = append(allFiles, savedAudioFiles...)
	return allFiles, nil
}

// ---------------------------------------------------------------------------
// Official Qobuz API Engine (Fallback Bail-In)
// ---------------------------------------------------------------------------

const (
	qobuzAPIBaseURL       = "https://www.qobuz.com/api.json/0.2"
	defaultQobuzAppID     = "712109809"
	defaultQobuzAppSecret = "b5a96f62b0821c81ef841cf43c683b58"
)

// QobuzSession stores user authentication and app credentials.
type QobuzSession struct {
	AppID         string `json:"app_id"`
	AppSecret     string `json:"app_secret,omitempty"`
	UserAuthToken string `json:"user_auth_token,omitempty"`
	UserID        int64  `json:"user_id,omitempty"`
	UserEmail     string `json:"user_email,omitempty"`
}

type QobuzTrackItem struct {
	ID          int64  `json:"id"`
	Title       string `json:"title"`
	Duration    int    `json:"duration"`
	TrackNumber int    `json:"track_number"`
	Performer   struct {
		Name string `json:"name"`
	} `json:"performer"`
	Artist struct {
		Name string `json:"name"`
	} `json:"artist"`
	Album struct {
		Title string `json:"title"`
		Image struct {
			Large     string `json:"large"`
			Small     string `json:"small"`
			Thumbnail string `json:"thumbnail"`
		} `json:"image"`
	} `json:"album"`
}

type QobuzAlbumResponse struct {
	ID     string `json:"id"`
	Title  string `json:"title"`
	Artist struct {
		Name string `json:"name"`
	} `json:"artist"`
	Image struct {
		Large     string `json:"large"`
		Small     string `json:"small"`
		Thumbnail string `json:"thumbnail"`
	} `json:"image"`
	Tracks struct {
		Items []QobuzTrackItem `json:"items"`
		Total int              `json:"total"`
	} `json:"tracks"`
}

type QobuzPlaylistResponse struct {
	ID    int64  `json:"id"`
	Title string `json:"title"`
	Owner struct {
		Name string `json:"name"`
	} `json:"owner"`
	Tracks struct {
		Items []QobuzTrackItem `json:"items"`
		Total int              `json:"total"`
	} `json:"tracks"`
}

type QobuzFileUrlResponse struct {
	URL          string  `json:"url"`
	TrackID      int64   `json:"track_id"`
	FormatID     int     `json:"format_id"`
	MimeType     string  `json:"mime_type"`
	SamplingRate float64 `json:"sampling_rate"`
	BitDepth     int     `json:"bit_depth"`
	Error        string  `json:"error,omitempty"`
}

func getQobuzSessionPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "qobuz_session.json"
	}
	dir := filepath.Join(home, ".fetchit")
	_ = os.MkdirAll(dir, 0755)
	return filepath.Join(dir, "qobuz_session.json")
}

func loadQobuzSession() (*QobuzSession, error) {
	path := getQobuzSessionPath()
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var sess QobuzSession
	if err := json.Unmarshal(data, &sess); err != nil {
		return nil, err
	}
	return &sess, nil
}

func saveQobuzSession(sess *QobuzSession) error {
	path := getQobuzSessionPath()
	data, err := json.MarshalIndent(sess, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0600)
}

func getActiveQobuzSession(cfg *Config) *QobuzSession {
	sess, _ := loadQobuzSession()
	if sess == nil {
		sess = &QobuzSession{}
	}

	if envAppID := os.Getenv("QOBUZ_APP_ID"); envAppID != "" {
		sess.AppID = envAppID
	}
	if envSecret := os.Getenv("QOBUZ_APP_SECRET"); envSecret != "" {
		sess.AppSecret = envSecret
	}
	if envToken := os.Getenv("QOBUZ_USER_AUTH_TOKEN"); envToken != "" {
		sess.UserAuthToken = envToken
	}

	if sess.AppID == "" {
		sess.AppID = defaultQobuzAppID
	}
	if sess.AppSecret == "" {
		sess.AppSecret = defaultQobuzAppSecret
	}

	return sess
}

func computeQobuzRequestSig(formatID string, trackID int64, ts, appSecret string) string {
	raw := fmt.Sprintf("trackgetFileUrlformat_id%sintentstreamtrack_id%d%s%s", formatID, trackID, ts, appSecret)
	hash := md5.Sum([]byte(raw))
	return hex.EncodeToString(hash[:])
}

func loginQobuz(client *http.Client, session *QobuzSession, email, password string) error {
	formData := url.Values{
		"username": {email},
		"password": {password},
		"app_id":   {session.AppID},
	}
	resp, err := client.PostForm(qobuzAPIBaseURL+"/user/login", formData)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("login failed with HTTP %d", resp.StatusCode)
	}

	var lr struct {
		UserAuthToken string `json:"user_auth_token"`
		User          struct {
			ID    int64  `json:"id"`
			Email string `json:"email"`
		} `json:"user"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&lr); err != nil {
		return err
	}
	if lr.UserAuthToken == "" {
		return errors.New("no user_auth_token returned")
	}

	session.UserAuthToken = lr.UserAuthToken
	session.UserID = lr.User.ID
	session.UserEmail = lr.User.Email
	_ = saveQobuzSession(session)
	return nil
}

func ensureQobuzAuth(client *http.Client, session *QobuzSession) error {
	if session.UserAuthToken != "" {
		return nil
	}

	// 1. Check environment variables
	if email := os.Getenv("QOBUZ_EMAIL"); email != "" {
		if pass := os.Getenv("QOBUZ_PASSWORD"); pass != "" {
			logPhase("Logging in to Qobuz API with environment credentials…")
			if lErr := loginQobuz(client, session, email, pass); lErr != nil {
				return fmt.Errorf("Qobuz login failed: %w", lErr)
			}
			logOK("Qobuz authenticated successfully via environment credentials")
			return nil
		}
	}

	// 2. Interactive prompt if terminal attached
	if isTerminalStdin && isTerminalStdout {
		fmt.Printf("\r\n")
		fmt.Printf("\n%s╭────────────────────────────────────────────────────────────╮%s\n", colAccent, colReset)
		fmt.Printf("%s│%s %sQobuz Official API Authorization Required%s                  %s│%s\n", colAccent, colReset, colBold, colReset, colAccent, colReset)
		fmt.Printf("%s╰────────────────────────────────────────────────────────────╯%s\n", colAccent, colReset)
		choice, err := runCheckbox("Authentication methode:", []string{
			"Log in with Email & Password",
			"Enter User Auth Token (from web player)",
			"Cancel",
		}, 0)
		if err != nil {
			return err
		}
		fmt.Printf("\r\n")

		switch choice {
		case "Log in with Email & Password":
			email, err := runWizardText("Qobuz Email", "")
			if err != nil {
				return err
			}
			pass, err := runWizardText("Qobuz Password", "")
			if err != nil {
				return err
			}
			if email == "" || pass == "" {
				return errors.New("empty credentials provided")
			}
			logPhase("Authenticating with Qobuz…")
			if err := loginQobuz(client, session, email, pass); err != nil {
				return fmt.Errorf("Qobuz login failed: %w", err)
			}
			logOK(fmt.Sprintf("Logged in as %s (session saved)", email))
			return nil
		case "Enter User Auth Token (from web player)":
			tok, err := runWizardText("User Auth Token", "")
			if err != nil {
				return err
			}
			if tok == "" {
				return errors.New("empty auth token provided")
			}
			session.UserAuthToken = tok
			_ = saveQobuzSession(session)
			logOK("Token saved to ~/.fetchit/qobuz_session.json")
			return nil
		default:
			return errors.New("official Qobuz authentication was skipped")
		}
	}

	return fmt.Errorf("official Qobuz API requires credentials: set QOBUZ_USER_AUTH_TOKEN or QOBUZ_EMAIL/QOBUZ_PASSWORD or run interactively to authenticate")
}

func fetchQobuzTrackOfficial(client *http.Client, session *QobuzSession, trackID string) (*QobuzTrackItem, error) {
	endpoint := fmt.Sprintf("%s/track/get?track_id=%s", qobuzAPIBaseURL, trackID)
	req, err := http.NewRequestWithContext(context.Background(), "GET", endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-App-Id", session.AppID)
	if session.UserAuthToken != "" {
		req.Header.Set("X-User-Auth-Token", session.UserAuthToken)
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("track endpoint HTTP %d", resp.StatusCode)
	}

	var trk QobuzTrackItem
	if err := json.NewDecoder(resp.Body).Decode(&trk); err != nil {
		return nil, err
	}

	return &trk, nil
}

func fetchQobuzAlbumOfficial(client *http.Client, session *QobuzSession, albumID string) (string, string, []QobuzTrackItem, string, error) {
	endpoint := fmt.Sprintf("%s/album/get?album_id=%s&limit=500", qobuzAPIBaseURL, albumID)
	req, err := http.NewRequestWithContext(context.Background(), "GET", endpoint, nil)
	if err != nil {
		return "", "", nil, "", err
	}
	req.Header.Set("X-App-Id", session.AppID)
	if session.UserAuthToken != "" {
		req.Header.Set("X-User-Auth-Token", session.UserAuthToken)
	}

	resp, err := client.Do(req)
	if err != nil {
		return "", "", nil, "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", "", nil, "", fmt.Errorf("album endpoint HTTP %d", resp.StatusCode)
	}

	var alb QobuzAlbumResponse
	if err := json.NewDecoder(resp.Body).Decode(&alb); err != nil {
		return "", "", nil, "", err
	}

	coverURL := alb.Image.Large
	if coverURL == "" {
		coverURL = alb.Image.Small
	}
	artistName := alb.Artist.Name
	if artistName == "" {
		artistName = "Various Artists"
	}

	for i := range alb.Tracks.Items {
		if alb.Tracks.Items[i].Album.Title == "" {
			alb.Tracks.Items[i].Album.Title = alb.Title
		}
		if alb.Tracks.Items[i].Album.Image.Large == "" {
			alb.Tracks.Items[i].Album.Image.Large = coverURL
		}
		if alb.Tracks.Items[i].Performer.Name == "" && alb.Tracks.Items[i].Artist.Name == "" {
			alb.Tracks.Items[i].Performer.Name = artistName
		}
	}

	return alb.Title, artistName, alb.Tracks.Items, coverURL, nil
}

func fetchQobuzPlaylistOfficial(client *http.Client, session *QobuzSession, playlistID string) (string, string, []QobuzTrackItem, string, error) {
	endpoint := fmt.Sprintf("%s/playlist/get?playlist_id=%s&limit=500", qobuzAPIBaseURL, playlistID)
	req, err := http.NewRequestWithContext(context.Background(), "GET", endpoint, nil)
	if err != nil {
		return "", "", nil, "", err
	}
	req.Header.Set("X-App-Id", session.AppID)
	if session.UserAuthToken != "" {
		req.Header.Set("X-User-Auth-Token", session.UserAuthToken)
	}

	resp, err := client.Do(req)
	if err != nil {
		return "", "", nil, "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", "", nil, "", fmt.Errorf("playlist endpoint HTTP %d", resp.StatusCode)
	}

	var pl QobuzPlaylistResponse
	if err := json.NewDecoder(resp.Body).Decode(&pl); err != nil {
		return "", "", nil, "", err
	}

	owner := pl.Owner.Name
	if owner == "" {
		owner = "Qobuz"
	}

	coverURL := ""
	if len(pl.Tracks.Items) > 0 {
		coverURL = pl.Tracks.Items[0].Album.Image.Large
	}

	return pl.Title, owner, pl.Tracks.Items, coverURL, nil
}

func requestQobuzFileUrl(client *http.Client, session *QobuzSession, trackID int64, formatID string) (string, error) {
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	sig := computeQobuzRequestSig(formatID, trackID, ts, session.AppSecret)

	endpoint := fmt.Sprintf("%s/track/getFileUrl?format_id=%s&intent=stream&track_id=%d&request_ts=%s&request_sig=%s",
		qobuzAPIBaseURL, formatID, trackID, ts, sig)

	req, err := http.NewRequestWithContext(context.Background(), "GET", endpoint, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("X-App-Id", session.AppID)
	if session.UserAuthToken != "" {
		req.Header.Set("X-User-Auth-Token", session.UserAuthToken)
	}

	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var fileResp QobuzFileUrlResponse
	if err := json.NewDecoder(resp.Body).Decode(&fileResp); err != nil {
		return "", err
	}

	if fileResp.Error != "" {
		return "", errors.New(fileResp.Error)
	}
	if fileResp.URL == "" {
		return "", errors.New("empty stream URL returned")
	}

	return fileResp.URL, nil
}

func getQobuzTrackStreamingURL(client *http.Client, session *QobuzSession, trackID int64, quality, format string) (string, int, error) {
	var formatCandidates []string

	formatLower := strings.ToLower(format)
	qualityLower := strings.ToLower(quality)

	if formatLower == "mp3" {
		formatCandidates = []string{"5", "6", "7", "27"}
	} else {
		switch qualityLower {
		case "best", "hi-res", "24", "192", "96":
			formatCandidates = []string{"27", "7", "6", "5"}
		case "cd", "lossless", "16", "44":
			formatCandidates = []string{"6", "5"}
		default:
			formatCandidates = []string{"27", "7", "6", "5"}
		}
	}

	var lastErr error
	for _, fmtID := range formatCandidates {
		streamURL, err := requestQobuzFileUrl(client, session, trackID, fmtID)
		if err == nil && streamURL != "" {
			fID, _ := strconv.Atoi(fmtID)
			return streamURL, fID, nil
		}
		lastErr = err
	}

	return "", 0, fmt.Errorf("failed to obtain stream URL for track %d: %w", trackID, lastErr)
}

func streamOfficialQobuzAudio(client *http.Client, cfg *Config, streamURL, destPath string, spinnerIdx *int32) error {
	req, err := http.NewRequestWithContext(context.Background(), "GET", streamURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64)")

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
				drawProgressBar(int(idx), pct, "", "", "downloading audio stream…")
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

// runOfficialQobuzDownload handles Qobuz URL downloads via the official Qobuz API.
func runOfficialQobuzDownload(cfg *Config, binDir, rawURL string) ([]string, error) {
	parsed, err := parseQobuzURL(rawURL)
	if err != nil {
		return nil, err
	}

	session := getActiveQobuzSession(cfg)
	client := &http.Client{Timeout: 45 * time.Second}

	if err := ensureQobuzAuth(client, session); err != nil {
		return nil, err
	}

	var tracksToDownload []QobuzTrackItem
	var albumOrPlaylistTitle string
	var primaryArtist string
	var coverURL string

	switch parsed.Type {
	case "track":
		trk, err := fetchQobuzTrackOfficial(client, session, parsed.ID)
		if err != nil {
			return nil, fmt.Errorf("official Qobuz track fetch failed: %w", err)
		}
		tracksToDownload = []QobuzTrackItem{*trk}
		albumOrPlaylistTitle = trk.Album.Title
		primaryArtist = trk.Performer.Name
		if primaryArtist == "" {
			primaryArtist = trk.Artist.Name
		}
		coverURL = trk.Album.Image.Large
	case "album":
		title, artist, trks, cov, err := fetchQobuzAlbumOfficial(client, session, parsed.ID)
		if err != nil {
			return nil, fmt.Errorf("official Qobuz album fetch failed: %w", err)
		}
		albumOrPlaylistTitle = title
		primaryArtist = artist
		tracksToDownload = trks
		coverURL = cov
	case "playlist":
		title, owner, trks, cov, err := fetchQobuzPlaylistOfficial(client, session, parsed.ID)
		if err != nil {
			return nil, fmt.Errorf("official Qobuz playlist fetch failed: %w", err)
		}
		albumOrPlaylistTitle = title
		primaryArtist = owner
		tracksToDownload = trks
		coverURL = cov
	case "interpreter":
		return nil, fmt.Errorf("cannot download artist profile page: %s", parsed.ID)
	default:
		return nil, fmt.Errorf("unsupported Qobuz type: %s", parsed.Type)
	}

	if len(tracksToDownload) == 0 {
		return nil, errors.New("no tracks retrieved from official Qobuz API")
	}

	isMultiTrack := len(tracksToDownload) > 1
	if primaryArtist == "" {
		primaryArtist = "Unknown Artist"
	}

	logSubKV("album/playlist", truncateString(albumOrPlaylistTitle, 40))
	logSubKV("artist/curator", truncateString(primaryArtist, 40))
	logSubKV("tracks", fmt.Sprintf("%d", len(tracksToDownload)))
	logSubKV("backend", "Official Qobuz API v0.2")

	outDir := cfg.OutputDir
	if isMultiTrack && albumOrPlaylistTitle != "" {
		outDir = filepath.Join(outDir, sanitizeFilename(albumOrPlaylistTitle))
	}
	_ = os.MkdirAll(outDir, 0755)

	var savedFiles []string
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

		artistName := track.Performer.Name
		if artistName == "" {
			artistName = track.Artist.Name
		}
		if artistName == "" {
			artistName = primaryArtist
		}

		trackLabel := fmt.Sprintf("[%d/%d]", i+1, len(tracksToDownload))
		logPhase(fmt.Sprintf("%s %s – %s", trackLabel, artistName, track.Title))

		ext := ".flac"
		if strings.ToLower(cfg.Format) == "mp3" {
			ext = ".mp3"
		}

		meta := FileMeta{
			Title:       track.Title,
			Artist:      artistName,
			Album:       albumOrPlaylistTitle,
			TrackNumber: trackNum,
			Extension:   ext,
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

		if fi, err := os.Stat(destPath); err == nil && fi.Size() > 0 {
			logOK(fmt.Sprintf("already exists: %s", finalFilename))
			savedFiles = append(savedFiles, destPath)
			continue
		}

		streamURL, formatID, err := getQobuzTrackStreamingURL(client, session, track.ID, cfg.Quality, cfg.Format)
		if err != nil {
			if cfg.Verbose {
				logErr(fmt.Sprintf("official stream URL failed: %v", err))
			} else {
				logErr("failed to get track streaming URL — skipping")
			}
			failCount++
			continue
		}

		downloadPath := destPath
		if strings.ToLower(cfg.Format) == "mp3" && formatID != 5 {
			downloadPath = strings.TrimSuffix(destPath, ".mp3") + ".flac"
		}

		if err := streamOfficialQobuzAudio(client, cfg, streamURL, downloadPath, &spinnerIdx); err != nil {
			if cfg.Verbose {
				logErr(fmt.Sprintf("audio download failed: %v", err))
			} else {
				logErr("audio stream download failed — skipping")
			}
			failCount++
			continue
		}

		if strings.ToLower(cfg.Format) == "mp3" && formatID != 5 {
			if converted, tErr := transcodeAudio(binDir, downloadPath, "mp3"); tErr == nil {
				destPath = converted
			} else {
				destPath = downloadPath
			}
		}

		if _, err := os.Stat(coverFilePath); err == nil {
			_ = embedCoverArtFFmpeg(binDir, destPath, coverFilePath)
		}

		cleanupIntermediateFiles(destPath)
		logOK(fmt.Sprintf("saved: %s", finalFilename))
		savedFiles = append(savedFiles, destPath)
	}

	if failCount > 0 && len(savedFiles) == 0 {
		if cfg.Verbose {
			return savedFiles, fmt.Errorf("%d/%d tracks failed via official Qobuz API", failCount, len(tracksToDownload))
		}
		return savedFiles, fmt.Errorf("%d tracks could not be downloaded via official Qobuz API", failCount)
	}

	return savedFiles, nil
}

// ---------------------------------------------------------------------------
// Dual-Engine Orchestrator
// ---------------------------------------------------------------------------

// runQobuzDownload coordinates Qobuz downloads, trying the Lucida proxy first
// and falling back seamlessly to the official Qobuz API backend.
func runQobuzDownload(cfg *Config, binDir, rawURL string) ([]string, error) {
	lucidaClient := newLucidaHTTPClient(cfg)

	// Step 1: Attempt via Lucida proxy
	logPhase(fmt.Sprintf("Attempting Lucida engine for %s…", rawURL))
	saved, err := tryLucidaQobuzDownload(cfg, binDir, lucidaClient, rawURL)
	if err == nil && len(saved) > 0 {
		return saved, nil
	}

	// Step 2: Fallback to Official Qobuz API
	var cfErr lucidaCFError
	if errors.As(err, &cfErr) {
		logWarn("lucida.to is blocked by Cloudflare captcha")
	} else if err != nil {
		logWarn(fmt.Sprintf("Lucida unavailable (%v)", err))
	}
	logPhase("Falling back to official Qobuz API engine…")

	return runOfficialQobuzDownload(cfg, binDir, rawURL)
}

type QobuzURLInfo = qobuzURLInfo

var (
	IsQobuzURL             = isQobuzURL
	RunQobuzDownload       = runQobuzDownload
	ParseQobuzURL          = parseQobuzURL
	ComputeQobuzRequestSig = computeQobuzRequestSig
	GetActiveQobuzSession  = getActiveQobuzSession
)

