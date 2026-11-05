package vfs

import (
	"context"
	"testing"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/vfs/vfscommon"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestVFSInodeStability(t *testing.T) {
	for _, route := range []string{"ReadDirAll", "Stat"} {
		t.Run(route, func(t *testing.T) {
			for _, refresh := range []string{"Expired", "CacheCleanup", "FlushDirCache", "ForgetPath"} {
				t.Run(refresh, func(t *testing.T) {
					opt := vfscommon.Opt
					// Age the cache clocks explicitly so cleanup cannot race the test.
					opt.DirCacheTime = fs.Duration(time.Hour)
					opt.PollInterval = 0
					opt.CacheMode = vfscommon.CacheModeOff
					r, vfs := newTestVFSOpt(t, &opt)
					item := r.WriteObject(context.Background(), "top/sub/file", "unchanged contents", t1)
					r.CheckRemoteItems(t, item)

					root, err := vfs.Root()
					require.NoError(t, err)
					pruner := &recordingPruner{}
					vfs.SetPruner(pruner, pruner)
					t.Cleanup(func() { vfs.SetPruner(pruner, nil) })

					paths := []struct {
						name  string
						path  string
						isDir bool
					}{
						{"RootDirectory", "top", true},
						{"Subdirectory", "top/sub", true},
						{"RegularFile", "top/sub/file", false},
					}
					lookup := func() []Node {
						t.Helper()
						if route == "ReadDirAll" {
							_, err := root.ReadDirAll()
							require.NoError(t, err)
						}
						nodes := make([]Node, len(paths))
						for i, target := range paths {
							node, err := vfs.Stat(target.path)
							require.NoError(t, err)
							require.Equal(t, target.isDir, node.IsDir(), "%s", target.path)
							nodes[i] = node
							if route == "ReadDirAll" && target.isDir {
								_, err := node.(*Dir).ReadDirAll()
								require.NoError(t, err)
							}
						}
						return nodes
					}

					before := lookup()
					inodes := make([]uint64, len(before))
					for i, node := range before {
						inodes[i] = node.Inode()
						require.NotZero(t, inodes[i])
					}

					// Both the directory listing and per-leaf stat have freshness clocks.
					// Keep the nodes, but move both clocks past DirCacheTime.
					expired := time.Now().Add(-2 * time.Duration(opt.DirCacheTime))
					root.walk(func(dir *Dir) {
						if !dir.read.IsZero() {
							dir.read = expired
						}
						for leaf := range dir.statRead {
							dir.statRead[leaf] = expired
						}
						_, stale := dir._age(time.Now())
						require.True(t, stale)
					})
					require.Empty(t, pruner.callsSnapshot())

					switch refresh {
					case "CacheCleanup":
						// Invoke the timer callback synchronously on the expired root.
						root.cacheCleanup()
					case "FlushDirCache":
						vfs.FlushDirCache()
					case "ForgetPath":
						root.ForgetPath("top", fs.EntryDirectory)
					}
					if refresh != "Expired" {
						require.NotEmpty(t, pruner.callsSnapshot(), "forgetting must reach inode pruning")
					}

					refreshedAt := time.Now()
					after := lookup()
					parents := []*Dir{root, after[0].(*Dir), after[1].(*Dir)}
					leaves := []string{"top", "sub", "file"}
					for i, parent := range parents {
						parent.mu.RLock()
						read, statRead := parent.read, parent.statRead[leaves[i]]
						parent.mu.RUnlock()
						if route == "Stat" && !statRead.Before(refreshedAt) {
							t.Logf("%s: refreshed via fs.Stater", paths[i].path)
							require.True(t, read.IsZero() || read.Equal(expired), "Stat must not refresh the listing clock")
						} else {
							require.False(t, read.Before(refreshedAt), "%s: must refresh the directory listing", paths[i].path)
							if route == "Stat" {
								t.Logf("%s: Stat used directory-listing fallback", paths[i].path)
							}
						}
					}
					if refresh == "Expired" {
						assert.Empty(t, pruner.callsSnapshot(), "unchanged entries must not be pruned by refresh")
					}
					for i, target := range paths {
						t.Run(target.name, func(t *testing.T) {
							t.Logf("%s: inode before=%d after=%d", target.path, inodes[i], after[i].Inode())
							assert.Equal(t, inodes[i], after[i].Inode(), "%s: unchanged remote entry must retain its inode", target.path)
						})
					}
				})
			}
		})
	}
}
