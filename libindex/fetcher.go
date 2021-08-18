package libindex

import (
	"bufio"
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/klauspost/compress/gzip"
	"github.com/klauspost/compress/zstd"
	"github.com/quay/zlog"
	"go.opentelemetry.io/otel/baggage"
	"go.opentelemetry.io/otel/label"
	"golang.org/x/sync/errgroup"
	"golang.org/x/sync/singleflight"

	"github.com/quay/claircore"
)

// FetchArena is a struct that keeps track of all the layers fetched into it,
// and only removes them once all the users have gone away.
//
// Exported for use in cctool. If cctool goes away, this can get unexported.
type FetchArena struct {
	wc *http.Client
	sf *singleflight.Group

	mu sync.Mutex
	// Rc is a map of digest to refcount.
	rc map[string]int

	root string
}

// Init initializes the FetchArena.
//
// This method is provided instead of a constructor function to make embedding
// easier.
func (a *FetchArena) Init(wc *http.Client, root string) {
	a.wc = wc
	a.root = root
	a.sf = &singleflight.Group{}
	a.rc = make(map[string]int)
}

func (a *FetchArena) incRef(digest string) error {
	a.mu.Lock()
	a.rc[digest]++
	a.mu.Unlock()
	return nil
}

func (a *FetchArena) decRef(digest string) (int, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.rc[digest]--
	ct := a.rc[digest]
	if ct == 0 {
		delete(a.rc, digest)
		a.sf.Forget(digest)
		return 0, os.Remove(filepath.Join(a.root, digest))
	}
	return ct, nil
}

func (a *FetchArena) filename(l *claircore.Layer) string {
	digest := l.Hash.String()
	n := filepath.Join(a.root, digest)
	a.mu.Lock()
	a.rc[digest] = 0
	a.mu.Unlock()
	return n
}

// Close removes all files left in the arena.
//
// It's not an error to have active fetchers, but may cause errors to have files
// unlinked underneath their users.
func (a *FetchArena) Close(ctx context.Context) error {
	ctx = baggage.ContextWithValues(ctx,
		label.String("component", "libindex/fetchArena.Close"),
		label.String("arena", a.root))
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.rc) != 0 {
		zlog.Warn(ctx).
			Int("count", len(a.rc)).
			Msg("seem to have active fetchers")
		zlog.Info(ctx).
			Msg("clearing arena")
	}
	var err error
	for d := range a.rc {
		delete(a.rc, d)
		a.sf.Forget(d)
		if e := os.Remove(filepath.Join(a.root, d)); e != nil {
			if err == nil {
				err = e
				continue
			}
			err = fmt.Errorf("%v; %v", err, e)
		}
	}
	if err != nil {
		return err
	}
	return nil
}

// RealizeLayer is the inner function used inside the singleflight.
func (a *FetchArena) realizeLayer(ctx context.Context, l *claircore.Layer) (string, error) {
	ctx = baggage.ContextWithValues(ctx,
		label.String("component", "libindex/fetchArena.realizeLayer"),
		label.String("arena", a.root),
		label.Stringer("layer", l.Hash),
		label.String("uri", l.URI))
	zlog.Debug(ctx).Msg("layer fetch start")

	// Validate the layer input.
	if l.URI == "" {
		return "", fmt.Errorf("empty uri for layer %v", l.Hash)
	}
	url, err := url.ParseRequestURI(l.URI)
	if err != nil {
		return "", fmt.Errorf("failed to parse remote path uri: %v", err)
	}
	if l.Hash.Checksum() == nil {
		return "", fmt.Errorf("digest is empty")
	}
	vh := l.Hash.Hash()
	want := l.Hash.Checksum()

	// Open our target file before hitting the network.
	name := a.filename(l)
	rm := true
	fd, err := os.OpenFile(name, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return "", fmt.Errorf("fetcher: unable to create file: %w", err)
	}
	defer func() {
		if err := fd.Close(); err != nil {
			zlog.Warn(ctx).Err(err).Msg("unable to close layer file")
		}
		if rm {
			if err := os.Remove(name); err != nil {
				zlog.Warn(ctx).Err(err).Msg("unable to remove unsuccessful layer fetch")
			}
		}
	}()
	// It'd be nice to be able to pre-allocate our file on disk, but we can't
	// because of decompression.

	req := &http.Request{
		ProtoMajor: 1,
		ProtoMinor: 1,
		Method:     http.MethodGet,
		URL:        url,
		Header:     l.Headers,
	}
	req = req.WithContext(ctx)
	resp, err := a.wc.Do(req)
	if err != nil {
		return "", fmt.Errorf("fetcher: request failed: %w", err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
	default:
		// Especially for 4xx errors, the response body may indicate what's going
		// on, so include some of it in the error message. Capped at 256 bytes in
		// order to not flood the log.
		bodyStart, err := io.ReadAll(io.LimitReader(resp.Body, 256))
		if err == nil {
			return "", fmt.Errorf("fetcher: unexpected status code: %s (body starts: %q)",
				resp.Status, bodyStart)
		}
		return "", fmt.Errorf("fetcher: unexpected status code: %s", resp.Status)
	}
	tr := io.TeeReader(resp.Body, vh)

	br := bufio.NewReader(tr)
	// Look at the content-type and optionally fix it up.
	ct := resp.Header.Get("content-type")
	zlog.Debug(ctx).
		Str("content-type", ct).
		Msg("reported content-type")
	if ct == "" || ct == "text/plain" || ct == "binary/octet-stream" || ct == "application/octet-stream" {
		zlog.Debug(ctx).
			Str("content-type", ct).
			Msg("guessing compression")
		b, err := br.Peek(4)
		if err != nil {
			return "", err
		}
		switch detectCompression(b) {
		case cmpGzip:
			ct = "application/gzip"
		case cmpZstd:
			ct = "application/zstd"
		case cmpNone:
			ct = "application/x-tar"
		}
		zlog.Debug(ctx).
			Str("format", ct).
			Msg("guessed compression")
	}

	var r io.Reader
	switch {
	case ct == "application/gzip" || ct == "application/vnd.docker.image.rootfs.diff.tar.gzip":
		// Catch the old docker media type.
		fallthrough
	case strings.HasSuffix(ct, ".tar+gzip"):
		g, err := gzip.NewReader(br)
		if err != nil {
			return "", err
		}
		defer g.Close()
		r = g
	case ct == "application/zstd":
		fallthrough
	case strings.HasSuffix(ct, ".tar+zstd"):
		s, err := zstd.NewReader(br)
		if err != nil {
			return "", err
		}
		defer s.Close()
		r = s
	case ct == "application/x-tar":
		fallthrough
	case strings.HasSuffix(ct, ".tar"):
		r = br
	default:
		return "", fmt.Errorf("fetcher: unknown content-type %q", ct)
	}

	buf := bufio.NewWriter(fd)
	n, err := io.Copy(buf, r)
	zlog.Debug(ctx).Int64("size", n).Msg("wrote file")
	if err != nil {
		return "", err
	}
	if err := buf.Flush(); err != nil {
		return "", err
	}
	if got := vh.Sum(nil); !bytes.Equal(got, want) {
		err := fmt.Errorf("fetcher: validation failed: got %q, expected %q",
			hex.EncodeToString(got),
			hex.EncodeToString(want))
		return "", err
	}

	zlog.Debug(ctx).Msg("layer fetch ok")
	rm = false
	return name, nil
}

// Fetcher returns an indexer.Fetcher.
func (a *FetchArena) Fetcher() *FetchProxy {
	return &FetchProxy{a: a}
}

// FetchProxy tracks the files fetched for layers.
//
// This can be unexported if FetchArena gets unexported.
type FetchProxy struct {
	a     *FetchArena
	mu    sync.Mutex
	clean []string
}

// Fetch populates all the layers locally.
func (p *FetchProxy) Fetch(ctx context.Context, ls []*claircore.Layer) error {
	g, ctx := errgroup.WithContext(ctx)
	for _, l := range ls {
		g.Go(p.fetchOne(ctx, l))
	}
	if err := g.Wait(); err != nil {
		return fmt.Errorf("encountered error while fetching a layer: %v", err)
	}
	return nil
}

// FetchOne runs a fetch though the singleflight while waiting on the passed-in
// context.
func (p *FetchProxy) fetchOne(ctx context.Context, l *claircore.Layer) func() error {
	fn := func() (interface{}, error) {
		return p.a.realizeLayer(ctx, l)
	}
	return func() error {
		h := l.Hash.String()
		select {
		case res := <-p.a.sf.DoChan(h, fn):
			if err := res.Err; err != nil {
				return err
			}
			fn := res.Val.(string)
			if err := l.SetLocal(fn); err != nil {
				return err
			}
			if err := p.addRef(h); err != nil {
				return err
			}
		case <-ctx.Done():
			return ctx.Err()
		}
		return nil
	}
}

func (p *FetchProxy) addRef(digest string) error {
	if err := p.a.incRef(digest); err != nil {
		return err
	}
	p.mu.Lock()
	p.clean = append(p.clean, digest)
	p.mu.Unlock()
	return nil
}

// Close marks all the layers' backing files as unused.
//
// This method may actually delete the backing files.
func (p *FetchProxy) Close() error {
	var err error
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, digest := range p.clean {
		_, e := p.a.decRef(digest)
		if e != nil {
			if err == nil {
				err = e
			} else {
				err = fmt.Errorf("%v; %v", err, e)
			}
		}
	}
	if err != nil {
		return err
	}
	return nil
}

type compression int

const (
	cmpGzip compression = iota
	cmpZstd
	cmpNone
)

var cmpHeaders = [...][]byte{
	{0x1F, 0x8B, 0x08},       // cmpGzip
	{0x28, 0xB5, 0x2F, 0xFD}, // cmpZstd
}

func detectCompression(b []byte) compression {
	for c, h := range cmpHeaders {
		if len(b) < len(h) {
			continue
		}
		if bytes.Equal(h, b[:len(h)]) {
			return compression(c)
		}
	}
	return cmpNone
}