// Package hasher implements a checksum handling overlay backend
package hasher

import (
	"bytes"
	"context"
	"encoding/gob"
	"errors"
	"fmt"
	"hash"
	"io"
	"math"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/cache"
	"github.com/rclone/rclone/fs/config/configmap"
	"github.com/rclone/rclone/fs/config/configstruct"
	"github.com/rclone/rclone/fs/fspath"
	rclonehash "github.com/rclone/rclone/fs/hash"
	"github.com/rclone/rclone/fs/list"
	"github.com/rclone/rclone/fs/operations"
	"github.com/rclone/rclone/lib/kv"
)

// Register with Fs
func init() {
	fs.Register(&fs.RegInfo{
		Name:        "hasher",
		Description: "Better checksums for other remotes",
		NewFs:       NewFs,
		MetadataInfo: &fs.MetadataInfo{
			Help: `Any metadata supported by the underlying remote is read and written.`,
		},
		CommandHelp: commandHelp,
		Options: []fs.Option{{
			Name:     "remote",
			Required: true,
			Help:     "Remote to cache checksums for (e.g. myRemote:path).",
		}, {
			Name:     "hashes",
			Default:  fs.CommaSepList{"md5", "sha1"},
			Advanced: false,
			Help:     "Comma separated list of supported checksum types.",
		}, {
			Name:     "max_age",
			Advanced: false,
			Default:  fs.DurationOff,
			Help:     "Maximum time to keep checksums in cache (0 = no cache, off = cache forever).",
		}, {
			Name:     "auto_size",
			Advanced: true,
			Default:  fs.SizeSuffix(0),
			Help:     "Auto-update checksum for files smaller than this size (disabled by default).",
		}},
	})
}

// Options defines the configuration for this backend
type Options struct {
	Remote   string          `config:"remote"`
	Hashes   fs.CommaSepList `config:"hashes"`
	AutoSize fs.SizeSuffix   `config:"auto_size"`
	MaxAge   fs.Duration     `config:"max_age"`
}

// Fs represents a wrapped fs.Fs
type Fs struct {
	fs.Fs
	name     string
	root     string
	wrapper  fs.Fs
	features *fs.Features
	opt      *Options
	db       *kv.DB
	// fingerprinting
	fpTime bool               // true if using time in fingerprints
	fpHash rclonehash.Type    // hash type to use in fingerprints or None
	// hash types triaged by groups
	suppHashes rclonehash.Set // all supported checksum types
	passHashes rclonehash.Set // passed directly to the base without caching
	slowHashes rclonehash.Set // passed to the base and then cached
	autoHashes rclonehash.Set // calculated in-house and cached
	keepHashes rclonehash.Set // checksums to keep in cache (slow + auto)
}

var warnExperimental sync.Once

// NewFs constructs an Fs from the remote:path string
func NewFs(ctx context.Context, fsname, rpath string, cmap configmap.Mapper) (fs.Fs, error) {
	if !kv.Supported() {
		return nil, errors.New("hasher is not supported on this OS")
	}
	warnExperimental.Do(func() {
		fs.Infof(nil, "Hasher is EXPERIMENTAL!")
	})

	opt := &Options{}
	err := configstruct.Set(cmap, opt)
	if err != nil {
		return nil, err
	}

	if strings.HasPrefix(opt.Remote, fsname+":") {
		return nil, errors.New("can't point remote at itself")
	}
	remotePath := fspath.JoinRootPath(opt.Remote, rpath)
	baseFs, err := cache.Get(ctx, remotePath)
	if err != nil && err != fs.ErrorIsFile {
		return nil, fmt.Errorf("failed to derive base remote %q: %w", opt.Remote, err)
	}

	f := &Fs{
		Fs:   baseFs,
		name: fsname,
		root: rpath,
		opt:  opt,
	}
	// Correct root if definitely pointing to a file
	if err == fs.ErrorIsFile {
		f.root = path.Dir(f.root)
		if f.root == "." || f.root == "/" {
			f.root = ""
		}
	}
	baseFeatures := baseFs.Features()
	f.fpTime = baseFs.Precision() != fs.ModTimeNotSupported

	if baseFeatures.SlowHash {
		f.slowHashes = f.Fs.Hashes()
	} else {
		f.passHashes = f.Fs.Hashes()
		f.fpHash = f.passHashes.GetOne()
	}

	f.suppHashes = f.passHashes
	f.suppHashes.Add(f.slowHashes.Array()...)

	for _, hashName := range opt.Hashes {
		var ht rclonehash.Type
		if err := ht.Set(hashName); err != nil {
			return nil, fmt.Errorf("invalid token %q in hash string %q", hashName, opt.Hashes.String())
		}
		if !f.slowHashes.Contains(ht) {
			f.autoHashes.Add(ht)
		}
		f.keepHashes.Add(ht)
		f.suppHashes.Add(ht)
	}

	fs.Debugf(f, "Groups by usage: cached %s, passed %s, auto %s, slow %s, supported %s",
		f.keepHashes, f.passHashes, f.autoHashes, f.slowHashes, f.suppHashes)

	var nilSet rclonehash.Set
	if f.keepHashes == nilSet {
		return nil, errors.New("configured hash_names have nothing to keep in cache")
	}

	if f.opt.MaxAge > 0 {
		gob.Register(hashRecord{})
		db, err := kv.Start(ctx, "hasher", f.Fs)
		if err != nil {
			return nil, err
		}
		f.db = db
	}

	stubFeatures := &fs.Features{
		CanHaveEmptyDirectories:  true,
		IsLocal:                  true,
		ReadMimeType:             true,
		WriteMimeType:            true,
		SetTier:                  true,
		GetTier:                  true,
		ReadMetadata:             true,
		WriteMetadata:            true,
		UserMetadata:             true,
		ReadDirMetadata:          true,
		WriteDirMetadata:         true,
		WriteDirSetModTime:       true,
		UserDirMetadata:          true,
		DirModTimeUpdatesOnWrite: true,
		PartialUploads:           true,
	}
	f.features = stubFeatures.Fill(ctx, f).Mask(ctx, f.Fs).WrapsFs(f, f.Fs)

	// Enable ListP always
	f.features.ListP = f.ListP

	// Enable OpenChunkWriter if underlying backend supports it or OpenWriterAt
	if f.Fs.Features().OpenChunkWriter != nil || f.Fs.Features().OpenWriterAt != nil {
		f.features.OpenChunkWriter = f.OpenChunkWriter
	}

	cache.PinUntilFinalized(f.Fs, f)
	return f, err
}

//
// Filesystem
//

// Name of the remote (as passed into NewFs)
func (f *Fs) Name() string { return f.name }

// Root of the remote (as passed into NewFs)
func (f *Fs) Root() string { return f.root }

// Features returns the optional features of this Fs
func (f *Fs) Features() *fs.Features { return f.features }

// Hashes returns the supported hash sets.
func (f *Fs) Hashes() rclonehash.Set { return f.suppHashes }

// String returns a description of the FS
// The "hasher::" prefix is a distinctive feature.
func (f *Fs) String() string {
	return fmt.Sprintf("hasher::%s:%s", f.name, f.root)
}

// UnWrap returns the Fs that this Fs is wrapping
func (f *Fs) UnWrap() fs.Fs { return f.Fs }

// WrapFs returns the Fs that is wrapping this Fs
func (f *Fs) WrapFs() fs.Fs { return f.wrapper }

// SetWrapper sets the Fs that is wrapping this Fs
func (f *Fs) SetWrapper(wrapper fs.Fs) { f.wrapper = wrapper }

// Wrap base entries into hasher entries.
func (f *Fs) wrapEntries(baseEntries fs.DirEntries) (hashEntries fs.DirEntries, err error) {
	hashEntries = baseEntries[:0] // work inplace
	for _, entry := range baseEntries {
		switch x := entry.(type) {
		case fs.Object:
			obj, err := f.wrapObject(x, nil)
			if err != nil {
				return nil, err
			}
			hashEntries = append(hashEntries, obj)
		default:
			hashEntries = append(hashEntries, entry) // trash in - trash out
		}
	}
	return hashEntries, nil
}

// List the objects and directories in dir into entries.
func (f *Fs) List(ctx context.Context, dir string) (entries fs.DirEntries, err error) {
	return list.WithListP(ctx, dir, f)
}

// ListP lists the objects and directories of the Fs starting
// from dir non recursively into out.
//
// dir should be "" to start from the root, and should not
// have trailing slashes.
//
// This should return ErrDirNotFound if the directory isn't
// found.
//
// It should call callback for each tranche of entries read.
// These need not be returned in any particular order.  If
// callback returns an error then the listing will stop
// immediately.
func (f *Fs) ListP(ctx context.Context, dir string, callback fs.ListRCallback) error {
	wrappedCallback := func(entries fs.DirEntries) error {
		entries, err := f.wrapEntries(entries)
		if err != nil {
			return err
		}
		return callback(entries)
	}
	listP := f.Fs.Features().ListP
	if listP == nil {
		entries, err := f.Fs.List(ctx, dir)
		if err != nil {
			return err
		}
		return wrappedCallback(entries)
	}
	return listP(ctx, dir, wrappedCallback)
}

// ListR lists the objects and directories recursively into out.
func (f *Fs) ListR(ctx context.Context, dir string, callback fs.ListRCallback) (err error) {
	return f.Fs.Features().ListR(ctx, dir, func(baseEntries fs.DirEntries) error {
		hashEntries, err := f.wrapEntries(baseEntries)
		if err != nil {
			return err
		}
		return callback(hashEntries)
	})
}

// Purge a directory
func (f *Fs) Purge(ctx context.Context, dir string) error {
	if do := f.Fs.Features().Purge; do != nil {
		if err := do(ctx, dir); err != nil {
			return err
		}
		err := f.db.Do(true, &kvPurge{
			dir: path.Join(f.Fs.Root(), dir),
		})
		if err != nil {
			fs.Errorf(f, "Failed to purge some hashes: %v", err)
		}
		return nil
	}
	return fs.ErrorCantPurge
}

// PutStream uploads to the remote path with undeterminate size.
func (f *Fs) PutStream(ctx context.Context, in io.Reader, src fs.ObjectInfo, options ...fs.OpenOption) (fs.Object, error) {
	if do := f.Fs.Features().PutStream; do != nil {
		_ = f.pruneHash(src.Remote())
		oResult, err := do(ctx, in, src, options...)
		return f.wrapObject(oResult, err)
	}
	return nil, errors.New("PutStream not supported")
}

// PutUnchecked uploads the object, allowing duplicates.
func (f *Fs) PutUnchecked(ctx context.Context, in io.Reader, src fs.ObjectInfo, options ...fs.OpenOption) (fs.Object, error) {
	if do := f.Fs.Features().PutUnchecked; do != nil {
		_ = f.pruneHash(src.Remote())
		oResult, err := do(ctx, in, src, options...)
		return f.wrapObject(oResult, err)
	}
	return nil, errors.New("PutUnchecked not supported")
}

// pruneHash deletes hash for a path
func (f *Fs) pruneHash(remote string) error {
	return f.db.Do(true, &kvPrune{
		key: path.Join(f.Fs.Root(), remote),
	})
}

// CleanUp the trash in the Fs
func (f *Fs) CleanUp(ctx context.Context) error {
	if do := f.Fs.Features().CleanUp; do != nil {
		return do(ctx)
	}
	return errors.New("not supported by underlying remote")
}

// About gets quota information from the Fs
func (f *Fs) About(ctx context.Context) (*fs.Usage, error) {
	if do := f.Fs.Features().About; do != nil {
		return do(ctx)
	}
	return nil, errors.New("not supported by underlying remote")
}

// ChangeNotify calls the passed function with a path that has had changes.
func (f *Fs) ChangeNotify(ctx context.Context, notifyFunc func(string, fs.EntryType), pollIntervalChan <-chan time.Duration) {
	if do := f.Fs.Features().ChangeNotify; do != nil {
		do(ctx, notifyFunc, pollIntervalChan)
	}
}

// UserInfo returns info about the connected user
func (f *Fs) UserInfo(ctx context.Context) (map[string]string, error) {
	if do := f.Fs.Features().UserInfo; do != nil {
		return do(ctx)
	}
	return nil, fs.ErrorNotImplemented
}

// Disconnect the current user
func (f *Fs) Disconnect(ctx context.Context) error {
	if do := f.Fs.Features().Disconnect; do != nil {
		return do(ctx)
	}
	return fs.ErrorNotImplemented
}

// MergeDirs merges the contents of all the directories passed
// in into the first one and rmdirs the other directories.
func (f *Fs) MergeDirs(ctx context.Context, dirs []fs.Directory) error {
	if do := f.Fs.Features().MergeDirs; do != nil {
		return do(ctx, dirs)
	}
	return errors.New("MergeDirs not supported")
}

// DirSetModTime sets the directory modtime for dir
func (f *Fs) DirSetModTime(ctx context.Context, dir string, modTime time.Time) error {
	if do := f.Fs.Features().DirSetModTime; do != nil {
		return do(ctx, dir, modTime)
	}
	return fs.ErrorNotImplemented
}

// MkdirMetadata makes the root directory of the Fs object
func (f *Fs) MkdirMetadata(ctx context.Context, dir string, metadata fs.Metadata) (fs.Directory, error) {
	if do := f.Fs.Features().MkdirMetadata; do != nil {
		return do(ctx, dir, metadata)
	}
	return nil, fs.ErrorNotImplemented
}

// DirCacheFlush resets the directory cache - used in testing
// as an optional interface
func (f *Fs) DirCacheFlush() {
	if do := f.Fs.Features().DirCacheFlush; do != nil {
		do()
	}
}

// PublicLink generates a public link to the remote path (usually readable by anyone)
func (f *Fs) PublicLink(ctx context.Context, remote string, expire fs.Duration, unlink bool) (string, error) {
	if do := f.Fs.Features().PublicLink; do != nil {
		return do(ctx, remote, expire, unlink)
	}
	return "", errors.New("PublicLink not supported")
}

// Copy src to this remote using server-side copy operations.
func (f *Fs) Copy(ctx context.Context, src fs.Object, remote string) (fs.Object, error) {
	do := f.Fs.Features().Copy
	if do == nil {
		return nil, fs.ErrorCantCopy
	}
	o, ok := src.(*Object)
	if !ok {
		return nil, fs.ErrorCantCopy
	}
	oResult, err := do(ctx, o.Object, remote)
	return f.wrapObject(oResult, err)
}

// Move src to this remote using server-side move operations.
func (f *Fs) Move(ctx context.Context, src fs.Object, remote string) (fs.Object, error) {
	do := f.Fs.Features().Move
	if do == nil {
		return nil, fs.ErrorCantMove
	}
	o, ok := src.(*Object)
	if !ok {
		return nil, fs.ErrorCantMove
	}
	oResult, err := do(ctx, o.Object, remote)
	if err != nil {
		return nil, err
	}
	_ = f.db.Do(true, &kvMove{
		src: path.Join(f.Fs.Root(), src.Remote()),
		dst: path.Join(f.Fs.Root(), remote),
		dir: false,
		fs:  f,
	})
	return f.wrapObject(oResult, nil)
}

// DirMove moves src, srcRemote to this remote at dstRemote using server-side move operations.
func (f *Fs) DirMove(ctx context.Context, src fs.Fs, srcRemote, dstRemote string) error {
	do := f.Fs.Features().DirMove
	if do == nil {
		return fs.ErrorCantDirMove
	}
	srcFs, ok := src.(*Fs)
	if !ok {
		return fs.ErrorCantDirMove
	}
	err := do(ctx, srcFs.Fs, srcRemote, dstRemote)
	if err == nil {
		_ = f.db.Do(true, &kvMove{
			src: path.Join(srcFs.Fs.Root(), srcRemote),
			dst: path.Join(f.Fs.Root(), dstRemote),
			dir: true,
			fs:  f,
		})
	}
	return err
}

// Shutdown the backend, closing any background tasks and any cached connections.
func (f *Fs) Shutdown(ctx context.Context) (err error) {
	if f.db != nil && !f.db.IsStopped() {
		err = f.db.Stop(false)
	}
	if do := f.Fs.Features().Shutdown; do != nil {
		if err2 := do(ctx); err2 != nil {
			err = err2
		}
	}
	return
}

// NewObject finds the Object at remote.
func (f *Fs) NewObject(ctx context.Context, remote string) (fs.Object, error) {
	o, err := f.Fs.NewObject(ctx, remote)
	return f.wrapObject(o, err)
}

// OpenChunkWriter opens a writer for chunked uploads
func (f *Fs) OpenChunkWriter(ctx context.Context, remote string, src fs.ObjectInfo, options ...fs.OpenOption) (fs.ChunkWriterInfo, fs.ChunkWriter, error) {
	// Check if underlying backend supports chunked writing
	do := f.Fs.Features().OpenChunkWriter
	if do == nil {
		// Check if underlying backend supports OpenWriterAt
		openWriterAt := f.Fs.Features().OpenWriterAt
		if openWriterAt == nil {
			return fs.ChunkWriterInfo{}, nil, fs.ErrorNotImplemented
		}
		// Use standard multithread adapter pattern
		do = f.createOpenWriterAtAdapter(openWriterAt)
	}

	// Get underlying chunk writer
	underlyingInfo, underlyingWriter, err := do(ctx, remote, src, options...)
	if err != nil {
		return fs.ChunkWriterInfo{}, nil, err
	}

	// Calculate number of chunks
	numChunks := int(math.Ceil(float64(src.Size()) / float64(underlyingInfo.ChunkSize)))

	// Get active hash types for this backend
	activeHashTypes := f.getActiveHashTypes()

	// Initialize hash tracking
	hashTracker := newHashOrderTracker(activeHashTypes, numChunks)

	writer := &hasherChunkWriter{
		underlyingWriter: underlyingWriter,
		hashTracker:      hashTracker,
		f:                f,
		remote:           remote,
		chunkSize:        underlyingInfo.ChunkSize,
		totalSize:        src.Size(),
		numChunks:        numChunks,
	}

	// Prune any existing hash for this remote
	_ = f.pruneHash(remote)

	return underlyingInfo, writer, nil
}

// createOpenWriterAtAdapter creates an adapter from OpenWriterAt to OpenChunkWriter
func (f *Fs) createOpenWriterAtAdapter(openWriterAt fs.OpenWriterAtFn) fs.OpenChunkWriterFn {
	return func(ctx context.Context, remote string, src fs.ObjectInfo, options ...fs.OpenOption) (info fs.ChunkWriterInfo, writer fs.ChunkWriter, err error) {
		ci := fs.GetConfig(ctx)

		// Use standard multithread chunk size
		chunkSize := int64(ci.MultiThreadChunkSize)

		// Extract chunk size from options if provided
		for _, option := range options {
			if chunkOption, ok := option.(*fs.ChunkOption); ok {
				chunkSize = chunkOption.ChunkSize
				break
			}
		}

		writerAt, err := openWriterAt(ctx, remote, src.Size())
		if err != nil {
			return info, nil, err
		}

		// Use standard positioning: chunkNumber * chunkSize
		adapter := &standardChunkWriterAdapter{
			remote:    remote,
			size:      src.Size(),
			chunkSize: chunkSize,
			writerAt:  writerAt,
		}

		info = fs.ChunkWriterInfo{
			ChunkSize:   chunkSize,
			Concurrency: ci.MultiThreadStreams,
		}

		return info, adapter, nil
	}
}

// getActiveHashTypes returns the hash types that should be calculated
func (f *Fs) getActiveHashTypes() []rclonehash.Type {
	var activeHashes []rclonehash.Type
	for _, hashType := range f.keepHashes.Array() {
		activeHashes = append(activeHashes, hashType)
	}
	return activeHashes
}

// makeFingerprint creates a fingerprint for the given remote and size
func (f *Fs) makeFingerprint(ctx context.Context, remote string, size int64) string {
	timeStr := "-"
	if f.fpTime {
		// Use current time for new uploads
		timeStr = time.Now().UTC().Format(timeFormat)
	}
	hashStr := "-"
	// For new uploads, we don't have a hash yet, so use placeholder
	if size < 0 {
		return fmt.Sprintf("%d,%s,%s", -1, timeStr, hashStr)
	}
	return fmt.Sprintf("%d,%s,%s", size, timeStr, hashStr)
}

//
// Object
//

// Object represents a composite file wrapping one or more data chunks
type Object struct {
	fs.Object
	f *Fs
}

// Wrap base object into hasher object
func (f *Fs) wrapObject(o fs.Object, err error) (obj fs.Object, outErr error) {
	// log.Trace(o, "err=%v", err)("obj=%#v, outErr=%v", &obj, &outErr)
	if err != nil {
		return nil, err
	}
	if o == nil {
		return nil, fs.ErrorObjectNotFound
	}
	return &Object{Object: o, f: f}, nil
}

// Fs returns read only access to the Fs that this object is part of
func (o *Object) Fs() fs.Info { return o.f }

// UnWrap returns the wrapped Object
func (o *Object) UnWrap() fs.Object { return o.Object }

// Return a string version
func (o *Object) String() string {
	if o == nil {
		return "<nil>"
	}
	return o.Object.String()
}

// ID returns the ID of the Object if possible
func (o *Object) ID() string {
	if doer, ok := o.Object.(fs.IDer); ok {
		return doer.ID()
	}
	return ""
}

// GetTier returns the Tier of the Object if possible
func (o *Object) GetTier() string {
	if doer, ok := o.Object.(fs.GetTierer); ok {
		return doer.GetTier()
	}
	return ""
}

// SetTier set the Tier of the Object if possible
func (o *Object) SetTier(tier string) error {
	if doer, ok := o.Object.(fs.SetTierer); ok {
		return doer.SetTier(tier)
	}
	return errors.New("SetTier not supported")
}

// MimeType of an Object if known, "" otherwise
func (o *Object) MimeType(ctx context.Context) string {
	if doer, ok := o.Object.(fs.MimeTyper); ok {
		return doer.MimeType(ctx)
	}
	return ""
}

// Metadata returns metadata for an object
//
// It should return nil if there is no Metadata
func (o *Object) Metadata(ctx context.Context) (fs.Metadata, error) {
	do, ok := o.Object.(fs.Metadataer)
	if !ok {
		return nil, nil
	}
	return do.Metadata(ctx)
}

// SetMetadata sets metadata for an Object
//
// It should return fs.ErrorNotImplemented if it can't set metadata
func (o *Object) SetMetadata(ctx context.Context, metadata fs.Metadata) error {
	do, ok := o.Object.(fs.SetMetadataer)
	if !ok {
		return fs.ErrorNotImplemented
	}
	return do.SetMetadata(ctx, metadata)
}

// Fd returns the Fd of the Object
//
// It should return fs.ErrorNotImplemented if it's not available
func (o *Object) Fd(ctx context.Context, flags int) (uintptr, error) {
	do, ok := o.Object.(fs.Fder)
	if !ok {
		return 0, fs.ErrorNotImplemented
	}
	return do.Fd(ctx, flags)
}

//
// OpenChunkWriter support
//

// chunkData represents a chunk of data for hash calculation
type chunkData struct {
	number int
	data   []byte
	size   int64
}

// hashState tracks state for a single hash algorithm
type hashState struct {
	hasher       hash.Hash
	algorithm    rclonehash.Type
	bytesHashed  int64
}

// hashOrderTracker manages ordered hash processing across chunks
type hashOrderTracker struct {
	// Hash algorithms
	multiHasher *rclonehash.MultiHasher
	hashTypes   []rclonehash.Type

	// Ordering for deterministic results
	chunkBuffer map[int]*chunkData
	nextChunk   int  // Next chunk number to process
	numChunks   int  // Total number of chunks expected

	// Synchronization
	mu          sync.Mutex
	completed   []bool
	finalized   bool
	finalHashes map[rclonehash.Type]string
}

// newHashOrderTracker creates a new hash order tracker
func newHashOrderTracker(hashTypes []rclonehash.Type, numChunks int) *hashOrderTracker {
	tracker := &hashOrderTracker{
		chunkBuffer: make(map[int]*chunkData),
		completed:   make([]bool, numChunks),
		numChunks:   numChunks,
		hashTypes:   hashTypes,
	}

	// Initialize hash algorithms using a single MultiHasher
	var hashSet rclonehash.Set
	for _, hashType := range hashTypes {
		hashSet.Add(hashType)
	}
	
	multiHasher, err := rclonehash.NewMultiHasherTypes(hashSet)
	if err != nil {
		// If we can't create hashers, just skip them
		return tracker
	}

	tracker.multiHasher = multiHasher
	return tracker
}

// processChunkForHash processes a chunk for hash calculation
func (ht *hashOrderTracker) processChunkForHash(chunk *chunkData) {
	ht.mu.Lock()
	defer ht.mu.Unlock()

	// Mark this chunk as received
	if chunk.number >= 0 && chunk.number < len(ht.completed) {
		ht.completed[chunk.number] = true
	}

	// If this is the next chunk we're waiting for, process immediately
	if chunk.number == ht.nextChunk {
		ht.processChunkData(chunk)
		ht.nextChunk++

		// Process any buffered chunks that are now ready
		ht.processBufferedChunks()
	} else {
		// Buffer this chunk for later processing
		ht.chunkBuffer[chunk.number] = chunk
	}
}

// processChunkData updates hash algorithms with chunk data
func (ht *hashOrderTracker) processChunkData(chunk *chunkData) {
	// Update the MultiHasher with this chunk's data
	if ht.multiHasher != nil {
		ht.multiHasher.Write(chunk.data)
	}
}

// processBufferedChunks processes any buffered chunks that are now ready
func (ht *hashOrderTracker) processBufferedChunks() {
	for {
		chunk, exists := ht.chunkBuffer[ht.nextChunk]
		if !exists {
			break
		}

		ht.processChunkData(chunk)
		delete(ht.chunkBuffer, ht.nextChunk)
		ht.nextChunk++
	}
}

// waitForCompletion waits for all chunks to be processed
func (ht *hashOrderTracker) waitForCompletion(ctx context.Context) error {
	for {
		ht.mu.Lock()
		done := ht.nextChunk >= ht.numChunks
		ht.mu.Unlock()

		if done {
			break
		}

		// Check for context cancellation
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		// Brief wait before checking again
		time.Sleep(10 * time.Millisecond)
	}

	return nil
}

// finalizeHashes computes final hash values
func (ht *hashOrderTracker) finalizeHashes() map[rclonehash.Type]string {
	ht.mu.Lock()
	defer ht.mu.Unlock()

	if ht.finalized {
		return ht.finalHashes
	}

	ht.finalHashes = make(map[rclonehash.Type]string)
	
	if ht.multiHasher != nil {
		// Get hashes from the MultiHasher
		hashSums := ht.multiHasher.Sums()
		for _, hashType := range ht.hashTypes {
			if hashValue, ok := hashSums[hashType]; ok {
				ht.finalHashes[hashType] = hashValue
			}
		}
	}

	ht.finalized = true
	return ht.finalHashes
}

// reset clears all state for reuse or cleanup
func (ht *hashOrderTracker) reset() {
	ht.mu.Lock()
	defer ht.mu.Unlock()

	// Clear all buffers and reset state
	ht.chunkBuffer = make(map[int]*chunkData)
	ht.nextChunk = 0
	ht.finalized = false
	ht.finalHashes = nil

	// Reset the MultiHasher by recreating it
	if len(ht.hashTypes) > 0 {
		var hashSet rclonehash.Set
		for _, hashType := range ht.hashTypes {
			hashSet.Add(hashType)
		}
		
		if multiHasher, err := rclonehash.NewMultiHasherTypes(hashSet); err == nil {
			ht.multiHasher = multiHasher
		}
	}
}

// hasherChunkWriter implements fs.ChunkWriter for the hasher backend
type hasherChunkWriter struct {
	// Storage delegation
	underlyingWriter fs.ChunkWriter

	// Hash calculation (independent from storage)
	hashTracker *hashOrderTracker

	// Configuration
	f         *Fs
	remote    string
	chunkSize int64
	totalSize int64
	numChunks int
}

// WriteChunk writes a chunk of data
func (w *hasherChunkWriter) WriteChunk(ctx context.Context, chunkNumber int, reader io.ReadSeeker) (int64, error) {
	// Validate chunk number
	if chunkNumber < 0 || chunkNumber >= w.numChunks {
		return 0, fmt.Errorf("invalid chunk number: %d", chunkNumber)
	}

	// Read all data from the reader first to capture for hash calculation
	data, err := io.ReadAll(reader)
	if err != nil {
		return 0, fmt.Errorf("failed to read chunk %d data: %w", chunkNumber, err)
	}

	// Create a new reader from the data for the underlying writer
	dataReader := bytes.NewReader(data)

	// 1. Storage happens immediately using standard patterns
	bytesWritten, err := w.underlyingWriter.WriteChunk(ctx, chunkNumber, dataReader)
	if err != nil {
		return 0, fmt.Errorf("underlying writer failed for chunk %d: %w", chunkNumber, err)
	}

	// 2. Hash calculation happens in parallel (non-blocking)
	chunkData := &chunkData{
		number: chunkNumber,
		data:   data,
		size:   int64(len(data)),
	}

	go w.hashTracker.processChunkForHash(chunkData)

	return bytesWritten, nil
}

// Close finalizes the chunk writer and stores calculated hashes
func (w *hasherChunkWriter) Close(ctx context.Context) error {
	// First, close the underlying writer
	if err := w.underlyingWriter.Close(ctx); err != nil {
		return fmt.Errorf("failed to close underlying writer: %w", err)
	}

	// Wait for all hash calculations to complete
	if err := w.hashTracker.waitForCompletion(ctx); err != nil {
		return fmt.Errorf("hash calculation interrupted: %w", err)
	}

	// Finalize hashes and store in hasher backend cache
	finalHashes := w.hashTracker.finalizeHashes()

	// Store hashes in hasher backend's database
	return w.storeHashesInCache(finalHashes)
}

// Abort cancels the chunk writer operation
func (w *hasherChunkWriter) Abort(ctx context.Context) error {
	// First, abort the underlying writer
	if err := w.underlyingWriter.Abort(ctx); err != nil {
		return fmt.Errorf("failed to abort underlying writer: %w", err)
	}

	// Clear hash state
	w.hashTracker.reset()

	return nil
}

// storeHashesInCache stores calculated hashes in the hasher backend's cache
func (w *hasherChunkWriter) storeHashesInCache(hashes map[rclonehash.Type]string) error {
	if w.f.db == nil {
		// No database configured, skip caching
		return nil
	}

	// Convert to hasher backend's expected format (operations.HashSums uses string keys)
	hashSums := make(operations.HashSums)
	for hashType, hashValue := range hashes {
		hashSums[hashType.String()] = hashValue
	}

	// Create fingerprint for the uploaded file
	fp := w.f.makeFingerprint(context.Background(), w.remote, w.totalSize)

	// Store in hasher backend's key-value database using existing kvPut operation
	return w.f.db.Do(true, &kvPut{
		key:    path.Join(w.f.Fs.Root(), w.remote),
		fp:     fp,
		hashes: hashSums,
		age:    time.Duration(w.f.opt.MaxAge),
	})
}

// standardChunkWriterAdapter adapts OpenWriterAt to ChunkWriter interface
type standardChunkWriterAdapter struct {
	remote    string
	size      int64
	chunkSize int64
	writerAt  fs.WriterAtCloser
}

// WriteChunk implements the ChunkWriter interface using OpenWriterAt
func (w *standardChunkWriterAdapter) WriteChunk(ctx context.Context, chunkNumber int, reader io.ReadSeeker) (int64, error) {
	// Standard positioning - chunks can be written in any order
	offset := int64(chunkNumber) * w.chunkSize

	// Use OffsetWriter for positioned writes
	writer := io.NewOffsetWriter(w.writerAt, offset)
	return io.Copy(writer, reader)
}

// Close closes the underlying writer
func (w *standardChunkWriterAdapter) Close(ctx context.Context) error {
	return w.writerAt.Close()
}

// Abort closes the underlying writer (same as Close for WriterAt)
func (w *standardChunkWriterAdapter) Abort(ctx context.Context) error {
	return w.writerAt.Close()
}

// Check the interfaces are satisfied
var (
	_ fs.Fs              = (*Fs)(nil)
	_ fs.Purger          = (*Fs)(nil)
	_ fs.Copier          = (*Fs)(nil)
	_ fs.Mover           = (*Fs)(nil)
	_ fs.DirMover        = (*Fs)(nil)
	_ fs.Commander       = (*Fs)(nil)
	_ fs.PutUncheckeder  = (*Fs)(nil)
	_ fs.PutStreamer     = (*Fs)(nil)
	_ fs.CleanUpper      = (*Fs)(nil)
	_ fs.UnWrapper       = (*Fs)(nil)
	_ fs.ListRer         = (*Fs)(nil)
	_ fs.Abouter         = (*Fs)(nil)
	_ fs.Wrapper         = (*Fs)(nil)
	_ fs.MergeDirser     = (*Fs)(nil)
	_ fs.DirSetModTimer  = (*Fs)(nil)
	_ fs.MkdirMetadataer = (*Fs)(nil)
	_ fs.DirCacheFlusher = (*Fs)(nil)
	_ fs.ChangeNotifier  = (*Fs)(nil)
	_ fs.PublicLinker    = (*Fs)(nil)
	_ fs.UserInfoer      = (*Fs)(nil)
	_ fs.Disconnecter    = (*Fs)(nil)
	_ fs.Shutdowner      = (*Fs)(nil)
	_ fs.FullObject      = (*Object)(nil)
	_ fs.ChunkWriter     = (*hasherChunkWriter)(nil)
	_ fs.ChunkWriter     = (*standardChunkWriterAdapter)(nil)
)
