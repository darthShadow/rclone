package filter

import (
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const standardFilterRules = `- {.downloads,.inbound,.recyclebin}/**
- {tmp,temp,trash,inbound}/**
+ /*.rclonelink
+ /mounted.bin/**
+ /learning/{provider-a,provider-b,provider-c,provider-d}/**
+ /sports/stats-db/**
- .**
- *.{bin,bif,jpg,jpeg,png,part,partial,tmp}
- *.{txt,pdf,nfo,nfo-orig,json,dat}
- *.{test_ext_a,test_ext_b}
+ /{learning,sports}/**
+ /{media-a,media-a-int,media-a-dubbed}/**
+ /{media-b,media-b-int,media-b-dubbed}/**
- **`

const standardFilterRulesV2 = `- {.downloads,.inbound,.recyclebin}/**
- {tmp,temp,trash,inbound}/**
+ /*.rclonelink
+ /mounted.bin/**
+ /learning/provider-a/**
+ /learning/provider-b/**
+ /learning/provider-c/**
+ /learning/provider-d/**
+ /sports/stats-db/**
- .**
- *.{bin,bif,jpg,jpeg,png,part,partial,tmp}
- *.{txt,pdf,nfo,nfo-orig,json,dat}
- *.{test_ext_a,test_ext_b}
+ /{learning,sports}/**
+ /{media-a,media-a-int,media-a-dubbed}/**
+ /{media-b,media-b-int,media-b-dubbed}/**
- **`

const stressFilterRules = `- {.downloads,.inbound,.recyclebin}/**
+ /data/deep/nested/**
+ /data/{a,b}/nested/**
+ /*.rclonelink
- *.[Ll]og
- temp?/**
- *.{log,bak,swp}
- .**
+ /{keep-a,keep-b}/**
- **`

func makeTestRules(t *testing.T, specs []struct {
	glob    string
	include bool
}) []classifiedRule {
	t.Helper()

	rules := make([]classifiedRule, len(specs))
	for i, spec := range specs {
		re, err := GlobPathToRegexp(spec.glob, false)
		require.NoError(t, err)

		rule := classifyPattern(spec.glob, re, false)
		rule.Include = spec.include
		rules[i] = rule
	}

	return rules
}

func linearScan(rules []classifiedRule, remote string) bool {
	for _, rule := range rules {
		if rule.Fallback != nil && rule.Fallback.MatchString(remote) {
			return rule.Include
		}
	}
	return true
}

func parseFilterRulesString(t *testing.T, text string) []struct {
	glob    string
	include bool
} {
	t.Helper()

	var specs []struct {
		glob    string
		include bool
	}
	for lineNum, rawLine := range strings.Split(text, "\n") {
		line := strings.TrimSpace(rawLine)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}

		switch {
		case strings.HasPrefix(line, "- "):
			specs = append(specs, struct {
				glob    string
				include bool
			}{glob: strings.TrimPrefix(line, "- "), include: false})
		case strings.HasPrefix(line, "+ "):
			specs = append(specs, struct {
				glob    string
				include bool
			}{glob: strings.TrimPrefix(line, "+ "), include: true})
		default:
			t.Fatalf("line %d: unexpected filter line %q", lineNum+1, rawLine)
		}
	}

	return specs
}

func generateGoldenCorpus(specs []struct {
	glob    string
	include bool
}) []string {
	roots := map[string]struct{}{
		"alpha":    {},
		"beta":     {},
		"learning": {},
		"media":    {},
		"misc":     {},
		"media-a":  {},
		"random":   {},
		"sports":   {},
		"media-b":  {},
		"tmp":      {},
		"trash":    {},
	}
	components := map[string]struct{}{
		".cache":      {},
		".downloads":  {},
		".hidden":     {},
		".inbound":    {},
		".recyclebin": {},
		"Season01":    {},
		"extras":      {},
		"inbound":     {},
		"nested":      {},
		"provider-a":  {},
		"provider-b":  {},
		"provider-c":  {},
		"provider-d":  {},
		"stats-db":    {},
	}
	extensions := map[string]struct{}{
		"":            {},
		".bin":        {},
		".gif":        {},
		".jpg":        {},
		".json":       {},
		".mkv":        {},
		".mp4":        {},
		".nfo":        {},
		".pdf":        {},
		".png":        {},
		".rclonelink": {},
		".tmp":        {},
		".txt":        {},
	}

	for _, spec := range specs {
		classified := classifyPattern(spec.glob, nil, false)
		switch classified.Kind {
		case fpRootedPrefix:
			parts := strings.Split(classified.Prefix, "/")
			roots[parts[0]] = struct{}{}
			for _, part := range parts[1:] {
				components[part] = struct{}{}
			}
		case fpRootedPrefixSet, fpUnrootedPrefixSet:
			for prefix := range classified.Prefixes {
				if strings.Contains(prefix, "/") {
					parts := strings.Split(prefix, "/")
					roots[parts[0]] = struct{}{}
					for _, part := range parts[1:] {
						components[part] = struct{}{}
					}
					continue
				}
				roots[prefix] = struct{}{}
				components[prefix] = struct{}{}
			}
		case fpExtensionSet:
			for ext := range classified.Extensions {
				extensions[ext] = struct{}{}
			}
		case fpRootedExtension:
			extensions[classified.Extension] = struct{}{}
		}

		for _, token := range strings.FieldsFunc(spec.glob, func(r rune) bool {
			switch {
			case r >= 'a' && r <= 'z':
				return false
			case r >= 'A' && r <= 'Z':
				return false
			case r >= '0' && r <= '9':
				return false
			case r == '.' || r == '_' || r == '-':
				return false
			default:
				return true
			}
		}) {
			if token == "" {
				continue
			}
			if strings.HasPrefix(token, ".") {
				components[token] = struct{}{}
				continue
			}
			roots[token] = struct{}{}
			components[token] = struct{}{}
		}
	}

	rootList := sortedKeys(roots)
	componentList := sortedKeys(components)
	extensionList := sortedKeys(extensions)
	fileBases := []string{"cover", "episode", "feature", "guide", "index", "lesson", "movie", "notes", "poster", "sample"}

	seen := make(map[string]struct{}, 2048)
	corpus := make([]string, 0, 2048)
	add := func(path string) {
		if _, ok := seen[path]; ok {
			return
		}
		seen[path] = struct{}{}
		corpus = append(corpus, path)
	}

	staticPaths := []string{
		"",
		".gitignore",
		"app.log",
		".cache/file",
		"test.rclonelink",
		"data/a/nested/report.csv",
		"data/b/nested/report.csv",
		"temp1/work/file.txt",
		"media-a/feature.mkv",
		"media-a/poster.jpg",
		"sports/stats-db/game.mkv",
		"learning/provider-a/lesson.mp4",
		"learning/provider-b/readme.txt",
		"learning/provider-c/.hidden/file.txt",
		"media-b/show/episode.mkv",
		"random/.hidden/file.bin",
		"tmp/work/file.tmp",
		"trash/bin/file.txt",
		"inbound/drop/file.json",
		"mounted.bin/file.txt",
		"dir/.hidden/file.txt",
		"a/.cache/poster.png",
		"alpha/beta/movie.bin",
		"media/.downloads/file.rclonelink",
	}
	for _, path := range staticPaths {
		add(path)
	}

	for _, base := range fileBases {
		for _, ext := range extensionList {
			add(base + ext)
		}
	}

	for _, root := range rootList {
		for _, component := range componentList {
			for _, base := range fileBases[:6] {
				for _, ext := range extensionList[:8] {
					add(root + "/" + component + "/" + base + ext)
					add(root + "/" + component + "/.hidden/" + base + ext)
				}
			}
		}
	}

	for _, root := range rootList {
		for _, component := range componentList[:8] {
			for _, ext := range extensionList {
				add(root + "/" + component + "/nested/" + component + ext)
			}
		}
	}

	return corpus
}

func sortedKeys(set map[string]struct{}) []string {
	keys := make([]string, 0, len(set))
	for key := range set {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func TestClassifyFusedBlock(t *testing.T) {
	testCases := []struct {
		name  string
		rules []classifiedRule
		want  fusedBlockKind
	}{
		{
			name: "single rule",
			rules: []classifiedRule{
				{Kind: fpRootedPrefix},
			},
			want: fusedBlockSingle,
		},
		{
			name: "two rooted prefix sets",
			rules: []classifiedRule{
				{Kind: fpRootedPrefixSet},
				{Kind: fpRootedPrefixSet},
			},
			want: fusedBlockRootedPrefixSets,
		},
		{
			name: "two unrooted prefix sets",
			rules: []classifiedRule{
				{Kind: fpUnrootedPrefixSet},
				{Kind: fpUnrootedPrefixSet},
			},
			want: fusedBlockUnrootedPrefixSets,
		},
		{
			name: "mixed rooted",
			rules: []classifiedRule{
				{Kind: fpRootedPrefix},
				{Kind: fpRootedExtension},
			},
			want: fusedBlockMixedRooted,
		},
		{
			name: "extension dotfile",
			rules: []classifiedRule{
				{Kind: fpExtensionSet},
				{Kind: fpDotfileAll},
			},
			want: fusedBlockExtensionDotfile,
		},
		{
			name: "extensions only",
			rules: []classifiedRule{
				{Kind: fpExtensionSet},
				{Kind: fpExtensionSet},
			},
			want: fusedBlockExtensionDotfile,
		},
		{
			name: "all unclassified",
			rules: []classifiedRule{
				{Kind: fpUnclassified},
				{Kind: fpUnclassified},
			},
			want: fusedBlockMixed,
		},
		{
			name: "mixed catch all",
			rules: []classifiedRule{
				{Kind: fpRootedPrefix},
				{Kind: fpExtensionSet},
			},
			want: fusedBlockMixed,
		},
		{
			name:  "empty slice",
			rules: nil,
			want:  fusedBlockSingle,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, classifyFusedBlock(tc.rules))
		})
	}
}

func TestFusedEvaluateEmpty(t *testing.T) {
	frs := buildFusedRuleSet(nil)
	require.NotNil(t, frs)

	assert.True(t, frs.evaluate(""))
	assert.True(t, frs.evaluate("anything/goes/here.txt"))
}

func TestFusedEvaluateSingleRule(t *testing.T) {
	testCases := []struct {
		name  string
		specs []struct {
			glob    string
			include bool
		}
		matchPath string
		wantMatch bool
		missPath  string
		wantMiss  bool
	}{
		{
			name: "match all exclude",
			specs: []struct {
				glob    string
				include bool
			}{
				{glob: "**", include: false},
			},
			matchPath: "anything/here.txt",
			wantMatch: false,
		},
		{
			name: "rooted prefix include",
			specs: []struct {
				glob    string
				include bool
			}{
				{glob: "/keep/**", include: true},
			},
			matchPath: "keep/file.txt",
			wantMatch: true,
			missPath:  "other/file.txt",
			wantMiss:  true,
		},
		{
			name: "rooted prefix include exact prefix without trailing content",
			specs: []struct {
				glob    string
				include bool
			}{
				{glob: "/keep/**", include: true},
			},
			matchPath: "keep/file.txt",
			wantMatch: true,
			missPath:  "keep",
			wantMiss:  true,
		},
		{
			name: "rooted prefix include first component substring miss",
			specs: []struct {
				glob    string
				include bool
			}{
				{glob: "/keep/**", include: true},
			},
			matchPath: "keep/file.txt",
			wantMatch: true,
			missPath:  "keeper/file.txt",
			wantMiss:  true,
		},
		{
			name: "rooted prefix set exclude",
			specs: []struct {
				glob    string
				include bool
			}{
				{glob: "/{media-a,sports}/**", include: false},
			},
			matchPath: "sports/game.mp4",
			wantMatch: false,
			missPath:  "music/game.mp4",
			wantMiss:  true,
		},
		{
			name: "unrooted prefix set exclude",
			specs: []struct {
				glob    string
				include bool
			}{
				{glob: "{.tmp,.cache}/**", include: false},
			},
			matchPath: "dir/.cache/file.txt",
			wantMatch: false,
			missPath:  "dir/cache/file.txt",
			wantMiss:  true,
		},
		{
			name: "extension set exclude",
			specs: []struct {
				glob    string
				include bool
			}{
				{glob: "*.{jpg,png}", include: false},
			},
			matchPath: "dir/picture.jpg",
			wantMatch: false,
			missPath:  "dir/video.mp4",
			wantMiss:  true,
		},
		{
			name: "rooted extension include",
			specs: []struct {
				glob    string
				include bool
			}{
				{glob: "/*.rclonelink", include: true},
			},
			matchPath: "shortcut.rclonelink",
			wantMatch: true,
			missPath:  "dir/shortcut.rclonelink",
			wantMiss:  true,
		},
		{
			name: "dotfile exclude",
			specs: []struct {
				glob    string
				include bool
			}{
				{glob: ".**", include: false},
			},
			matchPath: ".hidden/file.txt",
			wantMatch: false,
			missPath:  "visible/file.txt",
			wantMiss:  true,
		},
		{
			name: "unclassified include",
			specs: []struct {
				glob    string
				include bool
			}{
				{glob: "/dir/*/file.txt", include: true},
			},
			matchPath: "dir/sub/file.txt",
			wantMatch: true,
			missPath:  "dir/deep/sub/file.txt",
			wantMiss:  true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			rules := makeTestRules(t, tc.specs)
			frs := buildFusedRuleSet(rules)
			require.NotNil(t, frs)

			assert.Equal(t, tc.wantMatch, frs.evaluate(tc.matchPath))
			if tc.missPath != "" {
				assert.Equal(t, tc.wantMiss, frs.evaluate(tc.missPath))
			}
		})
	}
}

func TestFusedEvaluateAlternatingPolarity(t *testing.T) {
	rules := makeTestRules(t, []struct {
		glob    string
		include bool
	}{
		{glob: "*.jpg", include: false},
		{glob: "/keep/**", include: true},
		{glob: "**", include: false},
	})

	frs := buildFusedRuleSet(rules)
	require.NotNil(t, frs)

	assert.False(t, frs.evaluate("photo.jpg"))
	assert.True(t, frs.evaluate("keep/important.doc"))
	assert.False(t, frs.evaluate("random.doc"))
}

func TestFusedEvaluateDeepPrefixFallback(t *testing.T) {
	specs := []struct {
		glob    string
		include bool
	}{
		{glob: "/*.rclonelink", include: true},
		{glob: "/data/archive/2024/**", include: true},
		{glob: "/mounted.bin/**", include: true},
		{glob: "**", include: false},
	}

	rules := makeTestRules(t, specs)
	frs := buildFusedRuleSet(rules)
	require.NotNil(t, frs)

	testCases := []struct {
		path string
		want bool
	}{
		{path: "data/archive/2024/report.csv", want: true},
		{path: "data/archive/2023/report.csv", want: false},
		{path: "data/archive/2024", want: false},
		{path: "data/other/2024/report.csv", want: false},
		{path: "mounted.bin/file.dat", want: true},
		{path: "test.rclonelink", want: true},
	}

	for _, tc := range testCases {
		t.Run(tc.path, func(t *testing.T) {
			assert.Equal(t, tc.want, frs.evaluate(tc.path))
			assert.Equal(t, linearScan(rules, tc.path), frs.evaluate(tc.path), "path %q", tc.path)
		})
	}
}

func TestFusedEvaluateParity(t *testing.T) {
	specs := []struct {
		glob    string
		include bool
	}{
		{glob: "{.downloads,.inbound,.recyclebin}/**", include: false},
		{glob: "{tmp,temp,trash,inbound}/**", include: false},
		{glob: "/*.rclonelink", include: true},
		{glob: "/learning/provider-a/**", include: true},
		{glob: "/learning/provider-b/**", include: true},
		{glob: "/sports/stats-db/**", include: true},
		{glob: ".**", include: false},
		{glob: "*.{bin,bif,jpg,jpeg,png,part,partial,tmp}", include: false},
		{glob: "*.{txt,pdf,nfo,nfo-orig,json,dat}", include: false},
		{glob: "/{learning,sports}/**", include: true},
		{glob: "/{media-a,media-a-int,media-a-dubbed}/**", include: true},
		{glob: "**", include: false},
	}

	rules := makeTestRules(t, specs)
	frs := buildFusedRuleSet(rules)
	require.NotNil(t, frs)

	paths := []string{
		"test.rclonelink",
		"media-a/feature.mkv",
		"media-a/poster.jpg",
		"media-a/readme.txt",
		"media-a/decade-10s.bak/Title (2024) [tag-123]/feature.mkv",
		"media-a/decade-3d/Title B (2012) [x264 DTS]/feature.nfo",
		"media-a-int/feature.mkv",
		"media-a-dubbed/feature.mkv",
		"media-b/decade-00s/Show A (2005) {id-12345}/Season 02/episode.mkv",
		"media-b/decade-00s/Show A (2005) {id-12345}/Season 02/poster.jpg",
		"sports/stats-db/game.mkv",
		"sports/highlights/poster.png",
		"learning/provider-a/lesson.mp4",
		"learning/provider-b/guide.pdf",
		"learning/random/guide.txt",
		"learning/random/guide.mp4",
		".hidden/file.txt",
		"dir/.hidden/file.txt",
		".downloads/feature.mkv",
		"dir/.downloads/feature.mkv",
		"tmp/work/file.tmp",
		"temp/work/file.mkv",
		"trash/archive/file.json",
		"inbound/drop/file.bin",
		"mounted.bin/file.txt",
		"mounted.bin/mounted.bin",
		"media-b/show/episode.mkv",
		"media-a-plus/file.mkv",
		"random/path/file.xyz",
		"random/path/file.partial",
		"sportsman/file.txt",
		".downloads2/file.txt",
		"learning-extra/file.txt",
		"tmp-old/file.txt",
	}

	for _, path := range paths {
		t.Run(path, func(t *testing.T) {
			assert.Equal(t, linearScan(rules, path), frs.evaluate(path))
		})
	}
}

func TestFusedEvaluateGoldenParity(t *testing.T) {
	testCases := []struct {
		name  string
		rules string
	}{
		{name: "standardFilterRules", rules: standardFilterRules},
		{name: "standardFilterRulesV2", rules: standardFilterRulesV2},
		{name: "stressFilterRules", rules: stressFilterRules},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			specs := parseFilterRulesString(t, tc.rules)
			rules := makeTestRules(t, specs)
			frs := buildFusedRuleSet(rules)
			require.NotNil(t, frs)

			corpus := generateGoldenCorpus(specs)
			require.GreaterOrEqual(t, len(corpus), 1000)

			for _, path := range corpus {
				assert.Equal(t, linearScan(rules, path), frs.evaluate(path), "path %q", path)
			}
		})
	}
}

func TestFusedEvaluateIgnoreCaseParity(t *testing.T) {
	specs := []struct {
		glob    string
		include bool
	}{
		{glob: "*.jpg", include: false},
		{glob: "/Keep/**", include: true},
		{glob: "{TMP,.CACHE}/**", include: false},
		{glob: "**", include: false},
	}

	rules := make([]classifiedRule, len(specs))
	for i, spec := range specs {
		re, err := GlobPathToRegexp(spec.glob, true)
		require.NoError(t, err)

		rule := classifyPattern(spec.glob, re, true)
		rule.Include = spec.include
		rules[i] = rule
		assert.Equal(t, fpUnclassified, rule.Kind)
	}

	frs := buildFusedRuleSet(rules)
	require.NotNil(t, frs)

	paths := []string{
		"PHOTO.JPG",
		"photos/album/Cover.JpG",
		"keep/Important.Doc",
		"KEEP/Posters/IMAGE.PNG",
		"work/.cache/File.TXT",
		"Work/TMP/File.BIN",
		"random.doc",
	}

	for _, path := range paths {
		t.Run(path, func(t *testing.T) {
			assert.Equal(t, linearScan(rules, path), frs.evaluate(path))
		})
	}
}
