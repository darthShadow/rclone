package filter

import (
	"regexp"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTinySet(t *testing.T) {
	t.Run("empty set", func(t *testing.T) {
		ts := newTinySet(nil)

		assert.False(t, ts.contains("anything"))
		assert.Equal(t, 0, ts.count)
		assert.Nil(t, ts.m)
	})

	t.Run("single element", func(t *testing.T) {
		ts := newTinySet([]string{"alpha"})

		assert.True(t, ts.contains("alpha"))
		assert.False(t, ts.contains("beta"))
		assert.Equal(t, 1, ts.count)
		assert.Nil(t, ts.m)
	})

	t.Run("exactly eight elements uses inline storage", func(t *testing.T) {
		elements := []string{"a", "b", "c", "d", "e", "f", "g", "h"}
		ts := newTinySet(elements)

		require.Equal(t, tinySetThreshold, ts.count)
		assert.Nil(t, ts.m)
		for _, element := range elements {
			assert.True(t, ts.contains(element), "expected %q to be present", element)
		}
		assert.False(t, ts.contains("i"))
	})

	t.Run("nine elements uses map fallback", func(t *testing.T) {
		elements := []string{"a", "b", "c", "d", "e", "f", "g", "h", "i"}
		ts := newTinySet(elements)

		require.Equal(t, -1, ts.count)
		require.NotNil(t, ts.m)
		for _, element := range elements {
			assert.True(t, ts.contains(element), "expected %q to be present", element)
		}
		assert.False(t, ts.contains("j"))
	})

	t.Run("tinySetFromMap with small map uses inline storage", func(t *testing.T) {
		ts := tinySetFromMap(map[string]struct{}{
			".jpg": {},
			".png": {},
			".gif": {},
		})

		assert.Equal(t, 3, ts.count)
		assert.Nil(t, ts.m)
		assert.True(t, ts.contains(".jpg"))
		assert.True(t, ts.contains(".png"))
		assert.True(t, ts.contains(".gif"))
		assert.False(t, ts.contains(".txt"))
	})

	t.Run("tinySetFromMap with large map uses backing map", func(t *testing.T) {
		ts := tinySetFromMap(map[string]struct{}{
			"a": {},
			"b": {},
			"c": {},
			"d": {},
			"e": {},
			"f": {},
			"g": {},
			"h": {},
			"i": {},
			"j": {},
		})

		assert.Equal(t, -1, ts.count)
		require.NotNil(t, ts.m)
		assert.True(t, ts.contains("a"))
		assert.True(t, ts.contains("j"))
		assert.False(t, ts.contains("k"))
	})
}

func TestComputePathProps(t *testing.T) {
	testCases := []struct {
		name string
		path string
		want pathProps
	}{
		{
			name: "plain file",
			path: "file.txt",
			want: pathProps{
				firstSlash: -1,
				lastDot:    4,
				firstComp:  "file.txt",
				ext:        ".txt",
			},
		},
		{
			name: "nested file",
			path: "dir/sub/file.txt",
			want: pathProps{
				firstSlash: 3,
				lastDot:    12,
				firstComp:  "dir",
				ext:        ".txt",
			},
		},
		{
			name: "makefile has no extension",
			path: "Makefile",
			want: pathProps{
				firstSlash: -1,
				lastDot:    -1,
				firstComp:  "Makefile",
			},
		},
		{
			name: "dotfile at root",
			path: ".gitignore",
			want: pathProps{
				firstSlash:      -1,
				lastDot:         0,
				firstComp:       ".gitignore",
				ext:             ".gitignore",
				hasDotComponent: true,
			},
		},
		{
			name: "hidden directory component",
			path: "dir/.hidden/file.txt",
			want: pathProps{
				firstSlash:      3,
				lastDot:         16,
				firstComp:       "dir",
				ext:             ".txt",
				hasDotComponent: true,
			},
		},
		{
			name: "last dot wins",
			path: "archive.tar.gz",
			want: pathProps{
				firstSlash: -1,
				lastDot:    11,
				firstComp:  "archive.tar.gz",
				ext:        ".gz",
			},
		},
		{
			name: "empty path",
			path: "",
			want: pathProps{
				firstSlash: -1,
				lastDot:    -1,
			},
		},
		{
			name: "dot before slash is not extension",
			path: ".cache/file",
			want: pathProps{
				firstSlash:      6,
				lastDot:         0,
				firstComp:       ".cache",
				hasDotComponent: true,
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, computePathProps(tc.path))
		})
	}
}

func TestClassifyPattern(t *testing.T) {
	fallback := regexp.MustCompile("fallback")

	testCases := []struct {
		name           string
		glob           string
		ignoreCase     bool
		re             *regexp.Regexp
		wantKind       fastPathKind
		wantPrefix     string
		wantPrefixes   map[string]struct{}
		wantExtensions map[string]struct{}
		wantExtension  string
		wantFallback   *regexp.Regexp
	}{
		{
			name:         "match all",
			glob:         "**",
			wantKind:     fpMatchAll,
			wantFallback: nil,
		},
		{
			name:         "rooted match all",
			glob:         "/**",
			wantKind:     fpMatchAll,
			wantFallback: nil,
		},
		{
			name:         "dotfile all",
			glob:         ".**",
			wantKind:     fpDotfileAll,
			wantFallback: nil,
		},
		{
			name:         "rooted prefix",
			glob:         "/movies/**",
			wantKind:     fpRootedPrefix,
			wantPrefix:   "movies",
			wantFallback: nil,
		},
		{
			name:         "deep rooted prefix",
			glob:         "/learning/provider-a/**",
			wantKind:     fpRootedPrefix,
			wantPrefix:   "learning/provider-a",
			wantFallback: nil,
		},
		{
			name:     "rooted prefix set",
			glob:     "/{movies,sports}/**",
			wantKind: fpRootedPrefixSet,
			wantPrefixes: map[string]struct{}{
				"movies": {},
				"sports": {},
			},
			wantFallback: nil,
		},
		{
			name:     "single-element rooted prefix set",
			glob:     "/{media-a}/**",
			wantKind: fpRootedPrefixSet,
			wantPrefixes: map[string]struct{}{
				"media-a": {},
			},
			wantFallback: nil,
		},
		{
			name:     "single extension",
			glob:     "*.jpg",
			wantKind: fpExtensionSet,
			wantExtensions: map[string]struct{}{
				".jpg": {},
			},
			wantFallback: nil,
		},
		{
			name:     "extension set",
			glob:     "*.{jpg,png,gif}",
			wantKind: fpExtensionSet,
			wantExtensions: map[string]struct{}{
				".gif": {},
				".jpg": {},
				".png": {},
			},
			wantFallback: nil,
		},
		{
			name:          "rooted extension",
			glob:          "/*.rclonelink",
			wantKind:      fpRootedExtension,
			wantExtension: ".rclonelink",
			wantFallback:  nil,
		},
		{
			name:     "unrooted prefix set",
			glob:     "{.tmp,.cache}/**",
			wantKind: fpUnrootedPrefixSet,
			wantPrefixes: map[string]struct{}{
				".cache": {},
				".tmp":   {},
			},
			wantFallback: nil,
		},
		{
			name:         "unrooted prefix set with empty alternative",
			glob:         "{,.cache}/**",
			wantKind:     fpUnclassified,
			wantFallback: nil,
		},
		{
			name:         "unclassified when alternative contains slash",
			glob:         "{a/b,c}/**",
			wantKind:     fpUnclassified,
			wantFallback: nil,
		},
		{
			name:         "unclassified complex glob",
			glob:         "/dir/*/file.txt",
			wantKind:     fpUnclassified,
			wantFallback: nil,
		},
		{
			name:         "ignore case disables fast path",
			glob:         "**",
			ignoreCase:   true,
			wantKind:     fpUnclassified,
			wantFallback: nil,
		},
		{
			name:         "fallback preserved",
			glob:         "/dir/*/file.txt",
			re:           fallback,
			wantKind:     fpUnclassified,
			wantFallback: fallback,
		},
		// R1-fixed edge cases
		{
			name:         "rooted prefix with dot",
			glob:         "/a.b/**",
			wantKind:     fpRootedPrefix,
			wantPrefix:   "a.b",
			wantFallback: nil,
		},
		{
			name:     "rooted prefix set with dot",
			glob:     "/{a.b,c}/**",
			wantKind: fpRootedPrefixSet,
			wantPrefixes: map[string]struct{}{
				"a.b": {},
				"c":   {},
			},
			wantFallback: nil,
		},
		{
			name:         "rooted prefix set with slash in alternative",
			glob:         "/{a/b,c}/**",
			wantKind:     fpUnclassified,
			wantFallback: nil,
		},
		{
			name:         "multi-dot extension",
			glob:         "*.tar.gz",
			wantKind:     fpUnclassified,
			wantFallback: nil,
		},
		{
			name:         "multi-dot brace extension",
			glob:         "*.{tar.gz,png}",
			wantKind:     fpUnclassified,
			wantFallback: nil,
		},
		{
			name:         "multi-dot rooted extension",
			glob:         "/*.tar.gz",
			wantKind:     fpUnclassified,
			wantFallback: nil,
		},
		{
			name:     "backslash in rooted prefix",
			glob:     `/foo\dbar/**`,
			wantKind: fpUnclassified,
		},
		{
			name:     "backslash comma in rooted prefix set",
			glob:     `/{a\,b,c}/**`,
			wantKind: fpUnclassified,
		},
		{
			name:     "backslash in extension",
			glob:     `*.jp\d`,
			wantKind: fpUnclassified,
		},
		{
			name:     "backslash in rooted extension",
			glob:     `/*.t\est`,
			wantKind: fpUnclassified,
		},
		{
			name:     "backslash in unrooted prefix set",
			glob:     `{a\b,c}/**`,
			wantKind: fpUnclassified,
		},
		// R4-fixed edge cases
		{
			name:         "extension with slash",
			glob:         "*.log/old",
			ignoreCase:   false,
			re:           nil,
			wantKind:     fpUnclassified,
			wantFallback: nil,
		},
		{
			name:         "brace extension with slash",
			glob:         "*.{log/old,txt}",
			ignoreCase:   false,
			re:           nil,
			wantKind:     fpUnclassified,
			wantFallback: nil,
		},
		{
			name:         "double slash rooted prefix",
			glob:         "/a//**",
			ignoreCase:   false,
			re:           nil,
			wantKind:     fpUnclassified,
			wantFallback: nil,
		},
		{
			name:         "normal rooted prefix still works",
			glob:         "/learning/provider-a/**",
			ignoreCase:   false,
			re:           nil,
			wantKind:     fpRootedPrefix,
			wantPrefix:   "learning/provider-a",
			wantFallback: nil,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyPattern(tc.glob, tc.re, tc.ignoreCase)

			assert.Equal(t, tc.wantKind, got.Kind)
			assert.Equal(t, tc.wantPrefix, got.Prefix)
			assert.Equal(t, tc.wantPrefixes, got.Prefixes)
			assert.Equal(t, tc.wantExtensions, got.Extensions)
			assert.Equal(t, tc.wantExtension, got.Extension)
			if tc.wantFallback == nil {
				assert.Nil(t, got.Fallback)
			} else {
				assert.Same(t, tc.wantFallback, got.Fallback)
			}
		})
	}

	t.Run("compiled fallback pointer is preserved", func(t *testing.T) {
		re, err := GlobPathToRegexp("/dir/*/file.txt", false)
		require.NoError(t, err)

		got := classifyPattern("/dir/*/file.txt", re, false)
		assert.Same(t, re, got.Fallback)
	})
}
