package test

import (
	"reflect"
	"testing"
)

func TestParseLinkInput(t *testing.T) {
	tests := []struct {
		name     string
		inputs   []string
		expected []string
	}{
		{
			name:     "Single URL",
			inputs:   []string{"https://www.youtube.com/watch?v=dQw4w9WgXcQ"},
			expected: []string{"https://www.youtube.com/watch?v=dQw4w9WgXcQ"},
		},
		{
			name:     "Comma divided with space",
			inputs:   []string{"https://youtu.be/aaa, https://youtu.be/bbb, https://youtu.be/ccc"},
			expected: []string{"https://youtu.be/aaa", "https://youtu.be/bbb", "https://youtu.be/ccc"},
		},
		{
			name:     "Comma divided without space",
			inputs:   []string{"https://youtu.be/aaa,https://youtu.be/bbb,https://youtu.be/ccc"},
			expected: []string{"https://youtu.be/aaa", "https://youtu.be/bbb", "https://youtu.be/ccc"},
		},
		{
			name:     "Space divided (spaces and tabs)",
			inputs:   []string{"https://youtu.be/aaa   https://youtu.be/bbb\thttps://youtu.be/ccc"},
			expected: []string{"https://youtu.be/aaa", "https://youtu.be/bbb", "https://youtu.be/ccc"},
		},
		{
			name:     "Enter divided (Unix newlines \\n)",
			inputs:   []string{"https://youtu.be/aaa\nhttps://youtu.be/bbb\nhttps://youtu.be/ccc"},
			expected: []string{"https://youtu.be/aaa", "https://youtu.be/bbb", "https://youtu.be/ccc"},
		},
		{
			name:     "Enter divided (Windows newlines \\r\\n)",
			inputs:   []string{"https://youtu.be/aaa\r\nhttps://youtu.be/bbb\r\nhttps://youtu.be/ccc\r\n"},
			expected: []string{"https://youtu.be/aaa", "https://youtu.be/bbb", "https://youtu.be/ccc"},
		},
		{
			name:     "Mixed delimiters (comma, space, and enter)",
			inputs:   []string{"https://youtu.be/aaa, https://youtu.be/bbb\r\nhttps://youtu.be/ccc https://youtu.be/ddd,https://youtu.be/eee"},
			expected: []string{"https://youtu.be/aaa", "https://youtu.be/bbb", "https://youtu.be/ccc", "https://youtu.be/ddd", "https://youtu.be/eee"},
		},
		{
			name:     "Quoted links (single, double, and backticks)",
			inputs:   []string{`"https://youtu.be/aaa", 'https://youtu.be/bbb', ` + "`https://youtu.be/ccc`"},
			expected: []string{"https://youtu.be/aaa", "https://youtu.be/bbb", "https://youtu.be/ccc"},
		},
		{
			name:     "Bracketed and markdown links",
			inputs:   []string{"<https://youtu.be/aaa> (<https://youtu.be/bbb>) (https://youtu.be/ccc)"},
			expected: []string{"https://youtu.be/aaa", "https://youtu.be/bbb", "https://youtu.be/ccc"},
		},
		{
			name:     "Numbered and bulleted list",
			inputs:   []string{"1. https://youtu.be/aaa\n2) https://youtu.be/bbb\n- https://youtu.be/ccc\n* https://youtu.be/ddd\n• https://youtu.be/eee"},
			expected: []string{"https://youtu.be/aaa", "https://youtu.be/bbb", "https://youtu.be/ccc", "https://youtu.be/ddd", "https://youtu.be/eee"},
		},
		{
			name:     "Deduplication preserving order",
			inputs:   []string{"https://youtu.be/aaa, https://youtu.be/bbb, https://youtu.be/aaa, https://youtu.be/ccc, https://youtu.be/bbb"},
			expected: []string{"https://youtu.be/aaa", "https://youtu.be/bbb", "https://youtu.be/ccc"},
		},
		{
			name:     "Multiple inputs slice",
			inputs:   []string{"https://youtu.be/aaa, https://youtu.be/bbb", "https://youtu.be/ccc\nhttps://youtu.be/ddd"},
			expected: []string{"https://youtu.be/aaa", "https://youtu.be/bbb", "https://youtu.be/ccc", "https://youtu.be/ddd"},
		},
		{
			name:     "Empty and whitespace only",
			inputs:   []string{"", "   ", "\n\r\t", ",,, , "},
			expected: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ParseLinkInput(tt.inputs...)
			if !reflect.DeepEqual(got, tt.expected) && !(len(got) == 0 && len(tt.expected) == 0) {
				t.Errorf("ParseLinkInput(%v) = %v, expected %v", tt.inputs, got, tt.expected)
			}
		})
	}
}
