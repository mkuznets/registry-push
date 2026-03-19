package main

import (
	"fmt"
	"io"
	"os"
	"sync"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/types"
	"github.com/klauspost/pgzip"
	"github.com/vbauerster/mpb/v8"
	"github.com/vbauerster/mpb/v8/decor"
)

func ProcessImage(img v1.Image, gzipLevel int, progress *mpb.Progress) (v1.Image, error) {
	fmt.Println("Exporting image layers...")
	layers, err := img.Layers()
	if err != nil {
		return nil, fmt.Errorf("getting image layers: %w", err)
	}

	fmt.Printf("Compressing %d layers (gzip level %d)\n", len(layers), gzipLevel)

	addenda := make([]mutate.Addendum, 0, len(layers))
	for _, layer := range layers {
		diffID, err := layer.DiffID()
		if err != nil {
			return nil, fmt.Errorf("getting layer diff id: %w", err)
		}
		shortID := diffID.Hex
		if len(shortID) > 12 {
			shortID = shortID[:12]
		}

		mt, err := layer.MediaType()
		if err != nil {
			return nil, fmt.Errorf("getting layer media type: %w", err)
		}

		rl := &recompressedLayer{original: layer, gzipLevel: gzipLevel, label: shortID}
		rl.compress(progress)
		if rl.err != nil {
			return nil, fmt.Errorf("compressing layer %s: %w", shortID, rl.err)
		}

		addenda = append(addenda, mutate.Addendum{
			Layer:     rl,
			MediaType: mt,
		})
	}

	result, err := mutate.Append(empty.Image, addenda...)
	if err != nil {
		return nil, fmt.Errorf("building recompressed image: %w", err)
	}

	cf, err := img.ConfigFile()
	if err != nil {
		return nil, fmt.Errorf("reading image config: %w", err)
	}

	result, err = mutate.ConfigFile(result, cf)
	if err != nil {
		return nil, fmt.Errorf("setting image config: %w", err)
	}

	return result, nil
}

type recompressedLayer struct {
	original  v1.Layer
	gzipLevel int
	label     string

	once sync.Once
	file *os.File
	size int64
	hash v1.Hash
	err  error
}

func (l *recompressedLayer) compress(progress *mpb.Progress) {
	l.once.Do(func() {
		rc, err := l.original.Uncompressed()
		if err != nil {
			l.err = fmt.Errorf("reading uncompressed layer: %w", err)
			return
		}
		defer func() { _ = rc.Close() }()

		var bar *mpb.Bar
		if progress != nil {
			// The uncompressed size is not available from v1.Layer,
			// so we start with total=0 and set the final total after
			// all bytes have been read.
			bar = progress.AddBar(0,
				mpb.PrependDecorators(
					decor.Name(l.label+" "),
					decor.CurrentKibiByte("% .2f"),
				),
				mpb.AppendDecorators(
					decor.EwmaSpeed(decor.SizeB1024(0), "% .2f", 30),
				),
			)
		}

		f, err := os.CreateTemp("", "layer-*.gz")
		if err != nil {
			l.err = fmt.Errorf("creating temp file: %w", err)
			return
		}
		// Remove from filesystem immediately; file handle keeps it alive.
		_ = os.Remove(f.Name())

		w, err := pgzip.NewWriterLevel(f, l.gzipLevel)
		if err != nil {
			_ = f.Close()
			l.err = fmt.Errorf("creating pgzip writer: %w", err)
			return
		}

		var src io.Reader = rc
		if bar != nil {
			src = bar.ProxyReader(rc)
		}

		n, copyErr := io.Copy(w, src)
		if bar != nil {
			bar.SetTotal(n, true)
		}
		if copyErr != nil {
			_ = f.Close()
			l.err = fmt.Errorf("compressing layer: %w", copyErr)
			return
		}
		if err := w.Close(); err != nil {
			_ = f.Close()
			l.err = fmt.Errorf("closing pgzip writer: %w", err)
			return
		}

		l.size, err = f.Seek(0, io.SeekEnd)
		if err != nil {
			_ = f.Close()
			l.err = fmt.Errorf("getting file size: %w", err)
			return
		}
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			_ = f.Close()
			l.err = fmt.Errorf("seeking to start: %w", err)
			return
		}

		l.hash, _, l.err = v1.SHA256(f)
		if l.err != nil {
			_ = f.Close()
			return
		}

		l.file = f
	})
}

func (l *recompressedLayer) Close() error {
	if l.file != nil {
		return l.file.Close()
	}
	return nil
}

func (l *recompressedLayer) Digest() (v1.Hash, error) {
	l.compress(nil)
	return l.hash, l.err
}

func (l *recompressedLayer) DiffID() (v1.Hash, error) {
	return l.original.DiffID()
}

func (l *recompressedLayer) Compressed() (io.ReadCloser, error) {
	l.compress(nil)
	if l.err != nil {
		return nil, l.err
	}
	if _, err := l.file.Seek(0, io.SeekStart); err != nil {
		return nil, fmt.Errorf("seeking layer file: %w", err)
	}
	return io.NopCloser(l.file), nil
}

func (l *recompressedLayer) Uncompressed() (io.ReadCloser, error) {
	return l.original.Uncompressed()
}

func (l *recompressedLayer) Size() (int64, error) {
	l.compress(nil)
	if l.err != nil {
		return 0, l.err
	}
	return l.size, nil
}

func (l *recompressedLayer) MediaType() (types.MediaType, error) {
	return l.original.MediaType()
}
