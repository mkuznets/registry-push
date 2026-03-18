package main

import (
	"fmt"
	"os"
	"strings"

	goflags "github.com/jessevdk/go-flags"
)

type Options struct {
	ChunkSize   int    `long:"chunk-size" description:"Max chunk size in bytes (0 = use registry default)" default:"0"`
	Concurrency int    `long:"concurrency" description:"Parallel layer operations" default:"5"`
	GzipLevel   int    `long:"gzip-level" description:"Gzip level 1-9, used with --recompress" default:"9"`
	Recompress  bool   `long:"recompress" description:"Re-compress layers with pgzip"`
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

	fmt.Printf("Source:      %s\n", opts.Args.Source)
	fmt.Printf("Destination: %s://%s/%s:%s\n", proto, dest.Host, dest.Repository, dest.Tag)
	fmt.Printf("Chunk size:  %d\n", opts.ChunkSize)
	fmt.Printf("Concurrency: %d\n", opts.Concurrency)
	fmt.Printf("Gzip level:  %d\n", opts.GzipLevel)
	fmt.Printf("Recompress:  %t\n", opts.Recompress)

	return nil
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}
