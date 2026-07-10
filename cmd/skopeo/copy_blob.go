package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/opencontainers/go-digest"
	"github.com/spf13/cobra"
	"go.podman.io/common/pkg/retry"
	"go.podman.io/image/v5/manifest"
	"go.podman.io/image/v5/pkg/blobinfocache"
	"go.podman.io/image/v5/transports"
	"go.podman.io/image/v5/transports/alltransports"
	"go.podman.io/image/v5/types"
)

type copyBlobOptions struct {
	global              *globalOptions
	deprecatedTLSVerify *deprecatedTLSVerifyOption
	srcImage            *imageOptions
	destImage           *imageDestOptions
	retryOpts           *retry.Options
	size                int64
	mediaType           string
	blobKind            string
}

type copyBlobResult struct {
	Digest      string `json:"digest"`
	Status      string `json:"status"`
	Bytes       int64  `json:"bytes"`
	MediaType   string `json:"media_type,omitempty"`
	BlobKind    string `json:"blob_kind"`
	DurationMS  int64  `json:"duration_ms"`
	Source      string `json:"source"`
	Destination string `json:"destination"`
}

type copyBlobOutcome struct {
	blobInfo types.BlobInfo
	kind     string
	status   string
}

func copyBlobCmd(global *globalOptions) *cobra.Command {
	sharedFlags, sharedOpts := sharedImageFlags()
	deprecatedTLSVerifyFlags, deprecatedTLSVerifyOpt := deprecatedTLSVerifyFlags()
	srcFlags, srcOpts := imageFlags(global, sharedOpts, deprecatedTLSVerifyOpt, "src-", "screds")
	destFlags, destOpts := imageDestFlags(global, sharedOpts, deprecatedTLSVerifyOpt, "dest-", "dcreds")
	retryFlags, retryOpts := retryFlags()
	opts := copyBlobOptions{
		global:              global,
		deprecatedTLSVerify: deprecatedTLSVerifyOpt,
		srcImage:            srcOpts,
		destImage:           destOpts,
		retryOpts:           retryOpts,
		size:                -1,
	}
	cmd := &cobra.Command{
		Use:   "copy-blob [command options] SOURCE-IMAGE DESTINATION-IMAGE DIGEST",
		Short: "Copy a single blob by digest from one image reference to another",
		Long: fmt.Sprintf(`Copy a single blob by digest while preserving streaming registry transfer behavior.

Container image references use the "transport":"details" format.

Supported transports:
%s

The command copies only the requested blob. It does not copy or publish image
manifests, tags, signatures, or other metadata.
`, strings.Join(transports.ListNames(), ", ")),
		RunE:              commandAction(opts.run),
		Example:           `skopeo copy-blob docker://registry.example.com/src/repo:tag docker://registry.example.com/dst/repo:tag sha256:...`,
		ValidArgsFunction: autocompleteImageNames,
	}
	adjustUsage(cmd)
	flags := cmd.Flags()
	flags.AddFlagSet(&sharedFlags)
	flags.AddFlagSet(&deprecatedTLSVerifyFlags)
	flags.AddFlagSet(&srcFlags)
	flags.AddFlagSet(&destFlags)
	flags.AddFlagSet(&retryFlags)
	flags.StringVar(&opts.blobKind, "blob-kind", "auto", "Blob kind: auto, layer, or config")
	flags.Int64Var(&opts.size, "size", -1, "Expected blob size in bytes, or -1 if unknown")
	flags.StringVar(&opts.mediaType, "media-type", "", "Expected blob media type")
	_ = flags.MarkHidden("size")
	_ = flags.MarkHidden("media-type")
	return cmd
}

func (opts *copyBlobOptions) run(args []string, stdout io.Writer) (retErr error) {
	if len(args) != 3 {
		return errorShouldDisplayUsage{errors.New("Exactly three arguments expected")}
	}
	opts.deprecatedTLSVerify.warnIfUsed([]string{"--src-tls-verify", "--dest-tls-verify"})

	sourceName := args[0]
	destinationName := args[1]
	if err := reexecIfNecessaryForImages(sourceName, destinationName); err != nil {
		return err
	}

	d, err := digest.Parse(args[2])
	if err != nil {
		return fmt.Errorf("Invalid blob digest %q: %w", args[2], err)
	}

	destRef, err := alltransports.ParseImageName(destinationName)
	if err != nil {
		return fmt.Errorf("Invalid destination name %s: %v", destinationName, err)
	}
	opts.destImage.warnAboutIneffectiveOptions(destRef.Transport())

	ctx, cancel := opts.global.commandTimeoutContext()
	defer cancel()

	inputInfo := types.BlobInfo{
		Digest:    d,
		Size:      opts.size,
		MediaType: opts.mediaType,
	}
	result := copyBlobResult{
		Digest:      d.String(),
		Source:      sourceName,
		Destination: destinationName,
	}

	start := time.Now()
	var outcome copyBlobOutcome
	if err := retry.IfNecessary(ctx, func() error {
		var err error
		outcome, err = opts.copyBlobOnce(ctx, sourceName, destinationName, inputInfo)
		return err
	}, opts.retryOpts); err != nil {
		return err
	}

	result.Status = outcome.status
	result.Bytes = outcome.blobInfo.Size
	result.MediaType = outcome.blobInfo.MediaType
	result.BlobKind = outcome.kind
	result.DurationMS = time.Since(start).Milliseconds()
	if result.Bytes < 0 {
		result.Bytes = opts.size
	}
	return json.NewEncoder(stdout).Encode(result)
}

func (opts *copyBlobOptions) copyBlobOnce(ctx context.Context, sourceName, destinationName string, inputInfo types.BlobInfo) (copyBlobOutcome, error) {
	srcSys, err := opts.srcImage.newSystemContext()
	if err != nil {
		return copyBlobOutcome{}, err
	}
	cache := blobinfocache.DefaultCache(srcSys)

	srcRef, err := alltransports.ParseImageName(sourceName)
	if err != nil {
		return copyBlobOutcome{}, fmt.Errorf("Invalid source name %s: %v", sourceName, err)
	}
	src, err := srcRef.NewImageSource(ctx, srcSys)
	if err != nil {
		return copyBlobOutcome{}, fmt.Errorf("Error parsing source image name %q: %w", sourceName, err)
	}
	defer src.Close()

	resolvedInfo, isConfig, found, err := opts.resolveBlobInfo(ctx, src, inputInfo)
	if err != nil {
		return copyBlobOutcome{}, err
	}
	inputInfo = resolvedInfo
	kind := "unknown"
	if found {
		kind = "layer"
		if isConfig {
			kind = "config"
		}
	}
	switch opts.blobKind {
	case "", "auto":
	case "config":
		isConfig = true
		kind = "config"
	case "layer":
		isConfig = false
		kind = "layer"
	default:
		return copyBlobOutcome{}, fmt.Errorf("unsupported --blob-kind %q, expected auto, layer, or config", opts.blobKind)
	}

	destRef, err := alltransports.ParseImageName(destinationName)
	if err != nil {
		return copyBlobOutcome{}, fmt.Errorf("Invalid destination name %s: %v", destinationName, err)
	}
	destSys, err := opts.destImage.newSystemContext()
	if err != nil {
		return copyBlobOutcome{}, err
	}
	dest, err := destRef.NewImageDestination(ctx, destSys)
	if err != nil {
		return copyBlobOutcome{}, fmt.Errorf("Error initializing destination %q: %w", destinationName, err)
	}
	defer dest.Close()

	reused, reusedInfo, err := dest.TryReusingBlob(ctx, inputInfo, cache, false)
	if err != nil {
		return copyBlobOutcome{}, fmt.Errorf("Error checking destination blob %q: %w", inputInfo.Digest, err)
	}
	if reused {
		if err := dest.Commit(ctx, nil); err != nil {
			return copyBlobOutcome{}, fmt.Errorf("Error committing reused destination blob %q: %w", inputInfo.Digest, err)
		}
		reusedInfo = mergeBlobInfo(reusedInfo, inputInfo)
		return copyBlobOutcome{blobInfo: reusedInfo, kind: kind, status: "already_exists"}, nil
	}

	reader, sourceSize, err := src.GetBlob(ctx, inputInfo, cache)
	if err != nil {
		return copyBlobOutcome{}, fmt.Errorf("Error reading source blob %q: %w", inputInfo.Digest, err)
	}
	defer reader.Close()

	if inputInfo.Size < 0 {
		inputInfo.Size = sourceSize
	}
	verifier := inputInfo.Digest.Verifier()
	stream := io.TeeReader(reader, verifier)
	copiedInfo, err := dest.PutBlob(ctx, stream, inputInfo, cache, isConfig)
	if err != nil {
		return copyBlobOutcome{}, fmt.Errorf("Error writing destination blob %q: %w", inputInfo.Digest, err)
	}
	if !verifier.Verified() {
		return copyBlobOutcome{}, fmt.Errorf("corrupt blob %q", inputInfo.Digest.String())
	}
	if err := dest.Commit(ctx, nil); err != nil {
		return copyBlobOutcome{}, fmt.Errorf("Error committing destination blob %q: %w", inputInfo.Digest, err)
	}
	copiedInfo.MediaType = inputInfo.MediaType
	return copyBlobOutcome{blobInfo: copiedInfo, kind: kind, status: "copied"}, nil
}

type copyBlobDescriptor struct {
	MediaType string               `json:"mediaType"`
	Digest    digest.Digest        `json:"digest"`
	Size      int64                `json:"size"`
	Manifests []copyBlobDescriptor `json:"manifests"`
}

func (opts *copyBlobOptions) resolveBlobInfo(ctx context.Context, src types.ImageSource, inputInfo types.BlobInfo) (types.BlobInfo, bool, bool, error) {
	manifestBytes, manifestMIMEType, err := src.GetManifest(ctx, nil)
	if err != nil {
		return inputInfo, false, false, nil
	}
	return opts.resolveBlobInfoFromManifest(ctx, src, inputInfo, manifestBytes, manifestMIMEType)
}

func (opts *copyBlobOptions) resolveBlobInfoFromManifest(ctx context.Context, src types.ImageSource, inputInfo types.BlobInfo, manifestBytes []byte, manifestMIMEType string) (types.BlobInfo, bool, bool, error) {
	if manifestMIMEType == "" {
		manifestMIMEType = manifest.GuessMIMEType(manifestBytes)
	}
	if manifest.MIMETypeIsMultiImage(manifestMIMEType) {
		var list copyBlobDescriptor
		if err := json.Unmarshal(manifestBytes, &list); err != nil {
			return inputInfo, false, false, fmt.Errorf("parsing source manifest list: %w", err)
		}
		for _, child := range list.Manifests {
			childManifest, childMIMEType, err := src.GetManifest(ctx, &child.Digest)
			if err != nil {
				return inputInfo, false, false, err
			}
			resolvedInfo, isConfig, found, err := opts.resolveBlobInfoFromManifest(ctx, src, inputInfo, childManifest, childMIMEType)
			if err != nil || found {
				return resolvedInfo, isConfig, found, err
			}
		}
		return inputInfo, false, false, nil
	}

	parsedManifest, err := manifest.FromBlob(manifestBytes, manifestMIMEType)
	if err != nil {
		return inputInfo, false, false, nil
	}
	configInfo := parsedManifest.ConfigInfo()
	if configInfo.Digest == inputInfo.Digest {
		return mergeBlobInfo(inputInfo, configInfo), true, true, nil
	}
	for _, layerInfo := range parsedManifest.LayerInfos() {
		if layerInfo.Digest == inputInfo.Digest {
			return mergeBlobInfo(inputInfo, layerInfo.BlobInfo), false, true, nil
		}
	}
	return inputInfo, false, false, nil
}

func mergeBlobInfo(base, discovered types.BlobInfo) types.BlobInfo {
	if base.Size < 0 && discovered.Size >= 0 {
		base.Size = discovered.Size
	}
	if base.MediaType == "" {
		base.MediaType = discovered.MediaType
	}
	return base
}
