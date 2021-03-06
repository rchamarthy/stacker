package stacker

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path"
	"strings"

	"github.com/anuvu/stacker/lib"
	"github.com/openSUSE/umoci"
	"github.com/openSUSE/umoci/oci/casext"
	"github.com/opencontainers/go-digest"
	ispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"
)

const (
	MediaTypeLayerSquashfs = "application/vnd.oci.image.layer.squashfs"
)

type BaseLayerOpts struct {
	Config    StackerConfig
	Name      string
	Target    string
	Layer     *Layer
	Cache     *BuildCache
	OCI       casext.Engine
	LayerType string
	Debug     bool
}

func GetBaseLayer(o BaseLayerOpts, sf *Stackerfile) error {
	// delete the tag if it exists
	o.OCI.DeleteReference(context.Background(), o.Name)

	switch o.Layer.From.Type {
	case BuiltType:
		return getBuilt(o, sf)
	case TarType:
		return getTar(o)
	case OCIType:
		return getOCI(o)
	case DockerType:
		return getDocker(o)
	case ScratchType:
		return getScratch(o)
	default:
		return fmt.Errorf("unknown layer type: %v", o.Layer.From.Type)
	}
}

func tagFromSkopeoUrl(thing string) (string, error) {
	if strings.HasPrefix(thing, "docker") {
		url, err := url.Parse(thing)
		if err != nil {
			return "", err
		}

		if url.Path != "" {
			return path.Base(strings.Split(url.Path, ":")[0]), nil
		}

		// skopeo allows docker://centos:latest or
		// docker://docker.io/centos:latest; if we don't have a
		// url path, let's use the host as the image tag
		return strings.Split(url.Host, ":")[0], nil
	} else if strings.HasPrefix(thing, "oci") {
		pieces := strings.Split(thing, ":")
		if len(pieces) != 3 {
			return "", fmt.Errorf("bad OCI tag: %s", thing)
		}

		return pieces[2], nil
	} else {
		return "", fmt.Errorf("invalid image url: %s", thing)
	}
}

func runSkopeo(toImport string, o BaseLayerOpts, copyToOutput bool) error {
	tag, err := tagFromSkopeoUrl(toImport)
	if err != nil {
		return err
	}

	// Note that we can do tihs over the top of the cache every time, since
	// skopeo should be smart enough to only copy layers that have changed.
	cacheDir := path.Join(o.Config.StackerDir, "layer-bases", "oci")
	if err := os.MkdirAll(cacheDir, 0755); err != nil {
		return err
	}

	defer func() {
		oci, err := umoci.OpenLayout(cacheDir)
		if err != nil {
			// Some error might have occurred, in which case we
			// don't have a valid OCI layout, which is fine.
			return
		}
		defer oci.Close()

		oci.GC(context.Background())
	}()

	err = lib.ImageCopy(lib.ImageCopyOpts{
		Src:      toImport,
		Dest:     fmt.Sprintf("oci:%s:%s", cacheDir, tag),
		SkipTLS:  o.Layer.From.Insecure,
		Progress: os.Stdout,
	})
	if err != nil {
		return err
	}

	if !copyToOutput {
		return nil
	}

	// If the layer type is something besides tar, we'll generate the
	// base layer after it's extracted from the input image.
	if o.LayerType == "tar" {
		// We just copied it to the cache, now let's copy that over to our image.
		err = lib.ImageCopy(lib.ImageCopyOpts{
			Src:  fmt.Sprintf("oci:%s:%s", cacheDir, tag),
			Dest: fmt.Sprintf("oci:%s:%s", o.Config.OCIDir, tag),
		})
	}

	return err
}

func extractOutput(o BaseLayerOpts) error {
	tag, err := o.Layer.From.ParseTag()
	if err != nil {
		return err
	}

	target := path.Join(o.Config.RootFSDir, o.Target)
	fmt.Println("unpacking to", target)

	// This is a bit of a hack; since we want to unpack from the
	// layer-bases import folder instead of the actual oci dir, we hack
	// this to make config.OCIDir be our input folder. That's a lie, but it
	// seems better to do a little lie here than to try and abstract it out
	// and make everyone else deal with it.
	modifiedConfig := o.Config
	modifiedConfig.OCIDir = path.Join(o.Config.StackerDir, "layer-bases", "oci")
	err = RunUmociSubcommand(modifiedConfig, o.Debug, []string{
		"--bundle-path", target,
		"--tag", tag,
		"unpack",
	})
	if err != nil {
		return err
	}

	// Delete the tag for the base layer; we're only interested in our
	// build layer outputs, not in the base layers.
	o.OCI.DeleteReference(context.Background(), tag)

	// Now, if the layer type is something besides tar, we need to
	// generate the base layer as whatever type that is. Note that we don't
	// need to this for build only layers, as those are not added to the
	// output.
	if o.LayerType == "squashfs" && !o.Layer.BuildOnly {
		o.OCI.GC(context.Background())

		tmpSquashfs, err := mkSquashfs(o.Config, "")
		if err != nil {
			return err
		}

		layerDigest, layerSize, err := o.OCI.PutBlob(context.Background(), tmpSquashfs)
		if err != nil {
			return err
		}

		cacheDir := path.Join(o.Config.StackerDir, "layer-bases", "oci")
		cache, err := umoci.OpenLayout(cacheDir)
		if err != nil {
			return errors.Wrapf(err, "couldn't open base layer dir")
		}
		defer cache.Close()

		cacheTag, err := tagFromSkopeoUrl(o.Layer.From.Url)
		manifest, err := LookupManifest(cache, cacheTag)
		if err != nil {
			return err
		}

		config, err := LookupConfig(cache, manifest.Config)
		if err != nil {
			return err
		}

		desc := ispec.Descriptor{
			MediaType: MediaTypeLayerSquashfs,
			Digest:    layerDigest,
			Size:      layerSize,
		}

		manifest.Layers = []ispec.Descriptor{desc}
		config.RootFS.DiffIDs = []digest.Digest{layerDigest}

		configDigest, configSize, err := o.OCI.PutBlobJSON(context.Background(), config)
		if err != nil {
			return err
		}

		manifest.Config = ispec.Descriptor{
			MediaType: ispec.MediaTypeImageConfig,
			Digest:    configDigest,
			Size:      configSize,
		}

		manifestDigest, manifestSize, err := o.OCI.PutBlobJSON(context.Background(), manifest)
		if err != nil {
			return err
		}

		desc = ispec.Descriptor{
			MediaType: ispec.MediaTypeImageManifest,
			Digest:    manifestDigest,
			Size:      manifestSize,
		}

		err = o.OCI.UpdateReference(context.Background(), o.Name, desc)
		if err != nil {
			return err
		}

		bundlePath := path.Join(o.Config.RootFSDir, ".working")
		err = updateBundleMtree(bundlePath, desc)
		if err != nil {
			return err
		}

		err = umoci.WriteBundleMeta(bundlePath, umoci.Meta{
			Version: umoci.MetaVersion,
			From: casext.DescriptorPath{
				Walk: []ispec.Descriptor{desc},
			},
		})
		if err != nil {
			return err
		}
	}

	return nil
}

func getDocker(o BaseLayerOpts) error {
	err := runSkopeo(o.Layer.From.Url, o, !o.Layer.BuildOnly && o.LayerType == "tar")
	if err != nil {
		return err
	}

	return extractOutput(o)
}

func umociInit(o BaseLayerOpts) error {
	return RunUmociSubcommand(o.Config, o.Debug, []string{
		"--tag", o.Name,
		"--bundle-path", path.Join(o.Config.RootFSDir, ".working"),
		"init",
	})
}

func getTar(o BaseLayerOpts) error {
	cacheDir := path.Join(o.Config.StackerDir, "layer-bases")
	if err := os.MkdirAll(cacheDir, 0755); err != nil {
		return err
	}

	tar, err := acquireUrl(o.Config, o.Layer.From.Url, cacheDir)
	if err != nil {
		return err
	}

	err = umociInit(o)
	if err != nil {
		return err
	}

	// TODO: make this respect ID maps
	layerPath := path.Join(o.Config.RootFSDir, o.Target, "rootfs")
	output, err := exec.Command("tar", "xf", tar, "-C", layerPath).CombinedOutput()
	if err != nil {
		return fmt.Errorf("error: %s: %s", err, string(output))
	}

	return nil
}

func getScratch(o BaseLayerOpts) error {
	return umociInit(o)
}

func getOCI(o BaseLayerOpts) error {
	err := runSkopeo(fmt.Sprintf("oci:%s", o.Layer.From.Url), o, !o.Layer.BuildOnly)
	if err != nil {
		return err
	}

	return extractOutput(o)
}

func getBuilt(o BaseLayerOpts, sf *Stackerfile) error {
	// We need to copy any base OCI layers to the output dir, since they
	// may not have been copied before and the final `umoci repack` expects
	// them to be there.
	base := o.Layer
	for {
		var ok bool
		base, ok = sf.Get(base.From.Tag)
		if !ok {
			return fmt.Errorf("missing base layer %s?", o.Layer.From.Tag)
		}

		if base.From.Type != BuiltType {
			break
		}
	}

	// Nothing to do here -- we didn't import any base layers.
	if (base.From.Type != DockerType && base.From.Type != OCIType) || !base.BuildOnly {
		return nil
	}

	// Nothing to do here either -- the previous step emitted a layer with
	// the base's tag name. We don't want to overwrite that with a stock
	// base layer.
	if !base.BuildOnly {
		return nil
	}

	tag, err := base.From.ParseTag()
	if err != nil {
		return err
	}

	cacheDir := path.Join(o.Config.StackerDir, "layer-bases", "oci")
	err = lib.ImageCopy(lib.ImageCopyOpts{
		Src:  fmt.Sprintf("oci:%s:%s", cacheDir, tag),
		Dest: fmt.Sprintf("oci:%s:%s", o.Config.OCIDir, tag),
	})
	if err != nil {
		return err
	}

	return o.OCI.DeleteReference(context.Background(), tag)
}

func ComputeAggregateHash(manifest ispec.Manifest, descriptor ispec.Descriptor) (string, error) {
	h := sha256.New()
	found := false

	for _, l := range manifest.Layers {
		_, err := h.Write([]byte(l.Digest.String()))
		if err != nil {
			return "", err
		}

		if l.Digest.String() == descriptor.Digest.String() {
			found = true
			break
		}
	}

	if !found {
		return "", errors.Errorf("couldn't find descriptor %s in manifest %s", descriptor.Digest.String(), manifest.Annotations["org.opencontainers.image.ref.name"])
	}

	return fmt.Sprintf("%x", h.Sum(nil)), nil
}

func RunUmociSubcommand(config StackerConfig, debug bool, args []string) error {
	binary, err := os.Readlink("/proc/self/exe")
	if err != nil {
		return err
	}

	cmd := []string{
		binary,
		"--oci-dir", config.OCIDir,
		"--roots-dir", config.RootFSDir,
		"--stacker-dir", config.StackerDir,
	}

	if debug {
		cmd = append(cmd, "--debug")
	}

	cmd = append(cmd, "umoci")
	cmd = append(cmd, args...)
	return MaybeRunInUserns(cmd, "image unpack failed")
}
