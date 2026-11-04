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
