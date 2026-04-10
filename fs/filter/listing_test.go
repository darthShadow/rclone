package filter

import (
	"context"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func tinySetPtrForTest(m map[string]struct{}) *tinySet {
	ts := tinySetFromMap(m)
	return &ts
}

func hydrateListingResidualForTest(residual *listingResidual, leafExtMaps []map[string]struct{}) *listingResidual {
	for i := range residual.leafBlocks {
		if len(leafExtMaps[i]) > 0 {
			ts := tinySetFromMap(leafExtMaps[i])
			residual.leafBlocks[i].tinyExts = &ts
		}
	}
	return residual
}

func TestLeafExtension(t *testing.T) {
	testCases := []struct {
		name string
		leaf string
		want string
	}{
		{name: "normal extension", leaf: "photo.jpg", want: ".jpg"},
		{name: "double extension", leaf: "archive.tar.gz", want: ".gz"},
		{name: "dotfile only", leaf: ".gitignore", want: ".gitignore"},
		{name: "no extension", leaf: "Makefile", want: ""},
		{name: "empty string", leaf: "", want: ""},
		{name: "dot at end", leaf: "file.", want: "."},
		{name: "multiple dots", leaf: "a.b.c.d", want: ".d"},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, leafExtension(tc.leaf))
		})
	}
}

func TestTinySetHashAndEqual(t *testing.T) {
	makeElements := func(n int) []string {
		elements := make([]string, n)
		for i := range elements {
			elements[i] = ".ext" + strconv.Itoa(i)
		}
		return elements
	}
	reverseCopy := func(in []string) []string {
		out := append([]string(nil), in...)
		for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
			out[i], out[j] = out[j], out[i]
		}
		return out
	}
	replaceLast := func(in []string, value string) []string {
		out := append([]string(nil), in...)
		out[len(out)-1] = value
		return out
	}

	boundary := makeElements(tinySetThreshold)
	overflow := makeElements(tinySetThreshold + 1)

	testCases := []struct {
		name          string
		left          []string
		same          []string
		different     []string
		wantMapBacked bool
	}{
		{
			name:      "empty set",
			left:      nil,
			same:      nil,
			different: []string{".tmp"},
		},
		{
			name:      "single element",
			left:      []string{".jpg"},
			same:      []string{".jpg"},
			different: []string{".png"},
		},
		{
			name:          "capacity boundary inline",
			left:          boundary,
			same:          reverseCopy(boundary),
			different:     replaceLast(boundary, ".other"),
			wantMapBacked: false,
		},
		{
			name:          "overflow map backed",
			left:          overflow,
			same:          reverseCopy(overflow),
			different:     replaceLast(overflow, ".other"),
			wantMapBacked: true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			left := newTinySet(tc.left)
			same := newTinySet(tc.same)
			different := newTinySet(tc.different)

			assert.Equal(t, left.hash(), same.hash(), "equivalent sets should hash identically")
			assert.True(t, left.equal(&same), "identical sets should compare equal")
			assert.False(t, left.equal(&different), "different sets should not compare equal")
			assert.Equal(t, tc.wantMapBacked, left.count == -1, "unexpected tinySet storage mode")
		})
	}
}

func TestAnalyzeParentResidual(t *testing.T) {
	anyRe := regexp.MustCompile(".*")

	testCases := []struct {
		name              string
		parent            string
		rules             []classifiedRule
		wantTerminal      bool
		wantFallback      bool
		wantRuleKinds     []fastPathKind
		wantMatchFullPath []bool
	}{
		{
			name:         "MatchAll terminal",
			parent:       "",
			rules:        []classifiedRule{{Kind: fpMatchAll, Include: false}},
			wantTerminal: true,
			wantFallback: false,
		},
		{
			name:         "MatchAll terminal with parent",
			parent:       "dir/",
			rules:        []classifiedRule{{Kind: fpMatchAll, Include: true}},
			wantTerminal: true,
			wantFallback: true,
		},
		{
			name:   "ExtensionSet survives",
			parent: "any/",
			rules: []classifiedRule{{
				Kind:       fpExtensionSet,
				Include:    false,
				Extensions: map[string]struct{}{".jpg": {}, ".png": {}},
			}},
			wantFallback:      true,
			wantRuleKinds:     []fastPathKind{fpExtensionSet},
			wantMatchFullPath: []bool{false},
		},
		{
			name:   "RootedExtension at root",
			parent: "",
			rules: []classifiedRule{{
				Kind:      fpRootedExtension,
				Include:   true,
				Extension: ".rclonelink",
			}},
			wantFallback:      true,
			wantRuleKinds:     []fastPathKind{fpRootedExtension},
			wantMatchFullPath: []bool{false},
		},
		{
			name:   "RootedExtension at depth > 0",
			parent: "sub/",
			rules: []classifiedRule{{
				Kind:      fpRootedExtension,
				Include:   true,
				Extension: ".rclonelink",
			}},
			wantFallback: true,
		},
		{
			name:   "RootedPrefixSet at root dead",
			parent: "",
			rules: []classifiedRule{{
				Kind:     fpRootedPrefixSet,
				Include:  true,
				Prefixes: map[string]struct{}{"movies": {}, "sports": {}},
			}},
			wantFallback: true,
		},
		{
			name:   "RootedPrefixSet parent matches",
			parent: "movies/",
			rules: []classifiedRule{{
				Kind:     fpRootedPrefixSet,
				Include:  true,
				Prefixes: map[string]struct{}{"movies": {}, "sports": {}},
			}},
			wantTerminal: true,
			wantFallback: true,
		},
		{
			name:   "RootedPrefixSet parent no match",
			parent: "other/",
			rules: []classifiedRule{{
				Kind:     fpRootedPrefixSet,
				Include:  true,
				Prefixes: map[string]struct{}{"movies": {}, "sports": {}},
			}},
			wantFallback: true,
		},
		{
			name:   "RootedPrefix ancestor terminal",
			parent: "movies/action/",
			rules: []classifiedRule{{
				Kind:    fpRootedPrefix,
				Include: true,
				Prefix:  "movies",
			}},
			wantTerminal: true,
			wantFallback: true,
		},
		{
			name:   "RootedPrefix mismatch dead",
			parent: "sports/",
			rules: []classifiedRule{{
				Kind:    fpRootedPrefix,
				Include: true,
				Prefix:  "movies",
			}},
			wantFallback: true,
		},
		{
			name:   "RootedPrefix descendant dead",
			parent: "",
			rules: []classifiedRule{{
				Kind:     fpRootedPrefix,
				Include:  true,
				Prefix:   "movies/action",
				Fallback: anyRe,
			}},
			wantFallback: true,
		},
		{
			name:   "RootedPrefix partial ancestor dead",
			parent: "movies/",
			rules: []classifiedRule{{
				Kind:    fpRootedPrefix,
				Include: true,
				Prefix:  "movies/action",
			}},
			wantFallback: true,
		},
		{
			name:   "UnrootedPrefixSet parent match",
			parent: "dir/.cache/",
			rules: []classifiedRule{{
				Kind:     fpUnrootedPrefixSet,
				Include:  false,
				Prefixes: map[string]struct{}{".cache": {}, ".tmp": {}},
			}},
			wantTerminal: true,
			wantFallback: false,
		},
		{
			name:   "UnrootedPrefixSet no match dead",
			parent: "dir/sub/",
			rules: []classifiedRule{{
				Kind:     fpUnrootedPrefixSet,
				Include:  false,
				Prefixes: map[string]struct{}{".cache": {}, ".tmp": {}},
			}},
			wantFallback: true,
		},
		{
			name:         "DotfileAll parent dot terminal",
			parent:       ".hidden/",
			rules:        []classifiedRule{{Kind: fpDotfileAll, Include: false}},
			wantTerminal: true,
			wantFallback: false,
		},
		{
			name:              "DotfileAll no parent dot leaf",
			parent:            "normal/",
			rules:             []classifiedRule{{Kind: fpDotfileAll, Include: false}},
			wantFallback:      true,
			wantRuleKinds:     []fastPathKind{fpDotfileAll},
			wantMatchFullPath: []bool{false},
		},
		{
			name:   "Unclassified regex fallback",
			parent: "",
			rules: []classifiedRule{{
				Kind:     fpUnclassified,
				Include:  true,
				Fallback: anyRe,
			}},
			wantFallback:      true,
			wantRuleKinds:     []fastPathKind{fpUnclassified},
			wantMatchFullPath: []bool{true},
		},
		{
			name:   "terminal with preceding leaf rules",
			parent: "",
			rules: []classifiedRule{
				{
					Kind:       fpExtensionSet,
					Include:    false,
					Extensions: map[string]struct{}{".tmp": {}},
				},
				{Kind: fpMatchAll, Include: true},
			},
			wantTerminal:      true,
			wantFallback:      true,
			wantRuleKinds:     []fastPathKind{fpExtensionSet},
			wantMatchFullPath: []bool{false},
		},
		{
			name:         "empty rules",
			parent:       "",
			wantTerminal: false,
			wantFallback: true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			analysis := analyzeParentResidual(tc.parent, tc.rules)

			assert.Equal(t, tc.wantTerminal, analysis.terminal)
			assert.Equal(t, tc.wantFallback, analysis.fallback)
			require.Len(t, analysis.listingRules, len(tc.wantRuleKinds))
			require.Len(t, tc.wantMatchFullPath, len(tc.wantRuleKinds))
			for i, rr := range analysis.listingRules {
				assert.Equal(t, tc.wantRuleKinds[i], rr.rule.Kind)
				assert.Equal(t, tc.wantMatchFullPath[i], rr.matchFullPath)
			}
		})
	}
}

func TestBuildListingResidual(t *testing.T) {
	anyRe := regexp.MustCompile(".*")

	testCases := []struct {
		name       string
		rules      []residualRule
		fallback   bool
		assertions func(t *testing.T, residual *listingResidual)
	}{
		{
			name:     "empty rules",
			fallback: true,
			assertions: func(t *testing.T, residual *listingResidual) {
				assert.Nil(t, residual.leafBlocks)
				assert.Nil(t, residual.regexBlocks)
				assert.Nil(t, residual.evalOrder)
				assert.True(t, residual.fallback)
			},
		},
		{
			name: "single leaf rule",
			rules: []residualRule{{
				rule: classifiedRule{
					Kind:       fpExtensionSet,
					Include:    true,
					Extensions: map[string]struct{}{".jpg": {}},
				},
			}},
			fallback: true,
			assertions: func(t *testing.T, residual *listingResidual) {
				require.Len(t, residual.leafBlocks, 1)
				assert.True(t, residual.leafBlocks[0].tinyExts.contains(".jpg"))
				assert.Len(t, residual.regexBlocks, 0)
				assert.Nil(t, residual.evalOrder)
			},
		},
		{
			name: "single regex rule",
			rules: []residualRule{{
				rule: classifiedRule{
					Kind:     fpUnclassified,
					Include:  false,
					Fallback: anyRe,
				},
				matchFullPath: true,
			}},
			fallback: true,
			assertions: func(t *testing.T, residual *listingResidual) {
				require.Len(t, residual.regexBlocks, 1)
				assert.Len(t, residual.leafBlocks, 0)
				assert.NotEmpty(t, residual.regexBlocks)
				assert.Nil(t, residual.evalOrder)
			},
		},
		{
			name: "two same-polarity leaf rules",
			rules: []residualRule{
				{
					rule: classifiedRule{
						Kind:       fpExtensionSet,
						Include:    true,
						Extensions: map[string]struct{}{".jpg": {}},
					},
				},
				{
					rule: classifiedRule{
						Kind:       fpExtensionSet,
						Include:    true,
						Extensions: map[string]struct{}{".png": {}},
					},
				},
			},
			fallback: true,
			assertions: func(t *testing.T, residual *listingResidual) {
				require.Len(t, residual.leafBlocks, 1)
				assert.True(t, residual.leafBlocks[0].tinyExts.contains(".jpg"))
				assert.True(t, residual.leafBlocks[0].tinyExts.contains(".png"))
			},
		},
		{
			name: "interleaved leaf-regex-leaf",
			rules: []residualRule{
				{
					rule: classifiedRule{
						Kind:       fpExtensionSet,
						Include:    true,
						Extensions: map[string]struct{}{".jpg": {}},
					},
				},
				{
					rule: classifiedRule{
						Kind:     fpUnclassified,
						Include:  false,
						Fallback: anyRe,
					},
					matchFullPath: true,
				},
				{
					rule: classifiedRule{
						Kind:    fpDotfileAll,
						Include: true,
					},
				},
			},
			fallback: true,
			assertions: func(t *testing.T, residual *listingResidual) {
				assert.Len(t, residual.leafBlocks, 2)
				assert.Len(t, residual.regexBlocks, 1)
				require.NotNil(t, residual.evalOrder)
				assert.Len(t, residual.evalOrder, 3)
				assert.Equal(t, listingEvalStep{kind: listingEvalLeaf, index: 0}, residual.evalOrder[0])
				assert.Equal(t, listingEvalStep{kind: listingEvalRegex, index: 0}, residual.evalOrder[1])
				assert.Equal(t, listingEvalStep{kind: listingEvalLeaf, index: 1}, residual.evalOrder[2])
			},
		},
		{
			name: "multi-dot rooted extension routes to regex fallback",
			rules: []residualRule{{
				rule: classifiedRule{
					Kind:      fpRootedExtension,
					Include:   true,
					Extension: ".tar.gz",
					Fallback:  regexp.MustCompile(`^[^/]*\.tar\.gz$`),
				},
			}},
			fallback: true,
			assertions: func(t *testing.T, residual *listingResidual) {
				assert.Len(t, residual.leafBlocks, 0)
				assert.Len(t, residual.regexBlocks, 1)
				assert.NotEmpty(t, residual.regexBlocks)
			},
		},
		{
			name: "unsupported kind in leaf",
			rules: []residualRule{{
				rule: classifiedRule{
					Kind:     fpRootedPrefixSet,
					Include:  true,
					Prefixes: map[string]struct{}{"movies": {}},
					Fallback: anyRe,
				},
			}},
			fallback: true,
			assertions: func(t *testing.T, residual *listingResidual) {
				assert.Len(t, residual.leafBlocks, 0)
				assert.Len(t, residual.regexBlocks, 1)
				assert.NotEmpty(t, residual.regexBlocks)
			},
		},
		{
			name:     "fallback propagation",
			fallback: false,
			assertions: func(t *testing.T, residual *listingResidual) {
				assert.False(t, residual.fallback)
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			residual, leafExtMaps := buildListingResidual(tc.rules, tc.fallback)
			residual = hydrateListingResidualForTest(residual, leafExtMaps)
			require.NotNil(t, residual)
			tc.assertions(t, residual)
		})
	}
}

func TestFlatListingCache(t *testing.T) {
	t.Run("cache miss builds", func(t *testing.T) {
		cache := newFlatListingCache(commonListingRules(), false)

		cr := cache.residualFor("dir/")

		require.NotNil(t, cr)
		require.NotNil(t, cr.residual)
	})

	t.Run("cache hit returns same", func(t *testing.T) {
		cache := newFlatListingCache(commonListingRules(), false)

		first := cache.residualFor("dir/")
		second := cache.residualFor("dir/")

		require.Same(t, first, second)
	})

	t.Run("different parent", func(t *testing.T) {
		cache := newFlatListingCache(commonListingRules(), false)

		first := cache.residualFor("dir/")
		second := cache.residualFor("other/")

		require.NotNil(t, first)
		require.NotNil(t, second)
		require.NotSame(t, first, second)
	})

	t.Run("terminal flag", func(t *testing.T) {
		cache := newFlatListingCache([]classifiedRule{{Kind: fpMatchAll, Include: true}}, false)

		cr := cache.residualFor("any/")

		require.True(t, cr.terminal)
		assert.True(t, cr.termResult)
	})

	t.Run("pointer stability", func(t *testing.T) {
		cache := newFlatListingCache(commonListingRules(), false)

		first := cache.residualFor("dir-0/")
		for i := 1; i < 400; i++ {
			parent := string([]rune{
				'd', 'i', 'r', '-',
				rune('a' + (i/26)%26),
				rune('a' + i%26),
				'/',
			})
			cache.residualFor(parent)
		}
		again := cache.residualFor("dir-0/")

		require.Same(t, first, again)
		require.NotNil(t, first.residual)
	})

	t.Run("root directory", func(t *testing.T) {
		cache := newFlatListingCache(commonListingRules(), false)

		cr := cache.residualFor("")

		require.NotNil(t, cr)
		require.NotNil(t, cr.residual)
	})
}

func TestFlatListingCacheConcurrent(t *testing.T) {
	cache := newFlatListingCache(commonListingRules(), false)

	const workers = 32
	results := make([]*cachedResidual, workers)
	start := make(chan struct{})
	var wg sync.WaitGroup

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			<-start
			results[idx] = cache.residualFor("shared/")
		}(i)
	}

	close(start)
	wg.Wait()

	require.NotNil(t, results[0])
	for i := 1; i < len(results); i++ {
		require.NotNil(t, results[i])
		assert.Same(t, results[0], results[i], "goroutine %d returned a different cached residual", i)
	}

	other := cache.residualFor("other/")
	require.NotNil(t, other)
	assert.NotSame(t, results[0], other)
}

func TestBatchMaskArithmetic(t *testing.T) {
	t.Run("evalLeafBlockBatch full match", func(t *testing.T) {
		block := listingLeafBlock{tinyExts: tinySetPtrForTest(map[string]struct{}{".jpg": {}})}

		mask := evalLeafBlockBatch(&block, []string{"a.jpg", "b.jpg"})

		assert.Equal(t, uint64(0b11), mask)
	})

	t.Run("evalLeafBlockBatch partial match", func(t *testing.T) {
		block := listingLeafBlock{tinyExts: tinySetPtrForTest(map[string]struct{}{".jpg": {}})}

		mask := evalLeafBlockBatch(&block, []string{"a.jpg", "b.png"})

		assert.Equal(t, uint64(0b01), mask)
	})

	t.Run("evalLeafBlockBatch dotfile", func(t *testing.T) {
		block := listingLeafBlock{dotfile: true}

		mask := evalLeafBlockBatch(&block, []string{".git", "readme"})

		assert.Equal(t, uint64(0b01), mask)
	})

	t.Run("evalLeafBlockBatch empty leaf", func(t *testing.T) {
		block := listingLeafBlock{tinyExts: tinySetPtrForTest(map[string]struct{}{".jpg": {}})}

		mask := evalLeafBlockBatch(&block, []string{"", "a.jpg"})

		assert.Equal(t, uint64(0b10), mask)
	})

	t.Run("applyBatchResults include", func(t *testing.T) {
		results := make([]bool, 4)

		applyBatchResults(results, 0, 0b0101, true)

		assert.Equal(t, []bool{true, false, true, false}, results)
	})

	t.Run("applyBatchResults exclude", func(t *testing.T) {
		results := []bool{true, true, true, true}

		applyBatchResults(results, 0, 0b1010, false)

		assert.Equal(t, []bool{true, false, true, false}, results)
	})

	t.Run("applyBatchResults with offset", func(t *testing.T) {
		results := make([]bool, 8)

		applyBatchResults(results, 4, 0b0001, true)

		assert.Equal(t, []bool{false, false, false, false, true, false, false, false}, results)
	})

	t.Run("allBits full chunk", func(t *testing.T) {
		leaves := make([]string, listingBatchSize)
		for i := range leaves {
			leaves[i] = ".hidden"
		}

		block := listingLeafBlock{dotfile: true}
		mask := evalLeafBlockBatch(&block, leaves)

		assert.Equal(t, ^uint64(0), mask)
	})

	t.Run("allBits partial chunk", func(t *testing.T) {
		leaves := make([]string, 7)
		for i := range leaves {
			leaves[i] = ".hidden"
		}

		block := listingLeafBlock{dotfile: true}
		mask := evalLeafBlockBatch(&block, leaves)

		assert.Equal(t, uint64((1<<7)-1), mask)
	})
}

func listingGoldenLeaves(_ string) []string {
	leaves := []string{
		"photo.jpg",
		"readme.txt",
		".hidden",
		"noext",
		"archive.tmp",
		"data.bin",
		"movie.mkv",
		"guide.pdf",
		"test.rclonelink",
	}
	return append([]string(nil), leaves...)
}

func TestRunListingParity(t *testing.T) {
	testCases := []struct {
		name          string
		rules         []classifiedRule
		parent        string
		leaves        []string
		wantEvalOrder bool
	}{
		{
			name:   "root directory mixed",
			rules:  commonListingRules(),
			parent: "",
			leaves: []string{"photo.jpg", ".hidden", "readme.txt", "file.tmp"},
		},
		{
			name:   "subdirectory",
			rules:  commonListingRules(),
			parent: "sub/",
			leaves: []string{"a.jpg", "b.png", ".git", "c.tmp", "doc.pdf"},
		},
		{
			name:   "large batch (>64 entries)",
			rules:  commonListingRules(),
			parent: "dir/",
			leaves: generatedLeaves(100),
		},
		{
			name:   "small batch (<threshold)",
			rules:  commonListingRules(),
			parent: "dir/",
			leaves: []string{"a.jpg", "b.tmp"},
		},
		{
			name: "interleaved leaf and regex blocks",
			rules: []classifiedRule{
				{
					Kind:       fpExtensionSet,
					Include:    true,
					Extensions: map[string]struct{}{".jpg": {}},
				},
				{
					Kind:     fpUnclassified,
					Include:  false,
					Fallback: regexp.MustCompile(`^.*\.log$`),
				},
				{
					Kind:    fpDotfileAll,
					Include: false,
				},
			},
			parent:        "",
			leaves:        []string{"photo.jpg", "debug.log", ".hidden", "readme.txt"},
			wantEvalOrder: true,
		},
		{
			name: "terminal parent",
			rules: []classifiedRule{{
				Kind:     fpRootedPrefix,
				Include:  true,
				Prefix:   "movies",
				Fallback: regexp.MustCompile(`^movies/`),
			}},
			parent: "movies/",
			leaves: []string{"a.mkv", "b.tmp", ".hidden"},
		},
		{
			name:   "empty leaves",
			rules:  commonListingRules(),
			parent: "dir/",
			leaves: nil,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			cache := newFlatListingCache(tc.rules, false)
			evaluator := &batchListingEvaluator{cache: cache}

			analysis := analyzeParentResidual(tc.parent, tc.rules)
			residual, leafExtMaps := buildListingResidual(analysis.listingRules, analysis.fallback)
			residual = hydrateListingResidualForTest(residual, leafExtMaps)
			if tc.wantEvalOrder {
				require.NotEmpty(t, residual.evalOrder)
			}

			scalarResults := make([]bool, len(tc.leaves))
			fillBool(scalarResults, residual.fallback)
			scalarEvalResidual(residual, tc.parent, tc.leaves, scalarResults)

			batchResults := make([]bool, len(tc.leaves))
			evaluator.runListing(context.Background(), tc.parent, tc.leaves, batchResults)

			require.Len(t, batchResults, len(scalarResults))
			for i := range tc.leaves {
				require.Equal(t, scalarResults[i], batchResults[i], "path %d: %s", i, tc.leaves[i])

				perLeafScalar := []bool{residual.fallback}
				scalarEvalResidual(residual, tc.parent, []string{tc.leaves[i]}, perLeafScalar)
				require.Equal(t, perLeafScalar[0], batchResults[i], "per-leaf parity mismatch for %q", tc.leaves[i])
			}
			if len(tc.leaves) == 0 {
				assert.Empty(t, batchResults)
			}
		})
	}
}

func TestListingGoldenParity(t *testing.T) {
	parents := []string{
		"",
		"media-a/",
		"media-b/",
		"learning/",
		"learning/provider-a/",
		"sports/",
		"sports/stats-db/",
		"dir/.hidden/",
		".downloads/",
		"data/",
		"data/deep/",
		"data/deep/nested/",
		"tmp/",
		"other/",
		"media-a/sub-dir/",
	}
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
			fused := buildFusedRuleSet(rules)
			evaluator := &batchListingEvaluator{cache: newFlatListingCache(rules, false)}

			for _, parent := range parents {
				name := parent
				if name == "" {
					name = "root"
				}

				t.Run(name, func(t *testing.T) {
					leaves := listingGoldenLeaves(parent)
					require.NotEmpty(t, leaves, "parent %q produced no leaves", parent)
					results := make([]bool, len(leaves))
					evaluator.runListing(context.Background(), parent, leaves, results)

					require.Len(t, results, len(leaves))
					for i, leaf := range leaves {
						assert.Equal(t, fused.evaluate(parent+leaf), results[i], "parent %q leaf %q", parent, leaf)
					}
				})
			}
		})
	}
}

func TestListingVsFusedParity(t *testing.T) {
	rules := []classifiedRule{
		{
			Kind:     fpRootedPrefix,
			Include:  true,
			Prefix:   "movies",
			Fallback: regexp.MustCompile(`^movies/`),
		},
		{
			Kind:       fpExtensionSet,
			Include:    false,
			Extensions: map[string]struct{}{".tmp": {}},
		},
		{
			Kind:    fpMatchAll,
			Include: true,
		},
	}

	fused := buildFusedRuleSet(rules)
	evaluator := &batchListingEvaluator{cache: newFlatListingCache(rules, false)}

	testCases := []struct {
		name     string
		parent   string
		leaves   []string
		expected []bool
	}{
		{
			name:     "movies subdir parity",
			parent:   "movies/",
			leaves:   []string{"file.mkv", "temp.tmp", ".hidden", "other.jpg"},
			expected: []bool{true, true, true, true},
		},
		{
			name:     "root parity",
			parent:   "",
			leaves:   []string{"file.mkv", "temp.tmp", ".hidden", "other.jpg"},
			expected: []bool{true, false, true, true},
		},
		{
			name:     "non-matching parent",
			parent:   "sports/",
			leaves:   []string{"game.mkv", "temp.tmp", ".hidden"},
			expected: []bool{true, false, true},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			batchResults := make([]bool, len(tc.leaves))
			evaluator.runListing(context.Background(), tc.parent, tc.leaves, batchResults)

			fusedResults := make([]bool, len(tc.leaves))
			for i, leaf := range tc.leaves {
				fusedResults[i] = fused.evaluate(tc.parent + leaf)
			}

			require.Equal(t, tc.expected, fusedResults)
			require.Equal(t, fusedResults, batchResults, "parity mismatch for parent %q", tc.parent)
		})
	}
}

func TestListingIgnoreCase(t *testing.T) {
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
		assert.Equal(t, fpUnclassified, rule.Kind)
		rules[i] = rule
	}

	fused := buildFusedRuleSet(rules)
	evaluator := &batchListingEvaluator{cache: newFlatListingCache(rules, true)}

	testCases := []struct {
		name   string
		parent string
		leaves []string
	}{
		{
			name:   "root",
			parent: "",
			leaves: []string{"PHOTO.JPG", "random.doc", "test.rclonelink"},
		},
		{
			name:   "keep parent",
			parent: "keep/",
			leaves: []string{"Important.Doc", "Cover.JPG"},
		},
		{
			name:   "tmp parent",
			parent: "Work/TMP/",
			leaves: []string{"File.BIN", "Poster.JPG"},
		},
		{
			name:   "cache parent",
			parent: "work/.cache/",
			leaves: []string{"File.TXT", "cover.jpg"},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			cr := evaluator.cache.residualFor(tc.parent)
			require.NotNil(t, cr)
			require.NotNil(t, cr.residual)
			assert.Empty(t, cr.residual.leafBlocks)
			assert.NotEmpty(t, cr.residual.regexBlocks)

			results := make([]bool, len(tc.leaves))
			evaluator.runListing(context.Background(), tc.parent, tc.leaves, results)

			for i, leaf := range tc.leaves {
				assert.Equal(t, fused.evaluate(tc.parent+leaf), results[i], "parent %q leaf %q", tc.parent, leaf)
			}
		})
	}
}

func TestRunListingCancellation(t *testing.T) {
	rules := commonListingRules()
	leaves := generatedLeaves(100)
	parent := "dir/"

	cache := newFlatListingCache(rules, false)
	evaluator := &batchListingEvaluator{cache: cache}
	analysis := analyzeParentResidual(parent, rules)
	residual, leafExtMaps := buildListingResidual(analysis.listingRules, analysis.fallback)
	residual = hydrateListingResidualForTest(residual, leafExtMaps)

	t.Run("cancelled context partial results", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		results := make([]bool, len(leaves))
		evaluator.runListing(ctx, parent, leaves, results)

		expected := make([]bool, len(leaves))
		fillBool(expected, residual.fallback)
		firstChunkSize := min(len(leaves), listingBatchSize)
		scalarEvalResidual(residual, parent, leaves[:firstChunkSize], expected[:firstChunkSize])

		require.Equal(t, expected, results)
	})

	t.Run("non-cancelled completes", func(t *testing.T) {
		results := make([]bool, len(leaves))
		evaluator.runListing(context.Background(), parent, leaves, results)

		expected := make([]bool, len(leaves))
		fillBool(expected, residual.fallback)
		scalarEvalResidual(residual, parent, leaves, expected)

		require.Equal(t, expected, results)
	})
}

func TestDebugAssertionChunkSize(t *testing.T) {
	block := listingLeafBlock{tinyExts: tinySetPtrForTest(map[string]struct{}{".jpg": {}})}
	leaves := make([]string, listingBatchSize+1)

	require.Panics(t, func() {
		evalLeafBlockBatch(&block, leaves)
	})
}

func TestFillBool(t *testing.T) {
	t.Run("fill true", func(t *testing.T) {
		buf := []bool{false, false, false}

		fillBool(buf, true)

		assert.Equal(t, []bool{true, true, true}, buf)
	})

	t.Run("fill false", func(t *testing.T) {
		buf := []bool{true, true, true}

		fillBool(buf, false)

		assert.Equal(t, []bool{false, false, false}, buf)
	})

	t.Run("fill empty", func(t *testing.T) {
		var buf []bool

		require.NotPanics(t, func() {
			fillBool(buf, true)
		})
		assert.Len(t, buf, 0)
	})
}

func TestCountComponents(t *testing.T) {
	testCases := []struct {
		name string
		path string
		want int
	}{
		{name: "empty string", path: "", want: 0},
		{name: "single component", path: "dir", want: 1},
		{name: "trailing slash", path: "dir/", want: 1},
		{name: "two components", path: "a/b/", want: 2},
		{name: "consecutive slashes", path: "a//b/", want: 2},
		{name: "leading slash", path: "/a/b", want: 2},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, countComponents(tc.path))
		})
	}
}

func TestFlatListingCacheLRUEviction(t *testing.T) {
	rules := commonListingRules()
	cache := newFlatListingCache(rules, false)
	cache.maxResiduals = 5

	// Insert 10 parents: dir0/ through dir9/
	// With maxResiduals=5, the first 5 inserted (dir0-dir4) should be evicted
	// as dir5-dir9 push them out.
	ptrs := make([]*cachedResidual, 10)
	for i := 0; i < 10; i++ {
		parent := "dir" + strconv.Itoa(i) + "/"
		ptrs[i] = cache.residualFor(parent)
		require.NotNil(t, ptrs[i])
	}

	// Verify cache size is capped at maxResiduals
	cache.mu.Lock()
	assert.Equal(t, 5, cache.lruList.Len())
	cache.mu.Unlock()

	// Verify: first 5 parents (dir0-dir4) were evicted from the cache
	cache.mu.Lock()
	for i := 0; i < 5; i++ {
		parent := "dir" + strconv.Itoa(i) + "/"
		_, ok := cache.lruIndex[parent]
		assert.False(t, ok, "dir%d/ should have been evicted", i)
	}
	cache.mu.Unlock()

	// Verify: last 5 parents (dir5-dir9) are still cached
	cache.mu.Lock()
	for i := 5; i < 10; i++ {
		parent := "dir" + strconv.Itoa(i) + "/"
		_, ok := cache.lruIndex[parent]
		assert.True(t, ok, "dir%d/ should still be in cache", i)
	}
	cache.mu.Unlock()

	// Verify: re-requesting an evicted parent re-builds correctly
	rebuilt := cache.residualFor("dir0/")
	require.NotNil(t, rebuilt)
	require.NotNil(t, rebuilt.residual)
	// It should be a fresh build (different pointer than the evicted one)
	assert.NotSame(t, ptrs[0], rebuilt)

	// Verify: the rebuilt residual evaluates correctly (same as a fresh build)
	leaves := []string{"photo.jpg", "temp.tmp", ".hidden", "readme.txt"}
	results := make([]bool, len(leaves))
	evaluator := &batchListingEvaluator{cache: cache}
	evaluator.runListing(context.Background(), "dir0/", leaves, results)

	freshCache := newFlatListingCache(rules, false)
	freshEval := &batchListingEvaluator{cache: freshCache}
	freshResults := make([]bool, len(leaves))
	freshEval.runListing(context.Background(), "dir0/", leaves, freshResults)
	assert.Equal(t, freshResults, results, "rebuilt residual should evaluate identically to fresh")

	// Verify: active pointers to evicted residuals remain valid (GC safety)
	// ptrs[0] was evicted from the cache but the pointer itself must still work
	evictedResidual := ptrs[0].residual
	require.NotNil(t, evictedResidual, "evicted residual pointer must remain valid")
	evictedResults := make([]bool, len(leaves))
	fillBool(evictedResults, evictedResidual.fallback)
	scalarEvalResidual(evictedResidual, "dir0/", leaves, evictedResults)
	assert.Equal(t, freshResults, evictedResults, "evicted residual should still evaluate correctly after eviction")
}

func TestFlatListingCacheLRUPromotion(t *testing.T) {
	cache := newFlatListingCache(commonListingRules(), false)
	cache.maxResiduals = 4

	original := make(map[string]*cachedResidual, 4)
	for i := 0; i < 4; i++ {
		parent := "dir" + strconv.Itoa(i) + "/"
		original[parent] = cache.residualFor(parent)
		require.NotNil(t, original[parent])
	}

	promoted := cache.residualFor("dir0/")
	require.Same(t, original["dir0/"], promoted, "access should reuse the cached entry before promotion")

	cache.residualFor("dir4/")
	cache.residualFor("dir5/")

	cache.mu.Lock()
	_, dir0Cached := cache.lruIndex["dir0/"]
	_, dir1Cached := cache.lruIndex["dir1/"]
	_, dir2Cached := cache.lruIndex["dir2/"]
	_, dir3Cached := cache.lruIndex["dir3/"]
	_, dir4Cached := cache.lruIndex["dir4/"]
	_, dir5Cached := cache.lruIndex["dir5/"]
	cache.mu.Unlock()

	assert.True(t, dir0Cached, "recently accessed entry should remain in cache")
	assert.False(t, dir1Cached, "least recently used entry should be evicted first")
	assert.False(t, dir2Cached, "next least recently used entry should be evicted second")
	assert.True(t, dir3Cached, "more recent untouched entry should remain cached")
	assert.True(t, dir4Cached, "new entry should be cached")
	assert.True(t, dir5Cached, "new entry should be cached")

	assert.Same(t, original["dir0/"], cache.residualFor("dir0/"), "promoted entry should survive later evictions")
}

func TestInternTinySetCollision(t *testing.T) {
	cache := newFlatListingCache(nil, false)

	// Create two different tinySets
	tsA := tinySetFromMap(map[string]struct{}{".jpg": {}, ".png": {}})
	tsB := tinySetFromMap(map[string]struct{}{".gif": {}, ".bmp": {}})

	cache.mu.Lock()
	internedA := cache.internTinySet(&tsA)
	internedB := cache.internTinySet(&tsB)
	cache.mu.Unlock()

	// Different content must produce different pointers
	assert.NotSame(t, internedA, internedB, "different tinySets must not alias")

	// Create a duplicate of A and intern it; must return the same pointer as A
	tsA2 := tinySetFromMap(map[string]struct{}{".jpg": {}, ".png": {}})
	cache.mu.Lock()
	internedA2 := cache.internTinySet(&tsA2)
	cache.mu.Unlock()

	assert.Same(t, internedA, internedA2, "identical tinySets must return the same interned pointer")

	// Verify the interned sets still function correctly
	assert.True(t, internedA.contains(".jpg"))
	assert.True(t, internedA.contains(".png"))
	assert.False(t, internedA.contains(".gif"))
	assert.True(t, internedB.contains(".gif"))
	assert.True(t, internedB.contains(".bmp"))
	assert.False(t, internedB.contains(".jpg"))
}

func TestInternTinySetCollisionBucket(t *testing.T) {
	cache := newFlatListingCache(nil, false)

	tsA := tinySetFromMap(map[string]struct{}{".jpg": {}, ".png": {}})
	tsB := tinySetFromMap(map[string]struct{}{".gif": {}, ".bmp": {}})
	tsA2 := tinySetFromMap(map[string]struct{}{".png": {}, ".jpg": {}})

	cache.mu.Lock()
	bucketHash := tsA.hash()
	cache.tinyIntern[bucketHash] = []*tinySet{&tsB}
	internedA := cache.internTinySet(&tsA)
	internedA2 := cache.internTinySet(&tsA2)
	bucket := cache.tinyIntern[bucketHash]
	cache.mu.Unlock()

	require.Len(t, bucket, 2, "collision bucket should retain distinct tinySets")
	assert.Same(t, &tsB, bucket[0], "existing non-equal bucket entry should be preserved")
	assert.Same(t, internedA, bucket[1], "new non-equal tinySet should be appended to the bucket")
	assert.Same(t, internedA, internedA2, "identical tinySets should still deduplicate within a populated bucket")
	assert.NotSame(t, &tsB, internedA, "different tinySets in the same bucket must not alias")
}

func TestListingRegexBlockMatchBytes(t *testing.T) {
	longLeaf := strings.Repeat("segment-", 20) + "tail.log"

	testCases := []struct {
		name    string
		parent  string
		leaf    string
		pattern string
	}{
		{
			name:    "ascii short name match",
			parent:  "media/",
			leaf:    "photo.jpg",
			pattern: `^media/.*\.jpg$`,
		},
		{
			name:    "ascii short name no match",
			parent:  "media/",
			leaf:    "notes.txt",
			pattern: `^media/.*\.jpg$`,
		},
		{
			name:    "utf8 path match",
			parent:  "musica/",
			leaf:    "nino-東京.txt",
			pattern: `^musica/nino-東京\.txt$`,
		},
		{
			name:    "special regex characters",
			parent:  "docs/",
			leaf:    "[draft](1)?.md",
			pattern: `^docs/\[draft\]\(1\)\?\.md$`,
		},
		{
			name:    "long bytes path match",
			parent:  "archive/",
			leaf:    longLeaf,
			pattern: "^archive/" + regexp.QuoteMeta(longLeaf) + "$",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			re := regexp.MustCompile(tc.pattern)
			block := listingRegexBlock{matchers: []*regexp.Regexp{re}}

			want := re.MatchString(tc.parent + tc.leaf)
			assert.Equal(t, want, block.matchBytes(tc.parent, tc.leaf))
		})
	}
}

func TestListingRegexBlockMatchBytes_MultiMatcher(t *testing.T) {
	testCases := []struct {
		name     string
		matchers []*regexp.Regexp
		parent   string
		leaf     string
		want     bool
	}{
		{
			name:     "empty matcher slice returns false",
			matchers: nil,
			parent:   "media/",
			leaf:     "photo.jpg",
			want:     false,
		},
		{
			name: "nil matcher skipped and real matcher checked",
			matchers: []*regexp.Regexp{
				nil,
				regexp.MustCompile(`^media/.*\.jpg$`),
			},
			parent: "media/",
			leaf:   "photo.jpg",
			want:   true,
		},
		{
			name: "first matcher misses second matcher hits",
			matchers: []*regexp.Regexp{
				regexp.MustCompile(`^media/.*\.png$`),
				regexp.MustCompile(`^media/.*\.jpg$`),
			},
			parent: "media/",
			leaf:   "photo.jpg",
			want:   true,
		},
		{
			name: "all matchers miss",
			matchers: []*regexp.Regexp{
				regexp.MustCompile(`^media/.*\.png$`),
				regexp.MustCompile(`^archive/.*$`),
			},
			parent: "media/",
			leaf:   "photo.jpg",
			want:   false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			block := listingRegexBlock{matchers: tc.matchers}
			assert.Equal(t, tc.want, block.matchBytes(tc.parent, tc.leaf))
		})
	}
}

func commonListingRules() []classifiedRule {
	return []classifiedRule{
		{
			Kind:       fpExtensionSet,
			Include:    true,
			Extensions: map[string]struct{}{".jpg": {}, ".png": {}},
		},
		{
			Kind:       fpExtensionSet,
			Include:    false,
			Extensions: map[string]struct{}{".tmp": {}},
		},
		{
			Kind:    fpDotfileAll,
			Include: false,
		},
		{
			Kind:    fpMatchAll,
			Include: true,
		},
	}
}

func generatedLeaves(n int) []string {
	leaves := make([]string, n)
	patterns := []string{
		"photo.jpg",
		"art.png",
		".hidden",
		"note.txt",
		"temp.tmp",
		"README",
	}
	for i := range n {
		leaves[i] = patterns[i%len(patterns)]
	}
	return leaves
}
