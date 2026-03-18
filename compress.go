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
)

func ProcessImage(img v1.Image, gzipLevel int) (v1.Image, error) {
	fmt.Println("Exporting image layers...")
	layers, err := img.Layers()
	if err != nil {
		return nil, fmt.Errorf("getting image layers: %w", err)
	}

	fmt.Printf("Compressing %d layers (gzip level %d)\n", len(layers), gzipLevel)

	addenda := make([]mutate.Addendum, 0, len(layers))
	for i, layer := range layers {
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
		// Eagerly compress so we can show progress per layer.
		fmt.Printf("  [%d/%d] %s: compressing...", i+1, len(layers), shortID)
		rl.init()
		if rl.err != nil {
			fmt.Println(" error")
			return nil, fmt.Errorf("compressing layer %s: %w", shortID, rl.err)
		}
		fmt.Printf(" %s\n", formatBytes(rl.size))

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

func formatBytes(b int64) string {
	switch {
	case b >= 1<<30:
		return fmt.Sprintf("%.2f GiB", float64(b)/float64(1<<30))
	case b >= 1<<20:
		return fmt.Sprintf("%.2f MiB", float64(b)/float64(1<<20))
	case b >= 1<<10:
		return fmt.Sprintf("%.2f KiB", float64(b)/float64(1<<10))
	default:
		return fmt.Sprintf("%d B", b)
	}
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

func (l *recompressedLayer) init() {
	l.once.Do(func() {
		rc, err := l.original.Uncompressed()
		if err != nil {
			l.err = fmt.Errorf("reading uncompressed layer: %w", err)
			return
		}
		defer func() { _ = rc.Close() }()

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
		if _, err := io.Copy(w, rc); err != nil {
			_ = f.Close()
			l.err = fmt.Errorf("compressing layer: %w", err)
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
	l.init()
	return l.hash, l.err
}

func (l *recompressedLayer) DiffID() (v1.Hash, error) {
	return l.original.DiffID()
}

func (l *recompressedLayer) Compressed() (io.ReadCloser, error) {
	l.init()
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
	l.init()
	if l.err != nil {
		return 0, l.err
	}
	return l.size, nil
}

func (l *recompressedLayer) MediaType() (types.MediaType, error) {
	return l.original.MediaType()
}
