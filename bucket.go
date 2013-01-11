package cbgb

import (
	"errors"
	"fmt"
	"io/ioutil"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"unsafe"

	"github.com/steveyen/gkvlite"
)

const (
	MAX_VBUCKET         = 1024
	BUCKET_DIR_SUFFIX   = "-bucket" // Suffix allows non-buckets to be ignored.
	DEFAULT_BUCKET_NAME = "default"
	STORES_PER_BUCKET   = 4
)

type bucket interface {
	Available() bool
	Dir() string
	Close() error
	Load() error

	Observer() *broadcaster
	Subscribe(ch chan<- interface{})
	Unsubscribe(ch chan<- interface{})

	CreateVBucket(vbid uint16) *vbucket
	destroyVBucket(vbid uint16) (destroyed bool)
	getVBucket(vbid uint16) *vbucket
	SetVBState(vbid uint16, newState VBState) *vbucket
}

// Holder of buckets.
type Buckets struct {
	buckets map[string]bucket
	dir     string // Directory where all buckets are stored.
	lock    sync.Mutex
}

// Build a new holder of buckets.
func NewBuckets(dirForBuckets string) (*Buckets, error) {
	if !isDir(dirForBuckets) {
		return nil, errors.New(fmt.Sprintf("not a directory: %v", dirForBuckets))
	}
	return &Buckets{buckets: map[string]bucket{}, dir: dirForBuckets}, nil
}

// Create a new named bucket.
// Return the new bucket, or nil if the bucket already exists.
func (b *Buckets) New(name string) (bucket, error) {
	b.lock.Lock()
	defer b.lock.Unlock()

	if b.buckets[name] != nil {
		return nil, errors.New(fmt.Sprintf("bucket already exists: %v", name))
	}

	// TODO: Need name checking & encoding for safety/security.
	bdir := b.dir + string(os.PathSeparator) + name + BUCKET_DIR_SUFFIX
	if err := os.Mkdir(bdir, 0777); err != nil {
		return nil, err
	}
	if !isDir(bdir) {
		return nil, errors.New(fmt.Sprintf("could not access bucket dir: %v", bdir))
	}

	rv, err := NewBucket(bdir)
	if err != nil {
		return nil, err
	}

	b.buckets[name] = rv
	return rv, nil
}

// Get the named bucket (or nil if it doesn't exist).
func (b *Buckets) Get(name string) bucket {
	b.lock.Lock()
	defer b.lock.Unlock()

	return b.buckets[name]
}

// Destroy the named bucket.
func (b *Buckets) Destroy(name string) {
	b.lock.Lock()
	defer b.lock.Unlock()

	bucket := b.buckets[name]
	if bucket != nil {
		bucket.Close()
		delete(b.buckets, name)
		os.RemoveAll(bucket.Dir())
	}
}

// Reads the buckets directory and returns list of bucket names.
func (b *Buckets) LoadNames() ([]string, error) {
	list, err := ioutil.ReadDir(b.dir)
	if err == nil {
		res := make([]string, 0, len(list))
		for _, entry := range list {
			if entry.IsDir() &&
				strings.HasSuffix(entry.Name(), BUCKET_DIR_SUFFIX) {
				res = append(res,
					entry.Name()[0:len(entry.Name())-len(BUCKET_DIR_SUFFIX)])
			}
		}
		return res, nil
	}
	return nil, err
}

// Loads all buckets from the buckets directory.
func (b *Buckets) Load() error {
	bucketNames, err := b.LoadNames()
	if err != nil {
		return err
	}
	for _, bucketName := range bucketNames {
		b, err := b.New(bucketName)
		if err != nil {
			return err
		}
		if b == nil {
			return errors.New(fmt.Sprintf("loading bucket %v, but it exists already",
				bucketName))
		}
		if err = b.Load(); err != nil {
			return err
		}
	}
	return nil
}

type bucketstorereq struct {
	cb  func(*bucketstore) error
	res chan error
}

type bucketstore struct {
	ident int
	dir   string
	file  *os.File // May be nil for in-memory only (e.g., for unit tests).
	store *gkvlite.Store
	ch    chan bucketstorereq
}

func (bs *bucketstore) service() {
	for r := range bs.ch {
		err := r.cb(bs)
		if r.res != nil {
			r.res <- err
			close(r.res)
		}
	}
	if bs.file != nil {
		bs.file.Close()
	}
}

type livebucket struct {
	vbuckets     [MAX_VBUCKET]unsafe.Pointer
	availablech  chan bool
	observer     *broadcaster
	dir          string
	bucketstores map[int]*bucketstore
}

func NewBucket(dirForBucket string) (bucket, error) {
	res := &livebucket{
		dir:          dirForBucket,
		observer:     newBroadcaster(0),
		availablech:  make(chan bool),
		bucketstores: make(map[int]*bucketstore),
	}
	for i := 0; i < STORES_PER_BUCKET; i++ {
		path := fmt.Sprintf("%s%c%v.store", dirForBucket, os.PathSeparator, i)
		file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0666)
		if err != nil {
			res.Close()
			return nil, err
		}
		store, err := gkvlite.NewStore(file)
		if err != nil {
			file.Close()
			res.Close()
			return nil, err
		}
		res.bucketstores[i] = &bucketstore{
			ident: i,
			dir:   dirForBucket,
			file:  file,
			store: store,
			ch:    make(chan bucketstorereq),
		}
		go res.bucketstores[i].service()
	}
	return res, nil
}

func (b *livebucket) Observer() *broadcaster {
	return b.observer
}

// Subscribe to bucket events.
//
// Note that this is retroactive -- it will send existing states.
func (b *livebucket) Subscribe(ch chan<- interface{}) {
	b.observer.Register(ch)
	go func() {
		for i := uint16(0); i < MAX_VBUCKET; i++ {
			c := vbucketChange{bucket: b,
				vbid:     i,
				oldState: VBDead,
				newState: VBDead}
			vb := c.getVBucket()
			if vb != nil {
				s := vb.GetVBState()
				if s != VBDead {
					c.newState = s
					ch <- c
				}
			}
		}
	}()
}

func (b *livebucket) Unsubscribe(ch chan<- interface{}) {
	b.observer.Unregister(ch)
}

func (b *livebucket) Available() bool {
	select {
	default:
	case <-b.availablech:
		return false
	}
	return true
}

func (b *livebucket) Close() error {
	close(b.availablech)
	for _, bs := range b.bucketstores {
		close(bs.ch)
	}
	return nil
}

func (b *livebucket) Dir() string {
	return b.dir
}

func (b *livebucket) Load() error {
	return nil // TODO: need to do a real load here.
}

func (b *livebucket) getVBucket(vbid uint16) *vbucket {
	if b == nil || !b.Available() {
		return nil
	}
	vbp := atomic.LoadPointer(&b.vbuckets[vbid])
	return (*vbucket)(vbp)
}

func (b *livebucket) casVBucket(vbid uint16, vb *vbucket, vbPrev *vbucket) bool {
	return atomic.CompareAndSwapPointer(&b.vbuckets[vbid],
		unsafe.Pointer(vbPrev), unsafe.Pointer(vb))
}

func (b *livebucket) CreateVBucket(vbid uint16) *vbucket {
	if b == nil || !b.Available() {
		return nil
	}
	vb, err := newVBucket(b, vbid, b.dir)
	if err != nil {
		return nil // TODO: Error propagation / logging.
	}
	if b.casVBucket(vbid, vb, nil) {
		return vb
	}
	return nil
}

func (b *livebucket) destroyVBucket(vbid uint16) (destroyed bool) {
	destroyed = false
	vb := b.getVBucket(vbid)
	if vb != nil {
		vb.SetVBState(VBDead, func(oldState VBState) {
			if b.casVBucket(vbid, nil, vb) {
				b.observer.Submit(vbucketChange{b, vbid, oldState, VBDead})
				destroyed = true
			}
		})
	}
	return
}

func (b *livebucket) SetVBState(vbid uint16, newState VBState) *vbucket {
	vb := b.getVBucket(vbid)
	if vb != nil {
		vb.SetVBState(newState, func(oldState VBState) {
			if b.getVBucket(vbid) == vb {
				b.observer.Submit(vbucketChange{b, vbid, oldState, newState})
			} else {
				vb = nil
			}
		})
	}
	return vb
}

type vbucketChange struct {
	bucket             bucket
	vbid               uint16
	oldState, newState VBState
}

func (c vbucketChange) getVBucket() *vbucket {
	if c.bucket == nil {
		return nil
	}
	return c.bucket.getVBucket(c.vbid)
}

func (c vbucketChange) String() string {
	return fmt.Sprintf("vbucket %v %v -> %v",
		c.vbid, c.oldState, c.newState)
}
