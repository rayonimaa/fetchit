package main

import (
	"reflect"
	"testing"
)

func TestIsTidalURL(t *testing.T) {
	validURLs := []string{
		"https://tidal.com/browse/track/12345678",
		"https://listen.tidal.com/track/12345678",
		"https://tidal.com/track/12345678",
		"http://tidal.com/track/12345678",
		"https://tidal.com/browse/album/87654321",
		"https://listen.tidal.com/album/87654321",
		"https://tidal.com/album/87654321",
		"https://tidal.com/browse/playlist/12345678-1234-1234-1234-123456789abc",
		"https://listen.tidal.com/playlist/12345678-1234-1234-1234-123456789abc",
		"https://tidal.com/playlist/12345678-1234-1234-1234-123456789abc",
		"https://tidal.com/us/track/12345678",
	}

	for _, u := range validURLs {
		if !isTidalURL(u) {
			t.Errorf("expected isTidalURL(%q) == true, got false", u)
		}
	}

	invalidURLs := []string{
		"https://open.spotify.com/track/4cOdK2wGLETKBW3PvgPWqT",
		"https://www.youtube.com/watch?v=dQw4w9WgXcQ",
		"https://google.com",
		"not-a-url",
	}

	for _, u := range invalidURLs {
		if isTidalURL(u) {
			t.Errorf("expected isTidalURL(%q) == false, got true", u)
		}
	}
}

func TestParseTidalURL(t *testing.T) {
	tests := []struct {
		url      string
		wantType string
		wantID   string
	}{
		{
			url:      "https://tidal.com/browse/track/12345678",
			wantType: "track",
			wantID:   "12345678",
		},
		{
			url:      "https://listen.tidal.com/album/98765432",
			wantType: "album",
			wantID:   "98765432",
		},
		{
			url:      "https://tidal.com/playlist/c03dc829-9b55-46f9-bb35-513143c7b396",
			wantType: "playlist",
			wantID:   "c03dc829-9b55-46f9-bb35-513143c7b396",
		},
	}

	for _, tt := range tests {
		info, err := parseTidalURL(tt.url)
		if err != nil {
			t.Fatalf("unexpected error parsing %q: %v", tt.url, err)
		}
		if info.Type != tt.wantType {
			t.Errorf("url %q: got type %q, want %q", tt.url, info.Type, tt.wantType)
		}
		if info.ID != tt.wantID {
			t.Errorf("url %q: got id %q, want %q", tt.url, info.ID, tt.wantID)
		}
	}
}

func TestParseDashManifest(t *testing.T) {
	manifestXML := `<?xml version="1.0" encoding="utf-8"?>
<MPD xmlns="urn:mpeg:dash:schema:mpd:2011" minBufferTime="PT1.5S" type="static">
  <Period duration="PT3M33S">
    <AdaptationSet mimeType="audio/mp4" codecs="flac">
      <Representation id="1" bandwidth="1000000">
        <SegmentTemplate initialization="https://audio.tidal.com/init.mp4" media="https://audio.tidal.com/chunk_$Number$.mp4" startNumber="1">
          <SegmentTimeline>
            <S d="44100" r="2" />
            <S d="22050" r="0" />
          </SegmentTimeline>
        </SegmentTemplate>
      </Representation>
    </AdaptationSet>
  </Period>
</MPD>`

	urls, err := parseDashManifest(manifestXML)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	expected := []string{
		"https://audio.tidal.com/init.mp4",
		"https://audio.tidal.com/chunk_1.mp4",
		"https://audio.tidal.com/chunk_2.mp4",
		"https://audio.tidal.com/chunk_3.mp4",
		"https://audio.tidal.com/chunk_4.mp4",
	}

	if !reflect.DeepEqual(urls, expected) {
		t.Errorf("got %v, want %v", urls, expected)
	}
}

func TestMapTidalQuality(t *testing.T) {
	tests := []struct {
		quality string
		format  string
		want    string
	}{
		{"best", "flac", "LOSSLESS"},
		{"hi-res", "flac", "HI_RES_LOSSLESS"},
		{"320", "flac", "HIGH"},
		{"128", "flac", "LOW"},
		{"best", "mp3", "LOW"},
	}

	for _, tt := range tests {
		got := mapTidalQuality(tt.quality, tt.format)
		if got != tt.want {
			t.Errorf("quality=%q, format=%q: got %q, want %q", tt.quality, tt.format, got, tt.want)
		}
	}
}

func TestGetTidalCoverURL(t *testing.T) {
	uuid := "c03dc829-9b55-46f9-bb35-513143c7b396"
	expected := "https://resources.tidal.com/images/c03dc829/9b55/46f9/bb35/513143c7b396/1280x1280.jpg"
	got := getTidalCoverURL(uuid)
	if got != expected {
		t.Errorf("got %q, want %q", got, expected)
	}
}
