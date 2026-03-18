package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/types"
	goflags "github.com/jessevdk/go-flags"
	"github.com/vbauerster/mpb/v8"
	"github.com/vbauerster/mpb/v8/decor"
	"golang.org/x/sync/errgroup"
)

type Options struct {
	ChunkSize   int    `long:"chunk-size" description:"Max chunk size in bytes (0 = use registry default)" default:"0"`
	Concurrency int    `long:"concurrency" description:"Parallel layer operations" default:"5"`
	GzipLevel int `long:"gzip-level" description:"Gzip compression level 1-9 for layers" default:"9"`
	Insecure    bool   `long:"insecure" description:"Use plain HTTP"`
	Username    string `long:"username" env:"REGISTRY_USERNAME" required:"true" description:"Registry username"`
	Password    string `long:"password" env:"REGISTRY_PASSWORD" required:"true" description:"Registry password"`
	Args        struct {
		Source      string `positional-arg-name:"source" required:"true"`
		Destination string `positional-arg-name:"destination" required:"true"`
	} `positional-args:"yes" required:"yes"`
}

type Destination struct {
	Host       string
	Repository string
	Tag        string
}

var tagPattern = regexp.MustCompile(`^[a-zA-Z0-9_.-]+$`)

func ParseDestination(raw string) (Destination, error) {
	if raw == "" {
		return Destination{}, fmt.Errorf("destination must not be empty")
	}

	ref := raw
	tag := "latest"
	if idx := strings.LastIndex(ref, ":"); idx != -1 {
		candidate := ref[idx+1:]
		if !strings.Contains(candidate, "/") {
			tag = candidate
			ref = ref[:idx]
		}
	}

	if !tagPattern.MatchString(tag) {
		return Destination{}, fmt.Errorf("invalid tag %q: must match [a-zA-Z0-9_.-]+", tag)
	}

	parts := strings.SplitN(ref, "/", 2)
	if len(parts) < 2 {
		return Destination{}, fmt.Errorf("destination must be in host/repository[:tag] format, got %q", raw)
	}

	host := parts[0]
	repository := parts[1]

	if host == "" {
		return Destination{}, fmt.Errorf("destination host must not be empty")
	}
	if repository == "" {
		return Destination{}, fmt.Errorf("destination repository must not be empty")
	}

	return Destination{
		Host:       host,
		Repository: repository,
		Tag:        tag,
	}, nil
}

func run() error {
	var opts Options
	parser := goflags.NewParser(&opts, goflags.Default)
	parser.Usage = "[OPTIONS] <source> <destination>"

	_, err := parser.Parse()
	if err != nil {
		if flagErr, ok := err.(*goflags.Error); ok && flagErr.Type == goflags.ErrHelp {
			return nil
		}
		return err
	}

	if opts.GzipLevel < 1 || opts.GzipLevel > 9 {
		return fmt.Errorf("--gzip-level must be between 1 and 9, got %d", opts.GzipLevel)
	}
	if opts.Concurrency < 1 {
		return fmt.Errorf("--concurrency must be at least 1, got %d", opts.Concurrency)
	}
	if opts.ChunkSize < 0 {
		return fmt.Errorf("--chunk-size must be non-negative, got %d", opts.ChunkSize)
	}

	dest, err := ParseDestination(opts.Args.Destination)
	if err != nil {
		return fmt.Errorf("invalid destination: %w", err)
	}

	proto := "https"
	if opts.Insecure {
		proto = "http"
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	return pushImage(ctx, &opts, dest, proto)
}

func pushImage(ctx context.Context, opts *Options, dest Destination, proto string) error {
	fmt.Printf("Resolving source: %s\n", opts.Args.Source)
	img, err := ResolveSource(ctx, opts.Args.Source)
	if err != nil {
		return fmt.Errorf("resolving source: %w", err)
	}

	img, err = ProcessImage(img, opts.GzipLevel)
	if err != nil {
		return fmt.Errorf("processing image: %w", err)
	}

	return pushImageWithSource(ctx, opts, dest, proto, img)
}

func pushImageWithSource(ctx context.Context, opts *Options, dest Destination, proto string, img v1.Image) error {
	baseURL := fmt.Sprintf("%s://%s/v2/%s", proto, dest.Host, dest.Repository)
	cred := Credentials{Username: opts.Username, Password: opts.Password}
	client := &http.Client{
		Transport: &http.Transport{
			DialContext: (&net.Dialer{
				Timeout: 30 * time.Second,
			}).DialContext,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 5 * time.Minute,
		},
	}

	layers, err := img.Layers()
	if err != nil {
		return fmt.Errorf("getting layers: %w", err)
	}

	progress := mpb.New(mpb.WithWidth(60))

	var (
		mu   sync.Mutex
		seen = make(map[v1.Hash]bool)
	)

	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(opts.Concurrency)

	for _, layer := range layers {
		g.Go(func() error {
			digest, err := layer.Digest()
			if err != nil {
				return fmt.Errorf("getting layer digest: %w", err)
			}
			mu.Lock()
			if seen[digest] {
				mu.Unlock()
				return nil
			}
			seen[digest] = true
			mu.Unlock()
			return pushSingleLayer(gctx, client, baseURL, cred, layer, int64(opts.ChunkSize), progress)
		})
	}

	g.Go(func() error {
		return pushConfigBlob(gctx, client, baseURL, cred, img, progress)
	})

	if err := g.Wait(); err != nil {
		progress.Shutdown()
		return err
	}

	progress.Wait()

	manifest, manifestMediaType, err := buildManifest(img)
	if err != nil {
		return fmt.Errorf("building manifest: %w", err)
	}

	fmt.Printf("Pushing manifest to %s:%s\n", dest.Repository, dest.Tag)
	if err := pushManifest(ctx, client, baseURL, cred, dest.Tag, manifest, manifestMediaType); err != nil {
		return err
	}

	fmt.Println("Push complete")
	return nil
}

func pushSingleLayer(ctx context.Context, client *http.Client, baseURL string, cred Credentials, layer v1.Layer, chunkSize int64, progress *mpb.Progress) error {
	digest, err := layer.Digest()
	if err != nil {
		return fmt.Errorf("getting layer digest: %w", err)
	}

	size, err := layer.Size()
	if err != nil {
		return fmt.Errorf("getting layer size: %w", err)
	}

	shortDigest := digest.Hex
	if len(shortDigest) > 12 {
		shortDigest = shortDigest[:12]
	}

	bar := progress.AddBar(size,
		mpb.PrependDecorators(
			decor.Name(shortDigest+" "),
			decor.CountersKibiByte("% .2f / % .2f"),
		),
		mpb.AppendDecorators(
			decor.EwmaSpeed(decor.SizeB1024(0), "% .2f", 30),
			decor.Name(" "),
			decor.Percentage(),
		),
	)

	rc, err := layer.Compressed()
	if err != nil {
		return fmt.Errorf("reading compressed layer: %w", err)
	}
	defer func() { _ = rc.Close() }()

	return pushLayer(ctx, client, baseURL, cred, digest, rc, size, chunkSize, bar)
}

func pushConfigBlob(ctx context.Context, client *http.Client, baseURL string, cred Credentials, img v1.Image, progress *mpb.Progress) error {
	configRaw, err := img.RawConfigFile()
	if err != nil {
		return fmt.Errorf("getting config: %w", err)
	}

	configDigest, configSize, err := v1.SHA256(bytes.NewReader(configRaw))
	if err != nil {
		return fmt.Errorf("computing config digest: %w", err)
	}

	bar := progress.AddBar(configSize,
		mpb.PrependDecorators(
			decor.Name("config       "),
			decor.CountersKibiByte("% .2f / % .2f"),
		),
		mpb.AppendDecorators(
			decor.Percentage(),
		),
	)

	return pushLayer(ctx, client, baseURL, cred, configDigest, bytes.NewReader(configRaw), configSize, 0, bar)
}

func buildManifest(img v1.Image) (data []byte, mediaType string, _ error) {
	m, err := img.Manifest()
	if err != nil {
		return nil, "", fmt.Errorf("getting manifest: %w", err)
	}

	mc := *m
	mc.MediaType = types.OCIManifestSchema1
	mc.Config.MediaType = types.OCIConfigJSON

	mc.Layers = make([]v1.Descriptor, len(m.Layers))
	copy(mc.Layers, m.Layers)
	for i := range mc.Layers {
		mc.Layers[i].MediaType = dockerToOCIMediaType(mc.Layers[i].MediaType)
	}

	data, err = json.Marshal(mc)
	if err != nil {
		return nil, "", fmt.Errorf("marshaling manifest: %w", err)
	}

	return data, string(types.OCIManifestSchema1), nil
}

var dockerToOCILayerTypes = map[types.MediaType]types.MediaType{
	types.DockerLayer:             types.OCILayer,
	types.DockerUncompressedLayer: types.OCIUncompressedLayer,
	types.DockerForeignLayer:      types.OCIRestrictedLayer,
}

func dockerToOCIMediaType(mt types.MediaType) types.MediaType {
	if oci, ok := dockerToOCILayerTypes[mt]; ok {
		return oci
	}
	return mt
}

func main() {
	if err := run(); err != nil {
		if _, ok := err.(*goflags.Error); !ok {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		}
		os.Exit(1)
	}
}
