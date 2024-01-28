// Package autopool provides the ability to modify objects in a Pool so that they are
// automatically added back to the pool on GC. This allows submitting to the pool objects that
// leave your code's control, such as a protocol buffer that is gRPC will encode to the caller.
// It should be noted that this package has no control over when the GC frees the object (if ever),
// so you should explicitly call .Put() when you can.  This will not stop GC by the sync.Pool itself.
// Most of the logic is taken from https://github.com/johnsiilver/golib/tree/master/development/autopool
package autopool

import (
	"runtime"
	"sync"
	"sync/atomic"
)

/**
References:
	* http://www.golangdevops.com/2019/12/31/autopool/
	* https://words.filippo.io/dispatches/certificate-interning/
	* https://go-review.googlesource.com/c/go/+/426454
	* https://go.dev/play/p/ATYDjKZ22mt
*/

type stat struct {
	total, misses, returns uint64
}

func (s *stat) miss() {
	atomic.AddUint64(&s.misses, 1)
}

func (s *stat) get() {
	atomic.AddUint64(&s.total, 1)
}

func (s *stat) put() {
	atomic.AddUint64(&s.returns, 1)
}

// Pool provides a sync.Pool where GC'd objects are put back into the Pool automatically.
type Pool[object any] struct {
	pool          *sync.Pool
	stats         *stat
	resetFunc     func(object)
	finalizerFunc func(object)
}

// New modifies the existing *sync.Pool to return objects that attempt to return
// to the Pool when the GC is prepared to free them.  Only safe to use before the
// Pool is used.
func New[object any](newFunc func() object, resetFunc func(object)) *Pool[object] {
	p := &Pool[object]{resetFunc: resetFunc}
	p.stats = new(stat)
	p.pool = &sync.Pool{
		New: func() any {
			p.stats.miss()
			return newFunc()
		},
	}
	p.finalizerFunc = func(x object) {
		p.resetFunc(x)
		p.Put(x)
	}
	return p
}

// Get works the same as sync.Pool.Get() with the exception that a finalizer is set
// on the object to return the item to the Pool when it is GC'd. Note: objects passed
// that depend on finalizers should not be used here, as they are cleared at certain
// points in the objects lifetime by this package.
func (p *Pool[object]) Get() object {
	p.stats.get()
	x := p.pool.Get().(object)
	runtime.SetFinalizer(x, p.finalizerFunc)
	return x
}

// Put adds x to the pool.
func (p *Pool[object]) Put(x object) {
	p.stats.put()
	runtime.SetFinalizer(x, nil)
	p.pool.Put(x)
}

// Stats returns statistics on the Pool.
func (p *Pool[object]) Stats() (total, misses, returns uint64) {
	return p.stats.total, p.stats.misses, p.stats.returns
}
