package local

import (
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/containerd/containerd/content"
	"github.com/containerd/containerd/errdefs"
	"github.com/containerd/containerd/filters"
	"github.com/containerd/containerd/log"
	digest "github.com/opencontainers/go-digest"
	"github.com/pkg/errors"
)

var (
	bufPool = sync.Pool{
		New: func() interface{} {
			return make([]byte, 1<<20)
		},
	}
)

// Store is digest-keyed store for content. All data written into the store is
// stored under a verifiable digest.
//
// Store can generally support multi-reader, single-writer ingest of data,
// including resumable ingest.
type store struct {
	root string
}

func NewStore(root string) (content.Store, error) {
	if err := os.MkdirAll(filepath.Join(root, "ingest"), 0777); err != nil && !os.IsExist(err) {
		return nil, err
	}

	return &store{
		root: root,
	}, nil
}

func (s *store) Info(ctx context.Context, dgst digest.Digest) (content.Info, error) {
	p := s.blobPath(dgst)
	fi, err := os.Stat(p)
	if err != nil {
		if os.IsNotExist(err) {
			err = errors.Wrapf(errdefs.ErrNotFound, "content %v", dgst)
		}

		return content.Info{}, err
	}

	return s.info(dgst, fi), nil
}

func (s *store) info(dgst digest.Digest, fi os.FileInfo) content.Info {
	return content.Info{
		Digest:    dgst,
		Size:      fi.Size(),
		CreatedAt: fi.ModTime(),
		UpdatedAt: fi.ModTime(),
	}
}

// Reader returns an io.ReadCloser for the blob.
func (s *store) Reader(ctx context.Context, dgst digest.Digest) (io.ReadCloser, error) {
	fp, err := os.Open(s.blobPath(dgst))
	if err != nil {
		if os.IsNotExist(err) {
			err = errors.Wrapf(errdefs.ErrNotFound, "content %v", dgst)
		}
		return nil, err
	}

	return fp, nil
}

// ReaderAt returns an io.ReaderAt for the blob.
func (s *store) ReaderAt(ctx context.Context, dgst digest.Digest) (io.ReaderAt, error) {
	return readerAt{f: s.blobPath(dgst)}, nil
}

// Delete removes a blob by its digest.
//
// While this is safe to do concurrently, safe exist-removal logic must hold
// some global lock on the store.
func (cs *store) Delete(ctx context.Context, dgst digest.Digest) error {
	if err := os.RemoveAll(cs.blobPath(dgst)); err != nil {
		if !os.IsNotExist(err) {
			return err
		}

		return errors.Wrapf(errdefs.ErrNotFound, "content %v", dgst)
	}

	return nil
}

func (cs *store) Update(ctx context.Context, info content.Info, fieldpaths ...string) (content.Info, error) {
	// TODO: Support persisting and updating mutable content data
	return content.Info{}, errors.Wrapf(errdefs.ErrFailedPrecondition, "update not supported on immutable content store")
}

func (cs *store) Walk(ctx context.Context, fn content.WalkFunc, filters ...string) error {
	// TODO: Support filters
	root := filepath.Join(cs.root, "blobs")
	var alg digest.Algorithm
	return filepath.Walk(root, func(path string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !fi.IsDir() && !alg.Available() {
			return nil
		}

		// TODO(stevvooe): There are few more cases with subdirs that should be
		// handled in case the layout gets corrupted. This isn't strict enough
		// an may spew bad data.

		if path == root {
			return nil
		}
		if filepath.Dir(path) == root {
			alg = digest.Algorithm(filepath.Base(path))

			if !alg.Available() {
				alg = ""
				return filepath.SkipDir
			}

			// descending into a hash directory
			return nil
		}

		dgst := digest.NewDigestFromHex(alg.String(), filepath.Base(path))
		if err := dgst.Validate(); err != nil {
			// log error but don't report
			log.L.WithError(err).WithField("path", path).Error("invalid digest for blob path")
			// if we see this, it could mean some sort of corruption of the
			// store or extra paths not expected previously.
		}

		return fn(cs.info(dgst, fi))
	})
}

func (s *store) Status(ctx context.Context, ref string) (content.Status, error) {
	return s.status(s.ingestRoot(ref))
}

func (s *store) ListStatuses(ctx context.Context, fs ...string) ([]content.Status, error) {
	fp, err := os.Open(filepath.Join(s.root, "ingest"))
	if err != nil {
		return nil, err
	}

	defer fp.Close()

	fis, err := fp.Readdir(-1)
	if err != nil {
		return nil, err
	}

	filter, err := filters.ParseAll(fs...)
	if err != nil {
		return nil, err
	}

	var active []content.Status
	for _, fi := range fis {
		p := filepath.Join(s.root, "ingest", fi.Name())
		stat, err := s.status(p)
		if err != nil {
			if !os.IsNotExist(err) {
				return nil, err
			}

			// TODO(stevvooe): This is a common error if uploads are being
			// completed while making this listing. Need to consider taking a
			// lock on the whole store to coordinate this aspect.
			//
			// Another option is to cleanup downloads asynchronously and
			// coordinate this method with the cleanup process.
			//
			// For now, we just skip them, as they really don't exist.
			continue
		}

		if filter.Match(adaptStatus(stat)) {
			active = append(active, stat)
		}
	}

	return active, nil
}

// status works like stat above except uses the path to the ingest.
func (s *store) status(ingestPath string) (content.Status, error) {
	dp := filepath.Join(ingestPath, "data")
	fi, err := os.Stat(dp)
	if err != nil {
		return content.Status{}, err
	}

	ref, err := readFileString(filepath.Join(ingestPath, "ref"))
	if err != nil {
		return content.Status{}, err
	}

	return content.Status{
		Ref:       ref,
		Offset:    fi.Size(),
		Total:     s.total(ingestPath),
		UpdatedAt: fi.ModTime(),
		StartedAt: getStartTime(fi),
	}, nil
}

func adaptStatus(status content.Status) filters.Adaptor {
	return filters.AdapterFunc(func(fieldpath []string) (string, bool) {
		if len(fieldpath) == 0 {
			return "", false
		}
		switch fieldpath[0] {
		case "ref":
			return status.Ref, true
		}

		return "", false
	})
}

// total attempts to resolve the total expected size for the write.
func (s *store) total(ingestPath string) int64 {
	totalS, err := readFileString(filepath.Join(ingestPath, "total"))
	if err != nil {
		return 0
	}

	total, err := strconv.ParseInt(totalS, 10, 64)
	if err != nil {
		// represents a corrupted file, should probably remove.
		return 0
	}

	return total
}

// Writer begins or resumes the active writer identified by ref. If the writer
// is already in use, an error is returned. Only one writer may be in use per
// ref at a time.
//
// The argument `ref` is used to uniquely identify a long-lived writer transaction.
func (s *store) Writer(ctx context.Context, ref string, total int64, expected digest.Digest) (content.Writer, error) {
	// TODO(stevvooe): Need to actually store expected here. We have
	// code in the service that shouldn't be dealing with this.
	if expected != "" {
		p := s.blobPath(expected)
		if _, err := os.Stat(p); err == nil {
			return nil, errors.Wrapf(errdefs.ErrAlreadyExists, "content %v", expected)
		}
	}

	path, refp, data := s.ingestPaths(ref)

	if err := tryLock(ref); err != nil {
		return nil, errors.Wrapf(err, "locking ref %v failed", ref)
	}

	var (
		digester  = digest.Canonical.Digester()
		offset    int64
		startedAt time.Time
		updatedAt time.Time
	)

	// ensure that the ingest path has been created.
	if err := os.Mkdir(path, 0755); err != nil {
		if !os.IsExist(err) {
			return nil, err
		}

		status, err := s.status(path)
		if err != nil {
			return nil, errors.Wrap(err, "failed reading status of resume write")
		}

		if ref != status.Ref {
			// NOTE(stevvooe): This is fairly catastrophic. Either we have some
			// layout corruption or a hash collision for the ref key.
			return nil, errors.Wrapf(err, "ref key does not match: %v != %v", ref, status.Ref)
		}

		if total > 0 && status.Total > 0 && total != status.Total {
			return nil, errors.Errorf("provided total differs from status: %v != %v", total, status.Total)
		}

		// slow slow slow!!, send to goroutine or use resumable hashes
		fp, err := os.Open(data)
		if err != nil {
			return nil, err
		}
		defer fp.Close()

		p := bufPool.Get().([]byte)
		defer bufPool.Put(p)

		offset, err = io.CopyBuffer(digester.Hash(), fp, p)
		if err != nil {
			return nil, err
		}

		updatedAt = status.UpdatedAt
		startedAt = status.StartedAt
		total = status.Total
	} else {
		// the ingest is new, we need to setup the target location.
		// write the ref to a file for later use
		if err := ioutil.WriteFile(refp, []byte(ref), 0666); err != nil {
			return nil, err
		}

		if total > 0 {
			if err := ioutil.WriteFile(filepath.Join(path, "total"), []byte(fmt.Sprint(total)), 0666); err != nil {
				return nil, err
			}
		}

		startedAt = time.Now()
		updatedAt = startedAt
	}

	fp, err := os.OpenFile(data, os.O_WRONLY|os.O_CREATE, 0666)
	if err != nil {
		return nil, errors.Wrap(err, "failed to open data file")
	}

	return &writer{
		s:         s,
		fp:        fp,
		ref:       ref,
		path:      path,
		offset:    offset,
		total:     total,
		digester:  digester,
		startedAt: startedAt,
		updatedAt: updatedAt,
	}, nil
}

// Abort an active transaction keyed by ref. If the ingest is active, it will
// be cancelled. Any resources associated with the ingest will be cleaned.
func (s *store) Abort(ctx context.Context, ref string) error {
	root := s.ingestRoot(ref)
	if err := os.RemoveAll(root); err != nil {
		if os.IsNotExist(err) {
			return errors.Wrapf(errdefs.ErrNotFound, "ingest ref %q", ref)
		}

		return err
	}

	return nil
}

func (cs *store) blobPath(dgst digest.Digest) string {
	return filepath.Join(cs.root, "blobs", dgst.Algorithm().String(), dgst.Hex())
}

func (s *store) ingestRoot(ref string) string {
	dgst := digest.FromString(ref)
	return filepath.Join(s.root, "ingest", dgst.Hex())
}

// ingestPaths are returned. The paths are the following:
//
// - root: entire ingest directory
// - ref: name of the starting ref, must be unique
// - data: file where data is written
//
func (s *store) ingestPaths(ref string) (string, string, string) {
	var (
		fp = s.ingestRoot(ref)
		rp = filepath.Join(fp, "ref")
		dp = filepath.Join(fp, "data")
	)

	return fp, rp, dp
}

func readFileString(path string) (string, error) {
	p, err := ioutil.ReadFile(path)
	return string(p), err
}
