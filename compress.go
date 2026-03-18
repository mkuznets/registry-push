package main

import (
	"bytes"
	"fmt"
	"io"
	"sync"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/types"
	"github.com/klauspost/pgzip"
)

func ProcessImage(img v1.Image, gzipLevel int) (v1.Image, error) {
	layers, err := img.Layers()
	if err != nil {
		return nil, fmt.Errorf("getting image layers: %w", err)
	}

	addenda := make([]mutate.Addendum, 0, len(layers))
	for _, layer := range layers {
		mt, err := layer.MediaType()
		if err != nil {
			return nil, fmt.Errorf("getting layer media type: %w", err)
		}
		addenda = append(addenda, mutate.Addendum{
			Layer:     &recompressedLayer{original: layer, gzipLevel: gzipLevel},
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

	once sync.Once
	data []byte
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

		var buf bytes.Buffer
		w, err := pgzip.NewWriterLevel(&buf, l.gzipLevel)
		if err != nil {
			l.err = fmt.Errorf("creating pgzip writer: %w", err)
			return
		}
		if _, err := io.Copy(w, rc); err != nil {
			l.err = fmt.Errorf("compressing layer: %w", err)
			return
		}
		if err := w.Close(); err != nil {
			l.err = fmt.Errorf("closing pgzip writer: %w", err)
			return
		}

		l.data = buf.Bytes()
		l.hash, _, l.err = v1.SHA256(bytes.NewReader(l.data))
	})
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
	return io.NopCloser(bytes.NewReader(l.data)), nil
}

func (l *recompressedLayer) Uncompressed() (io.ReadCloser, error) {
	return l.original.Uncompressed()
}

func (l *recompressedLayer) Size() (int64, error) {
	l.init()
	if l.err != nil {
		return 0, l.err
	}
	return int64(len(l.data)), nil
}

func (l *recompressedLayer) MediaType() (types.MediaType, error) {
	return l.original.MediaType()
}
