//go:build linux || (darwin && amd64)

// Package mount2 implements a FUSE mounting system for rclone remotes.
package mount2

import (
	"fmt"
	"os"
	"runtime"
	"sync"
	"time"

	fusefs "github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
	"github.com/rclone/rclone/cmd/mountlib"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/vfs"
)

const maxBackground = 128

func init() {
	mountlib.NewMountCommand("mount2", true, mount)
	mountlib.AddRc("mount2", mount)
}

// mountOptions configures the options from the command line flags
//
// man mount.fuse for more info and note the -o flag for other options
func mountOptions(fsys *FS, f fs.Fs, opt *mountlib.Options) (mountOpts *fuse.MountOptions) {
	// Get the kernel's effective max write size (1 MiB on Linux <6.13, tunable on 6.13+)
	kernelMaxWrite := mountlib.GetEffectiveMaxWrite()

	// Cap MaxWrite to kernel limit
	requestedMaxWrite := int(fsys.opt.MaxWrite)
	effectiveMaxWrite := requestedMaxWrite
	if effectiveMaxWrite > kernelMaxWrite {
		effectiveMaxWrite = kernelMaxWrite
		fs.Infof(f, "MaxWrite %d exceeds kernel limit %d, capping to kernel limit",
			requestedMaxWrite, kernelMaxWrite)
	}

	// Apply minimum readahead enforcement (128 KiB) for consistency with kernel sysfs tuning
	readAheadKiB := mountlib.EnforceMinReadAheadKiB(int(fsys.opt.MaxReadAhead / 1024))
	readAheadBytes := readAheadKiB * 1024

	// Cap MaxReadAhead to effective MaxWrite (larger values can't be used in single requests)
	requestedReadAhead := readAheadBytes
	effectiveReadAhead := readAheadBytes
	if effectiveReadAhead > effectiveMaxWrite {
		effectiveReadAhead = effectiveMaxWrite
		fs.Infof(f, "MaxReadAhead %d exceeds effective MaxWrite %d, capping to MaxWrite",
			requestedReadAhead, effectiveMaxWrite)
	}

	mountOpts = &fuse.MountOptions{
		AllowOther:           fsys.opt.AllowOther,
		FsName:               opt.DeviceName,
		Name:                 "rclone",
		DisableXAttrs:        true,
		Debug:                fsys.opt.DebugFUSE,
		MaxReadAhead:         effectiveReadAhead,
		MaxWrite:             effectiveMaxWrite,
		DisableReadDirPlus:   false,
		IDMappedMount:        opt.AllowIDMap,
		MaxStackDepth:        fsys.opt.MaxStackDepth,
		ExtraCapabilities:    fuse.CAP_ASYNC_DIO,
		SyncRead:             !opt.AsyncRead,
		EnableSymlinkCaching: fsys.VFS.Opt.Links,
		// Lift the kernel async-request cap above go-fuse's default of 12 so
		// high-fanout reads aren't throttled by FUSE backing-dev congestion.
		// The congestion threshold follows the kernel-FUSE convention of
		// three quarters of that cap.
		MaxBackground:       maxBackground,
		CongestionThreshold: maxBackground * 3 / 4,

		// RememberInodes: true,
		// SingleThreaded: true,

		/*
			AllowOther bool

			// Options are the options passed as -o string to fusermount.
			Options []string

			// MaxBackground controls the maximum number of allowed background
			// asynchronous I/O requests.
			//
			// If unset, the default is _DEFAULT_BACKGROUND_TASKS, 12.
			// Concurrency for synchronous I/O is not limited.
			MaxBackground int

			// MaxInflightRequestBytes controls the number of bytes used for
			// request structs and input buffers checked out by go-fuse. This
			// includes buffers used by readers waiting on the kernel and requests
			// being processed concurrently.
			//
			// It also applies to requests that do not expect a reply, such as
			// FORGET and BATCH_FORGET. If unset, it defaults to math.MaxInt. If
			// set smaller than the bytes needed for a single request, one request
			// is still allowed through.
			MaxInflightRequestBytes int

			// NumCloneFDs opens this many additional /dev/fuse file
			// descriptors and binds them to the mount session via
			// FUSE_DEV_IOC_CLONE (Linux >= 4.2). Each active cloned fd has its
			// own kernel queue and reader goroutine tree, which reduces
			// single-fd read contention on many-core machines.
			//
			// Replies are written back through the fd from which the request was
			// read. Session-global operations, such as notifications and
			// RegisterBackingFd/UnregisterBackingFd, use the primary fd.
			//
			// Clone failures are logged and the server continues with the primary
			// fd and any clones that were already opened. On non-Linux, clone
			// attempts return ENOSYS through the stub implementation and the
			// server continues without clones. Defaults to 0 (no cloning).
			//
			// MaxInflightRequestBytes is enforced per active fd. When the limit
			// is at least one request's accounting size, the effective configured
			// ceiling scales with active fds:
			// (1 + active clones) * MaxInflightRequestBytes. If the limit is
			// smaller than one request, each active fd can still admit one
			// request.
			NumCloneFDs int

			// CongestionThreshold is the in-flight async-request count at which
			// the kernel marks the FUSE backing-dev as congested, throttling new
			// submissions. It corresponds to
			// /sys/fs/fuse/connections/<id>/congestion_threshold.
			//
			// If 0, go-fuse falls back to the kernel-FUSE convention of
			// 3/4 * MaxBackground. The value is silently clamped by the kernel
			// to MaxBackground if it is set higher.
			CongestionThreshold int

			// MaxWrite is the max size for read and write requests. If 0, use
			// go-fuse default (currently 128 kiB).
			// This number is internally capped at MAX_KERNEL_WRITE (higher values don't make
			// sense).
			//
			// Non-direct-io reads are mostly served via kernel readahead, which is
			// additionally subject to the MaxReadAhead limit.
			//
			// Implementation notes:
			//
			// There's four values the Linux kernel looks at when deciding the request size:
			// * MaxWrite, passed via InitOut.MaxWrite. Limits the WRITE size.
			// * max_read, passed via a string mount option. Limits the READ size.
			//   go-fuse sets max_read equal to MaxWrite.
			//   You can see the current max_read value in /proc/self/mounts .
			// * MaxPages, passed via InitOut.MaxPages. In Linux 4.20 and later, the value
			//   can go up to 1 MiB and go-fuse calculates the MaxPages value acc.
			//   to MaxWrite, rounding up.
			//   On older kernels, the value is fixed at 128 kiB and the
			//   passed value is ignored. No request can be larger than MaxPages, so
			//   READ and WRITE are effectively capped at MaxPages.
			// * MaxReadAhead, passed via InitOut.MaxReadAhead.
			MaxWrite int

			// MaxReadAhead is the max read ahead size to use. It controls how much data the
			// kernel reads in advance to satisfy future read requests from applications.
			// How much exactly is subject to clever heuristics in the kernel
			// (see https://git.kernel.org/pub/scm/linux/kernel/git/torvalds/linux.git/tree/mm/readahead.c?h=v6.2-rc5#n375
			// if you are brave) and hence also depends on the kernel version.
			//
			// If 0, use kernel default. This number is capped at the kernel maximum
			// (128 kiB on Linux) and cannot be larger than MaxWrite.
			//
			// MaxReadAhead only affects buffered reads (=non-direct-io), but even then, the
			// kernel can and does send larger reads to satisfy read requests from applications
			// (up to MaxWrite or VM_READAHEAD_PAGES=128 kiB, whichever is less).
			MaxReadAhead int

			// IgnoreSecurityLabels, if set, makes security related xattr
			// requests return NO_DATA without passing through the
			// user defined filesystem. You should only set this if you
			// file system implements extended attributes, and you are not
			// interested in security labels.
			IgnoreSecurityLabels bool // ignoring labels should be provided as a fusermount mount option.

			// RememberInodes, if set, makes go-fuse never forget inodes.
			// This may be useful for NFS.
			RememberInodes bool

			// FsName is the name of the filesystem, shown in "df -T"
			// and friends (as the first column, "Filesystem").
			FsName string

			// Name is the "fuse.<name>" suffix, shown in "df -T" and friends
			// (as the second column, "Type")
			Name string

			// SingleThreaded, if set, wraps the file system in a single-threaded
			// locking wrapper.
			SingleThreaded bool

			// DisableXAttrs, if set, returns ENOSYS for Getxattr, Listxattr,
			// Setxattr and Removexattr calls, so the kernel does not issue any
			// Xattr operations at all.
			DisableXAttrs bool

			// Debug, if set, enables verbose debugging information.
			Debug bool

			// Logger, if set, is an alternate log sink for debug statements.
			//
			// To increase signal/noise ratio Go-FUSE uses abbreviations in its debug log
			// output. Here is how to read it:
			//
			// - `iX` means `inode X`;
			// - `gX` means `generation X`;
			// - `tA` and `tE` means timeout for attributes and directory entry correspondingly;
			// - `[<off> +<size>)` means data range from `<off>` inclusive till `<off>+<size>` exclusive;
			// - `Xb` means `X bytes`.
			// - `pX` means the request originated from PID `x`. 0 means the request originated from the kernel.
			//
			// Every line is prefixed with either `rx <unique>` (receive from kernel) or `tx <unique>` (send to kernel)
			//
			// Example debug log output:
			//
			//     rx 2: LOOKUP i1 [".wcfs"] 6b p5874
			//     tx 2:     OK, {i3 g2 tE=1s tA=1s {M040755 SZ=0 L=0 1000:1000 B0*0 i0:3 A 0.000000 M 0.000000 C 0.000000}}
			//     rx 3: LOOKUP i3 ["zurl"] 5b p5874
			//     tx 3:     OK, {i4 g3 tE=1s tA=1s {M0100644 SZ=33 L=1 1000:1000 B0*0 i0:4 A 0.000000 M 0.000000 C 0.000000}}
			//     rx 4: OPEN i4 {O_RDONLY,0x8000} p5874
			//     tx 4:     38=function not implemented, {Fh 0 }
			//     rx 5: READ i4 {Fh 0 [0 +4096)  L 0 RDONLY,0x8000} p5874
			//     tx 5:     OK,  33b data "file:///"...
			//     rx 6: GETATTR i4 {Fh 0} p5874
			//     tx 6:     OK, {tA=1s {M0100644 SZ=33 L=1 1000:1000 B0*0 i0:4 A 0.000000 M 0.000000 C 0.000000}}
			//     rx 7: FLUSH i4 {Fh 0} p5874
			//     tx 7:     OK
			//     rx 8: LOOKUP i1 ["head"] 5b p5874
			//     tx 8:     OK, {i5 g4 tE=1s tA=1s {M040755 SZ=0 L=0 1000:1000 B0*0 i0:5 A 0.000000 M 0.000000 C 0.000000}}
			//     rx 9: LOOKUP i5 ["bigfile"] 8b p5874
			//     tx 9:     OK, {i6 g5 tE=1s tA=1s {M040755 SZ=0 L=0 1000:1000 B0*0 i0:6 A 0.000000 M 0.000000 C 0.000000}}
			//     rx 10: FLUSH i4 {Fh 0} p5874
			//     tx 10:     OK
			//     rx 11: GETATTR i1 {Fh 0} p5874
			//     tx 11:     OK, {tA=1s {M040755 SZ=0 L=1 1000:1000 B0*0 i0:1 A 0.000000 M 0.000000 C 0.000000}}
			Logger *log.Logger

			// EnableLocks, if set, asks the kernel to forward file locks to FUSE
			// When used, you must implement the GetLk/SetLk/SetLkw methods.
			EnableLocks bool

			// EnableSymlinkCaching, if set, makes the kernel cache all Readlink return values.
			// The filesystem must use content notification to force the
			// kernel to issue a new Readlink call.
			EnableSymlinkCaching bool

			// ExplicitDataCacheControl, if set, asks the kernel not to do automatic
			// data cache invalidation. The filesystem is fully responsible for
			// invalidating data cache.
			ExplicitDataCacheControl bool

			// SyncRead disables the CAP_ASYNC_READ capability.  The
			// kernel then only sends one read request per file handle at
			// a time, and orders the requests by offset.  This is useful
			// if reading out of order or concurrently is expensive for
			// (example: Amazon Cloud Drive).
			//
			// If unset, multiple concurrent reads may be issued to
			// service userspace requests and kernel readahead.
			//
			// See the comment to FUSE_CAP_ASYNC_READ in
			// https://github.com/libfuse/libfuse/blob/master/include/fuse_common.h
			// for more details.
			SyncRead bool

			// DirectMount, if set, makes go-fuse first attempt to use syscall.Mount instead of
			// fusermount to mount the filesystem. This will not update /etc/mtab
			// but might be needed if fusermount is not available.
			// Also, Server.Unmount will attempt syscall.Unmount before calling
			// fusermount.
			DirectMount bool

			// DirectMountStrict, if set, is like DirectMount but no fallback to fusermount is
			// performed. If both DirectMount and DirectMountStrict are set,
			// DirectMountStrict wins.
			DirectMountStrict bool

			// DirectMountFlags are the mountflags passed to syscall.Mount. If zero, the
			// default value used by fusermount are used: syscall.MS_NOSUID|syscall.MS_NODEV.
			//
			// If you actually *want* zero flags, pass syscall.MS_MGC_VAL, which is ignored
			// by the kernel. See `man 2 mount` for details about MS_MGC_VAL.
			DirectMountFlags uintptr

			// EnableAcl, if set, enables kernel ACL support.
			//
			// See the comments to FUSE_CAP_POSIX_ACL
			// in https://github.com/libfuse/libfuse/blob/master/include/fuse_common.h
			// for details.
			EnableAcl bool

			// DisableReadDirPlus, if set, disables the ReadDirPlus capability so
			// ReadDir is used instead. Simple directory queries (i.e. 'ls' without
			// '-l') can be faster with ReadDir, as no per-file stat calls are needed.
			DisableReadDirPlus bool

			// DisableSplice, if set, disables splicing from files to the FUSE device.
			DisableSplice bool

			// PanicHandler is called if an FS routine panics. The handler
			// should return a nonzero status. If not set, the default is
			// to print a stack trace and return EIO.
			PanicHandler func(any) Status

			// MaxStackDepth is the maximum stacking depth for passthrough files.
			// If unset, the default is 1.
			MaxStackDepth int

			// IDMappedMount, if set, enables an ID-mapped mount if the Kernel supports
			// it.
			//
			// An ID-mapped mount allows the device to be mounted on the system with the
			// IDs remapped (via mount_setattr, move_mount syscalls) to those of the
			// user on the local system.
			//
			// Enabling this flag automatically sets the "default_permissions" mount
			// option. This is required by FUSE to delegate the UID/GID-based permission
			// checks to the kernel. For requests that create new inodes, FUSE will send
			// the mapped UID/GIDs. For all other requests, FUSE will send "-1".
			IDMappedMount bool

			// DisabledCapabilities is a bitmask, containing capablities
			// (the CAP_* bitmasks) that must be disabled for the entire
			// mount.
			DisabledCapabilities uint64

			// ExtraCapabilities is a bitmask of capabilities which
			// must be enabled in addition to the defaults.
			ExtraCapabilities uint64
		*/

	}
	var opts []string
	if fsys.opt.AllowOther {
		opts = append(opts, "allow_other")
	}
	if fsys.opt.AllowRoot {
		opts = append(opts, "allow_root")
	}
	if fsys.opt.DefaultPermissions {
		opts = append(opts, "default_permissions")
	}
	if fsys.VFS.Opt.ReadOnly {
		opts = append(opts, "ro")
	}
	if fsys.opt.WritebackCache {
		fs.Printf(nil, "FIXME --write-back-cache not supported")
		// FIXME opts = append(opts,fuse.WritebackCache())
	}
	// Some OS X only options
	if runtime.GOOS == "darwin" {
		opts = append(opts,
			// VolumeName sets the volume name shown in Finder.
			fmt.Sprintf("volname=%s", opt.VolumeName),

			// NoAppleXattr makes OSXFUSE disallow extended attributes with the
			// prefix "com.apple.". This disables persistent Finder state and
			// other such information.
			"noapplexattr",

			// NoAppleDouble makes OSXFUSE disallow files with names used by OS X
			// to store extended attributes on file systems that do not support
			// them natively.
			//
			// Such file names are:
			//
			//     ._*
			//     .DS_Store
			"noappledouble",
		)
	}
	mountOpts.Options = opts
	return mountOpts
}

// notifySupport reports which kernel notifications this mount may use.
func notifySupport(ks *fuse.InitIn, links bool) (prune, content bool) {
	prune = ks.SupportsNotify(fuse.NOTIFY_PRUNE)
	content = links &&
		ks.Flags64()&fuse.CAP_CACHE_SYMLINKS != 0 &&
		ks.SupportsNotify(fuse.NOTIFY_INVAL_INODE)
	return prune, content
}

// setNegativeTimeout wires NegativeTimeout into opts when AttrTimeout > 0.
//
// EntryTimeout, AttrTimeout, and NegativeTimeout intentionally alias the
// same opt.AttrTimeout backing storage so they always present a single,
// consistent value to go-fuse. NegativeTimeout is only set when
// AttrTimeout > 0 so --attr-timeout=0 preserves the direct ENOENT wire
// format instead of a zero-TTL OK+NodeId=0 negative-dentry response.
func setNegativeTimeout(opts *fusefs.Options, attrTimeout *fs.Duration) {
	if *attrTimeout > 0 {
		opts.NegativeTimeout = (*time.Duration)(attrTimeout)
	}
}

// mount the file system
//
// The mount point will be ready when this returns.
//
// returns an error, and an error channel for the serve process to
// report an error when fusermount is called.
func mount(VFS *vfs.VFS, mountpoint string, opt *mountlib.Options) (<-chan error, func() error, string, error) {
	f := VFS.Fs()
	if err := mountlib.CheckOverlap(f, mountpoint); err != nil {
		return nil, nil, "", err
	}
	if err := mountlib.CheckAllowNonEmpty(mountpoint, opt); err != nil {
		return nil, nil, "", err
	}
	fs.Debugf(f, "Mounting on %q", mountpoint)

	fsys := NewFS(VFS, opt)
	mountOpts := mountOptions(fsys, f, opt)

	// FIXME fill out
	opts := fusefs.Options{
		MountOptions: *mountOpts,
		EntryTimeout: (*time.Duration)(&opt.AttrTimeout),
		AttrTimeout:  (*time.Duration)(&opt.AttrTimeout),
		GID:          VFS.Opt.GID,
		UID:          VFS.Opt.UID,
	}
	// Wire NegativeTimeout per the AttrTimeout aliasing convention.
	setNegativeTimeout(&opts, &opt.AttrTimeout)

	root, err := fsys.Root()
	if err != nil {
		return nil, nil, "", err
	}
	opts.RootStableAttr = &fusefs.StableAttr{Ino: root.vfsDir.Load().Inode()}

	rawFS := fusefs.NewNodeFS(root, &opts)
	server, err := fuse.NewServer(rawFS, mountpoint, &opts.MountOptions)
	if err != nil {
		return nil, nil, "", err
	}
	prune, content := notifySupport(server.KernelSettings(), VFS.Opt.Links)
	registeredPruner := false
	if prune {
		VFS.SetPruner(fsys, fsys)
		registeredPruner = true
		fs.Infof(f, "NotifyPrune supported: registered VFS pruner")
	} else {
		fs.Infof(f, "NotifyPrune unsupported: VFS pruner not registered")
	}
	registeredContentInvalidator := false
	switch {
	case !VFS.Opt.Links:
	case server.KernelSettings().Flags64()&fuse.CAP_CACHE_SYMLINKS == 0:
		fs.Infof(f, "Symlink caching unsupported: VFS content invalidator not registered")
	case !content:
		fs.Infof(f, "NotifyContent unsupported: VFS content invalidator not registered")
	default:
		VFS.SetContentInvalidator(fsys, fsys)
		registeredContentInvalidator = true
		fs.Infof(f, "Symlink caching supported: registered VFS content invalidator")
	}

	var unregisterOnce sync.Once
	unregister := func() {
		unregisterOnce.Do(func() {
			if registeredPruner {
				VFS.SetPruner(fsys, nil)
			}
			if registeredContentInvalidator {
				VFS.SetContentInvalidator(fsys, nil)
			}
		})
	}

	umount := func() error {
		return server.Unmount()
	}

	// serverSettings := server.KernelSettings()
	// fs.Debugf(f, "Server settings %+v", serverSettings)

	// Serve the mount point in the background returning error to errChan
	errs := make(chan error, 1)
	go func() {
		server.Serve()
		unregister()
		errs <- nil
	}()

	fs.Debugf(f, "Waiting for the mount to start...")
	err = server.WaitMount()
	if err != nil {
		unregister()
		return nil, nil, "", err
	}

	fs.Debugf(f, "Mount started")

	// Log effective FUSE settings for debugging
	fs.Debugf(f, "FUSE: MaxWrite=%d (kernel limit %d) MaxReadAhead=%d",
		mountOpts.MaxWrite, mountlib.GetEffectiveMaxWrite(), mountOpts.MaxReadAhead)

	// Tune kernel readahead to match FUSE setting (Linux only, requires root)
	// This makes --max-read-ahead actually work by setting the sysfs value.
	// Only attempt if running as root to avoid warning spam for non-root users.
	// Use mountOpts.MaxReadAhead (already capped) to ensure FUSE and sysfs match exactly.
	if mountOpts.MaxReadAhead > 0 && os.Getuid() == 0 {
		if err := mountlib.TuneKernelReadAhead(mountpoint, mountOpts.MaxReadAhead/1024); err != nil {
			fs.LogLevelPrintf(fs.LogLevelWarning, nil, "Failed to tune kernel readahead: %v", err)
		}
	}

	return errs, umount, mountpoint, nil
}
