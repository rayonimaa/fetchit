package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestIsQobuzURL(t *testing.T) {
	tests := []struct {
		url      string
		expected bool
	}{
		{"https://play.qobuz.com/album/0060253765103", true},
		{"https://play.qobuz.com/track/12345678", true},
		{"https://open.qobuz.com/album/g3jksdl189", true},
		{"https://www.qobuz.com/us-en/album/nevermind/0072064244252", true},
		{"http://play.qobuz.com/playlist/9999", true},
		{"https://open.spotify.com/track/abc", false},
		{"https://www.youtube.com/watch?v=123", false},
		{"https://example.com/song", false},
	}

	for _, tt := range tests {
		got := isQobuzURL(tt.url)
		if got != tt.expected {
			t.Errorf("isQobuzURL(%q) = %v, want %v", tt.url, got, tt.expected)
		}
	}
}

func TestBuildYtdlpOutputTemplate(t *testing.T) {
	opts := RenameOptions{
		IncludeArtist:    true,
		IncludeAlbum:     false,
		IncludeTitle:     true,
		IncludeNumbering: false,
	}

	tmpl := buildYtdlpOutputTemplate(opts, false, "downloads")
	expected := filepath.Join("downloads", "%(uploader,channel)s%(uploader,channel& - |)s%(title)s.%(ext)s")
	if tmpl != expected {
		t.Errorf("buildYtdlpOutputTemplate() = %q, want %q", tmpl, expected)
	}

	// Playlist with numbering and album
	optsPL := RenameOptions{
		IncludeArtist:    true,
		IncludeAlbum:     true,
		IncludeTitle:     true,
		IncludeNumbering: true,
	}
	tmplPL := buildYtdlpOutputTemplate(optsPL, true, "downloads")
	expectedPL := filepath.Join("downloads", "%(playlist_title|Playlist)s", "%(playlist_index&%02d - |)s%(uploader,channel)s%(uploader,channel& - |)s%(album)s%(album& - |)s%(title)s.%(ext)s")
	if tmplPL != expectedPL {
		t.Errorf("buildYtdlpOutputTemplate(PL) = %q, want %q", tmplPL, expectedPL)
	}
}

func TestCleanupIntermediateFiles(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "fetchit-cleanup-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	finalFile := filepath.Join(tmpDir, "Artist - Song.mp3")
	interPart := filepath.Join(tmpDir, "Artist - Song.mp3.part")
	interWebm := filepath.Join(tmpDir, "Artist - Song.webm")
	interJpg := filepath.Join(tmpDir, "Artist - Song.jpg")
	otherSong := filepath.Join(tmpDir, "Another - Song.mp3")

	_ = os.WriteFile(finalFile, []byte("final audio"), 0644)
	_ = os.WriteFile(interPart, []byte("residual part"), 0644)
	_ = os.WriteFile(interWebm, []byte("residual webm"), 0644)
	_ = os.WriteFile(interJpg, []byte("residual cover"), 0644)
	_ = os.WriteFile(otherSong, []byte("unrelated audio"), 0644)

	cleanupIntermediateFiles(finalFile)

	if _, err := os.Stat(finalFile); os.IsNotExist(err) {
		t.Errorf("final file was deleted unexpectedly")
	}
	if _, err := os.Stat(otherSong); os.IsNotExist(err) {
		t.Errorf("unrelated song was deleted unexpectedly")
	}
	if _, err := os.Stat(interPart); !os.IsNotExist(err) {
		t.Errorf("intermediate .part file was NOT cleaned up")
	}
	if _, err := os.Stat(interWebm); !os.IsNotExist(err) {
		t.Errorf("intermediate .webm file was NOT cleaned up")
	}
	if _, err := os.Stat(interJpg); !os.IsNotExist(err) {
		t.Errorf("intermediate .jpg file was NOT cleaned up")
	}
}

func TestParseQobuzURL(t *testing.T) {
	tests := []struct {
		url          string
		expectedType string
		expectedID   string
		expectErr    bool
	}{
		{
			url:          "https://play.qobuz.com/album/0060253765103",
			expectedType: "album",
			expectedID:   "0060253765103",
		},
		{
			url:          "https://play.qobuz.com/track/12345678",
			expectedType: "track",
			expectedID:   "12345678",
		},
		{
			url:          "https://open.qobuz.com/album/g3jksdl189",
			expectedType: "album",
			expectedID:   "g3jksdl189",
		},
		{
			url:          "https://www.qobuz.com/us-en/album/nevermind/0072064244252",
			expectedType: "album",
			expectedID:   "0072064244252",
		},
		{
			url:          "https://www.qobuz.com/fr-fr/track/some-slug/998877",
			expectedType: "track",
			expectedID:   "998877",
		},
		{
			url:          "http://play.qobuz.com/playlist/9999",
			expectedType: "playlist",
			expectedID:   "9999",
		},
		{
			url:          "https://play.qobuz.com/playlist/9999?source=web&tab=tracks",
			expectedType: "playlist",
			expectedID:   "9999",
		},
		{
			url:          "https://play.qobuz.com/interpreter/554433",
			expectedType: "interpreter",
			expectedID:   "554433",
		},
		{
			url:       "https://open.spotify.com/track/abc",
			expectErr: true,
		},
		{
			url:       "https://youtube.com/watch?v=123",
			expectErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.url, func(t *testing.T) {
			got, err := parseQobuzURL(tt.url)
			if tt.expectErr {
				if err == nil {
					t.Fatalf("expected error for URL %q, got nil", tt.url)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.Type != tt.expectedType {
				t.Errorf("Type = %q, want %q", got.Type, tt.expectedType)
			}
			if got.ID != tt.expectedID {
				t.Errorf("ID = %q, want %q", got.ID, tt.expectedID)
			}
		})
	}
}

func TestComputeQobuzRequestSig(t *testing.T) {
	formatID := "27"
	trackID := int64(123456)
	ts := "1700000000"
	secret := "b5a96f62b0821c81ef841cf43c683b58"

	sig := computeQobuzRequestSig(formatID, trackID, ts, secret)
	if len(sig) != 32 {
		t.Errorf("expected 32-character hex MD5 signature, got %d chars: %s", len(sig), sig)
	}

	// Verify deterministic calculation
	expectedSig := computeQobuzRequestSig(formatID, trackID, ts, secret)
	if sig != expectedSig {
		t.Errorf("expected signature %s, got %s", expectedSig, sig)
	}
}

func TestGetActiveQobuzSession(t *testing.T) {
	cfg := &Config{}
	sess := getActiveQobuzSession(cfg)
	if sess.AppID == "" {
		t.Errorf("expected default AppID, got empty")
	}
	if sess.AppSecret == "" {
		t.Errorf("expected default AppSecret, got empty")
	}

	// Environment variable override
	t.Setenv("QOBUZ_APP_ID", "custom_id_123")
	t.Setenv("QOBUZ_APP_SECRET", "custom_secret_456")
	t.Setenv("QOBUZ_USER_AUTH_TOKEN", "custom_token_789")

	overridden := getActiveQobuzSession(cfg)
	if overridden.AppID != "custom_id_123" {
		t.Errorf("AppID = %q, want custom_id_123", overridden.AppID)
	}
	if overridden.AppSecret != "custom_secret_456" {
		t.Errorf("AppSecret = %q, want custom_secret_456", overridden.AppSecret)
	}
	if overridden.UserAuthToken != "custom_token_789" {
		t.Errorf("UserAuthToken = %q, want custom_token_789", overridden.UserAuthToken)
	}
}
