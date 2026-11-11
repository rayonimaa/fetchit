package test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGenerateFilename(t *testing.T) {
	tests := []struct {
		name        string
		meta        FileMeta
		opts        RenameOptions
		expected    string
		expectedErr error
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
			expected:    "Rick Astley - Never Gonna Give You Up.mp3",
			expectedErr: nil,
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
			expected:    "05 - Rick Astley - Never Gonna Give You Up.mp3",
			expectedErr: nil,
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
			expected:    "The Kid LAROI - F_CK LOVE 3 - Stay.flac",
			expectedErr: nil,
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
			expected:    "AC_DC - Song_ What _ Why_ _Special_ _Edition_ _Remix_ _ Live.mp3",
			expectedErr: nil,
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
			expected:    "_NUL.mp3",
			expectedErr: nil,
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
			expected:    "Only Title.wav",
			expectedErr: ErrNoNamingFields,
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
			expected:    "02 - The Weeknd - Blinding Lights.%(ext)s",
			expectedErr: nil,
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
			expected:    "Song with trailing dots.mp3",
			expectedErr: nil,
		},
		{
			name: "Failsafe error when custom namer receives no metadata input",
			meta: FileMeta{
				Extension: ".mp3",
			},
			opts: RenameOptions{
				IncludeArtist: true,
				IncludeTitle:  true,
			},
			expected:    "Track.mp3",
			expectedErr: ErrNoNamerInput,
		},
		{
			name: "Failsafe error when custom namer receives only track number",
			meta: FileMeta{
				TrackNumber: 7,
				Extension:   ".flac",
			},
			opts: RenameOptions{
				IncludeArtist: true,
				IncludeTitle:  true,
			},
			expected:    "Track 7.flac",
			expectedErr: ErrNoNamerInput,
		},
		{
			name: "Failsafe error when metadata strings are all whitespace",
			meta: FileMeta{
				Title:     "   ",
				Artist:    "\t",
				Album:     " \n",
				Extension: ".mp3",
			},
			opts: RenameOptions{
				IncludeArtist: true,
				IncludeTitle:  true,
			},
			expected:    "Track.mp3",
			expectedErr: ErrNoNamerInput,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := generateFilename(tt.meta, tt.opts)
			if got != tt.expected {
				t.Errorf("generateFilename() = %q, want %q", got, tt.expected)
			}
			if tt.expectedErr != nil {
				if !errors.Is(err, tt.expectedErr) {
					t.Errorf("generateFilename() error = %v, want %v", err, tt.expectedErr)
				}
			} else if err != nil {
				t.Errorf("generateFilename() unexpected error = %v", err)
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

func TestResolvePath(t *testing.T) {
	// Strips surrounding quotes
	quoted := `"C:\test\music"`
	resolved := resolvePath(quoted)
	if strings.Contains(resolved, `"`) {
		t.Errorf("resolvePath(%q) should strip double quotes, got %q", quoted, resolved)
	}

	singleQuoted := `'C:\test\music'`
	resolvedSingle := resolvePath(singleQuoted)
	if strings.Contains(resolvedSingle, `'`) {
		t.Errorf("resolvePath(%q) should strip single quotes, got %q", singleQuoted, resolvedSingle)
	}

	// Empty path
	if resolvePath("") != "" {
		t.Errorf("resolvePath(\"\") should return empty string")
	}

	// Tilde expansion
	home, err := os.UserHomeDir()
	if err == nil && home != "" {
		tildePath := "~/Music"
		resolvedTilde := resolvePath(tildePath)
		expectedHomeSub := filepath.Join(home, "Music")
		if resolvedTilde != expectedHomeSub {
			t.Errorf("resolvePath(%q) = %q, want %q", tildePath, resolvedTilde, expectedHomeSub)
		}
	}
}

func TestMoveFile(t *testing.T) {
	tempDir := t.TempDir()
	src := filepath.Join(tempDir, "source.txt")
	dest := filepath.Join(tempDir, "dest.txt")

	content := []byte("testing moveFile function")
	if err := os.WriteFile(src, content, 0644); err != nil {
		t.Fatalf("failed to create test file: %v", err)
	}

	if err := moveFile(src, dest); err != nil {
		t.Fatalf("moveFile failed: %v", err)
	}

	// Source must be gone
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Errorf("source file should have been removed after move")
	}

	// Dest must exist with matching content
	destContent, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("failed to read dest file: %v", err)
	}
	if string(destContent) != string(content) {
		t.Errorf("dest content = %q, want %q", string(destContent), string(content))
	}
}
