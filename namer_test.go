package main

import (
	"strings"
	"testing"
)

func TestGenerateFilename(t *testing.T) {
	tests := []struct {
		name     string
		meta     FileMeta
		opts     RenameOptions
		expected string
	}{
		{
			name: "Standard single track: Artist and Title",
			meta: FileMeta{
				Title:       "Never Gonna Give You Up",
				Artist:      "Rick Astley",
				Album:       "Whenever You Need Somebody",
				TrackNumber: 1,
				Extension:   ".mp3",
			},
			opts: RenameOptions{
				IncludeArtist:    true,
				IncludeAlbum:     false,
				IncludeTitle:     true,
				IsPlaylist:       false,
				IncludeNumbering: false,
			},
			expected: "Rick Astley - Never Gonna Give You Up.mp3",
		},
		{
			name: "Playlist with numbering: Artist - Title",
			meta: FileMeta{
				Title:       "Never Gonna Give You Up",
				Artist:      "Rick Astley",
				Album:       "Whenever You Need Somebody",
				TrackNumber: 5,
				Extension:   "mp3", // Test without leading dot
			},
			opts: RenameOptions{
				IncludeArtist:    true,
				IncludeAlbum:     false,
				IncludeTitle:     true,
				IsPlaylist:       true,
				IncludeNumbering: true,
			},
			expected: "05 - Rick Astley - Never Gonna Give You Up.mp3",
		},
		{
			name: "With Album included: Artist - Album - Title",
			meta: FileMeta{
				Title:       "Stay",
				Artist:      "The Kid LAROI",
				Album:       "F*CK LOVE 3",
				TrackNumber: 1,
				Extension:   ".flac",
			},
			opts: RenameOptions{
				IncludeArtist:    true,
				IncludeAlbum:     true,
				IncludeTitle:     true,
				IsPlaylist:       false,
				IncludeNumbering: false,
			},
			expected: "The Kid LAROI - F_CK LOVE 3 - Stay.flac",
		},
		{
			name: "Sanitizes OS-forbidden characters: < > : \" / \\ | ? *",
			meta: FileMeta{
				Title:     "Song: What / Why? *Special* <Edition> \"Remix\" | Live",
				Artist:    "AC/DC",
				Extension: ".mp3",
			},
			opts: RenameOptions{
				IncludeArtist: true,
				IncludeTitle:  true,
			},
			expected: "AC_DC - Song_ What _ Why_ _Special_ _Edition_ _Remix_ _ Live.mp3",
		},
		{
			name: "Windows reserved filename (NUL)",
			meta: FileMeta{
				Title:     "NUL",
				Extension: ".mp3",
			},
			opts: RenameOptions{
				IncludeTitle: true,
			},
			expected: "_NUL.mp3",
		},
		{
			name: "Fallback when no checkboxes are checked",
			meta: FileMeta{
				Title:     "Only Title",
				Artist:    "Ignored Artist",
				Extension: ".wav",
			},
			opts: RenameOptions{
				IncludeArtist: false,
				IncludeAlbum:  false,
				IncludeTitle:  false, // should fallback to Title = true
			},
			expected: "Only Title.wav",
		},
		{
			name: "yt-dlp template extension",
			meta: FileMeta{
				Title:       "Blinding Lights",
				Artist:      "The Weeknd",
				TrackNumber: 2,
				Extension:   ".%(ext)s",
			},
			opts: RenameOptions{
				IncludeArtist:    true,
				IncludeTitle:     true,
				IsPlaylist:       true,
				IncludeNumbering: true,
			},
			expected: "02 - The Weeknd - Blinding Lights.%(ext)s",
		},
		{
			name: "Trims trailing dots and spaces before extension",
			meta: FileMeta{
				Title:     "Song with trailing dots...   ",
				Extension: ".mp3",
			},
			opts: RenameOptions{
				IncludeTitle: true,
			},
			expected: "Song with trailing dots.mp3",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := generateFilename(tt.meta, tt.opts)
			if got != tt.expected {
				t.Errorf("generateFilename() = %q, want %q", got, tt.expected)
			}
		})
	}
}

func TestSanitizeBaseNameLength(t *testing.T) {
	longName := strings.Repeat("A", 300)
	got := sanitizeBaseName(longName)
	if len(got) > 240 {
		t.Errorf("sanitizeBaseName() produced %d bytes, want <= 240", len(got))
	}
}
