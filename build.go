package stacker

import (
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"os/user"
	"path"
	"runtime"
	"strings"
	"time"

	"github.com/anuvu/stacker/lib"
	stackeroci "github.com/anuvu/stacker/oci"
	"github.com/anuvu/stacker/squashfs"
	"github.com/openSUSE/umoci"
	"github.com/openSUSE/umoci/mutate"
	"github.com/openSUSE/umoci/oci/casext"
	"github.com/openSUSE/umoci/pkg/fseval"
	ispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"
	"github.com/vbatts/go-mtree"
	"golang.org/x/sys/unix"
)

type BuildArgs struct {
	Config                  StackerConfig
	LeaveUnladen            bool
	NoCache                 bool
	Substitute              []string
	OnRunFailure            string
	ApplyConsiderTimestamps bool
	LayerType               string
	Debug                   bool
	OrderOnly               bool
	RemoteSaveTags          []string
}

func updateBundleMtree(rootPath string, newPath ispec.Descriptor) error {
	newName := strings.Replace(newPath.Digest.String(), ":", "_", 1) + ".mtree"

	infos, err := ioutil.ReadDir(rootPath)
	if err != nil {
		return err
	}

	for _, fi := range infos {
		if !strings.HasSuffix(fi.Name(), ".mtree") {
			continue
		}

		return os.Rename(path.Join(rootPath, fi.Name()), path.Join(rootPath, newName))
	}

	return nil
}

func mkSquashfs(config StackerConfig, eps *squashfs.ExcludePaths) (io.ReadCloser, error) {
	// generate the squashfs in OCIDir, and then open it, read it from
	// there, and delete it.
	if err := os.MkdirAll(config.OCIDir, 0755); err != nil {
		return nil, err
	}

	rootfsPath := path.Join(config.RootFSDir, WorkingContainerName, "rootfs")
	return squashfs.MakeSquashfs(config.OCIDir, rootfsPath, eps)
}

func generateSquashfsLayer(oci casext.Engine, name string, author string, opts *BuildArgs) error {
	meta, err := umoci.ReadBundleMeta(path.Join(opts.Config.RootFSDir, WorkingContainerName))
	if err != nil {
		return err
	}

	mtreeName := strings.Replace(meta.From.Descriptor().Digest.String(), ":", "_", 1)
	mtreePath := path.Join(opts.Config.RootFSDir, WorkingContainerName, mtreeName+".mtree")

	mfh, err := os.Open(mtreePath)
	if err != nil {
		return err
	}

	spec, err := mtree.ParseSpec(mfh)
	if err != nil {
		return err
	}

	fsEval := fseval.DefaultFsEval
	rootfsPath := path.Join(opts.Config.RootFSDir, WorkingContainerName, "rootfs")
	newDH, err := mtree.Walk(rootfsPath, nil, umoci.MtreeKeywords, fsEval)
	if err != nil {
		return errors.Wrapf(err, "couldn't mtree walk %s", rootfsPath)
	}

	diffs, err := mtree.CompareSame(spec, newDH, umoci.MtreeKeywords)
	if err != nil {
		return err
	}

	// This is a pretty massive hack, because there's no library for
	// generating squashfs images. However, mksquashfs does take a list of
	// files to exclude from the image. So we go through and accumulate a
	// list of these files.
	//
	// For missing files, since we're going to use overlayfs with
	// squashfs, we use overlayfs' mechanism for whiteouts, which is a
	// character device with device numbers 0/0. But since there's no
	// library for generating squashfs images, we have to write these to
	// the actual filesystem, and then remember what they are so we can
	// delete them later.
	missing := []string{}
	defer func() {
		for _, f := range missing {
			os.Remove(f)
		}
	}()

	paths := squashfs.NewExcludePaths()
	for _, diff := range diffs {
		switch diff.Type() {
		case mtree.Modified, mtree.Extra:
			p := path.Join(rootfsPath, diff.Path())
			missing = append(missing, p)
			paths.AddInclude(p, diff.New().IsDir())
		case mtree.Missing:
			p := path.Join(rootfsPath, diff.Path())
			missing = append(missing, p)
			paths.AddInclude(p, diff.Old().IsDir())
			if err := unix.Mknod(p, unix.S_IFCHR, int(unix.Mkdev(0, 0))); err != nil {
				if !os.IsNotExist(err) && err != unix.ENOTDIR {
					return errors.Wrapf(err, "couldn't mknod whiteout for %s", diff.Path())
				}
			}
		case mtree.Same:
			paths.AddExclude(path.Join(rootfsPath, diff.Path()))
		}
	}

	tmpSquashfs, err := mkSquashfs(opts.Config, paths)
	if err != nil {
		return err
	}
	defer tmpSquashfs.Close()

	desc, err := stackeroci.AddBlobNoCompression(oci, name, tmpSquashfs)
	if err != nil {
		return err
	}

	newName := strings.Replace(desc.Digest.String(), ":", "_", 1) + ".mtree"
	err = umoci.GenerateBundleManifest(newName, path.Join(opts.Config.RootFSDir, WorkingContainerName), fsEval)
	if err != nil {
		return err
	}

	os.Remove(mtreePath)
	meta.From = casext.DescriptorPath{
		Walk: []ispec.Descriptor{desc},
	}
	err = umoci.WriteBundleMeta(path.Join(opts.Config.RootFSDir, WorkingContainerName), meta)
	if err != nil {
		return err
	}

	return nil
}

// SaveLayer stores the final layers into a separate location based on the content of
// the stackerfile, this is useful to avoid an extra manual step to upload build results
// and also in case of caching in between stacker builds
// The logic should work for both Docker registry destination and OCI layout destinations
// In case of OCI layout destinations the tag will be included in the layer name
func SaveLayer(opts *BuildArgs, sf *Stackerfile, name string) error {
	if len(sf.buildConfig.SaveUrl) == 0 {
		return fmt.Errorf("layer %s cannot be saved since it doesn't have a save URL", name)
	}

	// Need to determine if URL is docker/oci or something else
	is, err := NewImageSource(sf.buildConfig.SaveUrl)
	if err != nil {
		return err
	}

	// Determine list of tags to be used
	tags := opts.RemoteSaveTags

	// Attempt to produce a git commit tag
	commitTag, err := NewGitLayerTag(sf.referenceDirectory)
	if err == nil {
		// Add git tag to the list of tags to be used
		tags = append(tags, commitTag)
	}

	if len(tags) == 0 {
		fmt.Printf("can't save layer %s since list of tags is empty\n", name)
	}

	// Store the layers to new detination
	for _, tag := range tags {
		var destUrl string
		switch is.Type {
		case DockerType:
			destUrl = fmt.Sprintf("%s/%s:%s", strings.TrimRight(sf.buildConfig.SaveUrl, "/"), name, tag)
		case OCIType:
			destUrl = fmt.Sprintf("%s:%s_%s", sf.buildConfig.SaveUrl, name, tag)
		default:
			return fmt.Errorf("can't save layers to destination type: %s", is.Type)
		}

		fmt.Printf("saving %s\n", destUrl)
		err = lib.ImageCopy(lib.ImageCopyOpts{
			Src:      fmt.Sprintf("oci:%s:%s", opts.Config.OCIDir, name),
			Dest:     destUrl,
			Progress: os.Stdout,
			SkipTLS:  true,
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// Builder is responsible for building the layers based on stackerfiles
type Builder struct {
	builtStackerfiles StackerFiles // Keep track of all the Stackerfiles which were built
	opts              *BuildArgs   // Build options
}

// NewBuilder initializes a new Builder struct
func NewBuilder(opts *BuildArgs) *Builder {
	return &Builder{
		builtStackerfiles: make(map[string]*Stackerfile, 1),
		opts:              opts,
	}
}

// Build builds a single stackerfile
func (b *Builder) Build(file string) error {
	opts := b.opts

	if opts.NoCache {
		os.RemoveAll(opts.Config.StackerDir)
	}

	sf, err := NewStackerfile(file, opts.Substitute)
	if err != nil {
		return err
	}

	s, err := NewStorage(opts.Config)
	if err != nil {
		return err
	}
	if !opts.LeaveUnladen {
		defer s.Detach()
	}

	order, err := sf.DependencyOrder()
	if err != nil {
		return err
	}

	var oci casext.Engine
	if _, statErr := os.Stat(opts.Config.OCIDir); statErr != nil {
		oci, err = umoci.CreateLayout(opts.Config.OCIDir)
	} else {
		oci, err = umoci.OpenLayout(opts.Config.OCIDir)
	}
	if err != nil {
		return err
	}
	defer oci.Close()

	// Add this stackerfile to the list of stackerfiles which were built
	b.builtStackerfiles[file] = sf
	buildCache, err := OpenCache(opts.Config, oci, b.builtStackerfiles)
	if err != nil {
		return err
	}

	// compute the git version for the directory that the stacker file is
	// in. we don't care if it's not a git directory, because in that case
	// we'll fall back to putting the whole stacker file contents in the
	// metadata.
	gitVersion, _ := GitVersion(sf.referenceDirectory)

	username := os.Getenv("SUDO_USER")

	if username == "" {
		user, err := user.Current()
		if err != nil {
			return err
		}

		username = user.Username
	}

	host, err := os.Hostname()
	if err != nil {
		return err
	}

	author := fmt.Sprintf("%s@%s", username, host)

	s.Delete(WorkingContainerName)
	for _, name := range order {
		l, ok := sf.Get(name)
		if !ok {
			return fmt.Errorf("%s not present in stackerfile?", name)
		}

		fmt.Printf("building image %s...\n", name)

		// We need to run the imports first since we now compare
		// against imports for caching layers. Since we don't do
		// network copies if the files are present and we use rsync to
		// copy things across, hopefully this isn't too expensive.
		fmt.Println("importing files...")
		imports, err := l.ParseImport()
		if err != nil {
			return err
		}

		if err := Import(opts.Config, name, imports); err != nil {
			return err
		}

		cacheEntry, ok := buildCache.Lookup(name)
		if ok {
			if l.BuildOnly {
				if cacheEntry.Name != name {
					err = s.Snapshot(cacheEntry.Name, name)
					if err != nil {
						return err
					}
				}
			} else {
				err = oci.UpdateReference(context.Background(), name, cacheEntry.Blob)
				if err != nil {
					return err
				}
			}
			fmt.Printf("found cached layer %s\n", name)

			// Save image if requested by user
			if len(sf.buildConfig.SaveUrl) != 0 {
				err := SaveLayer(opts, sf, name)
				if err != nil {
					return err
				}
			}

			continue
		}

		baseOpts := BaseLayerOpts{
			Config:    opts.Config,
			Name:      name,
			Target:    WorkingContainerName,
			Layer:     l,
			Cache:     buildCache,
			OCI:       oci,
			LayerType: opts.LayerType,
			Debug:     opts.Debug,
		}

		s.Delete(WorkingContainerName)
		if l.From.Type == BuiltType {
			if err := s.Restore(l.From.Tag, WorkingContainerName); err != nil {
				return err
			}
		} else {
			if err := s.Create(WorkingContainerName); err != nil {
				return err
			}
		}

		err = GetBaseLayer(baseOpts, b.builtStackerfiles)
		if err != nil {
			return err
		}

		apply, err := NewApply(b.builtStackerfiles, baseOpts, s, opts.ApplyConsiderTimestamps)
		if err != nil {
			return err
		}

		err = apply.DoApply()
		if err != nil {
			return err
		}

		fmt.Println("running commands...")

		run, err := l.ParseRun()
		if err != nil {
			return err
		}

		if len(run) != 0 {
			_, err := os.Stat(path.Join(opts.Config.RootFSDir, WorkingContainerName, "rootfs/bin/sh"))
			if err != nil {
				return fmt.Errorf("rootfs for %s does not have a /bin/sh", name)
			}

			importsDir := path.Join(opts.Config.StackerDir, "imports", name)

			script := fmt.Sprintf("#!/bin/sh -xe\n%s", strings.Join(run, "\n"))
			if err := ioutil.WriteFile(path.Join(importsDir, ".stacker-run.sh"), []byte(script), 0755); err != nil {
				return err
			}

			fmt.Println("running commands for", name)
			if err := Run(opts.Config, name, "/stacker/.stacker-run.sh", l, opts.OnRunFailure, nil); err != nil {
				return err
			}
		}

		// This is a build only layer, meaning we don't need to include
		// it in the final image, as outputs from it are going to be
		// imported into future images. Let's just snapshot it and add
		// a bogus entry to our cache.
		if l.BuildOnly {
			s.Delete(name)
			if err := s.Snapshot(WorkingContainerName, name); err != nil {
				return err
			}

			fmt.Println("build only layer, skipping OCI diff generation")

			// A small hack: for build only layers, we keep track
			// of the name, so we can make sure it exists when
			// there is a cache hit. We should probably make this
			// into some sort of proper Either type.
			if err := buildCache.Put(name, ispec.Descriptor{}); err != nil {
				return err
			}
			continue
		}

		fmt.Println("generating layer for", name)
		switch opts.LayerType {
		case "tar":
			err = RunUmociSubcommand(opts.Config, opts.Debug, []string{
				"--tag", name,
				"--bundle-path", path.Join(opts.Config.RootFSDir, WorkingContainerName),
				"repack",
			})
			if err != nil {
				return err
			}
		case "squashfs":
			err = generateSquashfsLayer(oci, name, author, opts)
			if err != nil {
				return err
			}
		default:
			return fmt.Errorf("unknown layer type: %s", opts.LayerType)
		}
		descPaths, err := oci.ResolveReference(context.Background(), name)
		if err != nil {
			return err
		}

		mutator, err := mutate.New(oci, descPaths[0])
		if err != nil {
			return errors.Wrapf(err, "mutator failed")
		}

		imageConfig, err := mutator.Config(context.Background())
		if err != nil {
			return err
		}

		pathSet := false
		for k, v := range l.Environment {
			if k == "PATH" {
				pathSet = true
			}
			imageConfig.Env = append(imageConfig.Env, fmt.Sprintf("%s=%s", k, v))
		}

		if !pathSet {
			for _, s := range imageConfig.Env {
				if strings.HasPrefix(s, "PATH=") {
					pathSet = true
					break
				}
			}
		}

		// if the user didn't specify a path, let's set a sane one
		if !pathSet {
			imageConfig.Env = append(imageConfig.Env, fmt.Sprintf("PATH=%s", ReasonableDefaultPath))
		}

		if l.Cmd != nil {
			imageConfig.Cmd, err = l.ParseCmd()
			if err != nil {
				return err
			}
		}

		if l.Entrypoint != nil {
			imageConfig.Entrypoint, err = l.ParseEntrypoint()
			if err != nil {
				return err
			}
		}

		if l.FullCommand != nil {
			imageConfig.Cmd = nil
			imageConfig.Entrypoint, err = l.ParseFullCommand()
			if err != nil {
				return err
			}
		}

		if imageConfig.Volumes == nil {
			imageConfig.Volumes = map[string]struct{}{}
		}

		for _, v := range l.Volumes {
			imageConfig.Volumes[v] = struct{}{}
		}

		if imageConfig.Labels == nil {
			imageConfig.Labels = map[string]string{}
		}

		for k, v := range l.Labels {
			imageConfig.Labels[k] = v
		}

		if l.WorkingDir != "" {
			imageConfig.WorkingDir = l.WorkingDir
		}

		meta, err := mutator.Meta(context.Background())
		if err != nil {
			return err
		}

		meta.Created = time.Now()
		meta.Architecture = runtime.GOARCH
		meta.OS = runtime.GOOS
		meta.Author = author

		annotations, err := mutator.Annotations(context.Background())
		if err != nil {
			return err
		}

		if gitVersion != "" {
			fmt.Println("setting git version annotation to", gitVersion)
			annotations[GitVersionAnnotation] = gitVersion
		} else {
			annotations[StackerContentsAnnotation] = sf.AfterSubstitutions
		}

		history := ispec.History{
			EmptyLayer: true, // this is only the history for imageConfig edit
			Created:    &meta.Created,
			CreatedBy:  "stacker build",
			Author:     author,
		}

		err = mutator.Set(context.Background(), imageConfig, meta, annotations, &history)
		if err != nil {
			return err
		}

		newPath, err := mutator.Commit(context.Background())
		if err != nil {
			return err
		}

		err = oci.UpdateReference(context.Background(), name, newPath.Root())
		if err != nil {
			return err
		}

		// Now, we need to set the umoci data on the fs to tell it that
		// it has a layer that corresponds to this fs.
		bundlePath := path.Join(opts.Config.RootFSDir, WorkingContainerName)
		err = updateBundleMtree(bundlePath, newPath.Descriptor())
		if err != nil {
			return err
		}

		umociMeta := umoci.Meta{Version: umoci.MetaVersion, From: newPath}
		err = umoci.WriteBundleMeta(bundlePath, umociMeta)
		if err != nil {
			return err
		}

		// Delete the old snapshot if it existed; we just did a new build.
		s.Delete(name)
		if err := s.Snapshot(WorkingContainerName, name); err != nil {
			return err
		}

		fmt.Printf("filesystem %s built successfully\n", name)

		descPaths, err = oci.ResolveReference(context.Background(), name)
		if err != nil {
			return err
		}

		if err := buildCache.Put(name, descPaths[0].Descriptor()); err != nil {
			return err
		}

		// Save image if requested by user
		if len(sf.buildConfig.SaveUrl) != 0 {
			err := SaveLayer(opts, sf, name)
			if err != nil {
				return err
			}
		}
	}

	err = oci.GC(context.Background())
	if err != nil {
		fmt.Printf("final OCI GC failed: %v\n", err)
	}

	return err
}

// BuildMultiple builds a list of stackerfiles
func (b *Builder) BuildMultiple(paths []string) error {
	opts := b.opts

	// Read all the stacker recipes
	stackerFiles, err := NewStackerFiles(paths, opts.Substitute)
	if err != nil {
		return err
	}

	// Initialize the DAG
	dag, err := NewStackerFilesDAG(stackerFiles)
	if err != nil {
		return err
	}

	sortedPaths := dag.Sort()

	// Show the serial build order
	fmt.Printf("stacker build order:\n")
	for i, p := range sortedPaths {
		prerequisites, err := dag.GetStackerFile(p).Prerequisites()
		if err != nil {
			return err
		}
		fmt.Printf("%d build %s: requires: %v\n", i, p, prerequisites)
	}

	if opts.OrderOnly {
		// User has requested only to see the build order, so skipping the actual build
		return nil
	}

	// Build all Stackerfiles
	for i, p := range sortedPaths {
		fmt.Printf("building: %d %s\n", i, p)

		err = b.Build(p)
		if err != nil {
			return err
		}
	}

	return nil
}
