// Copyright 2014 CoreOS, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

//+build linux

package stage0

//
// rkt is a reference implementation of the app container specification.
//
// Execution on rkt is divided into a number of stages, and the `rkt`
// binary implements the first stage (stage 0)
//

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"

	"github.com/coreos/rkt/Godeps/_workspace/src/github.com/appc/spec/schema"
	"github.com/coreos/rkt/Godeps/_workspace/src/github.com/appc/spec/schema/types"
	"github.com/coreos/rkt/cas"
	"github.com/coreos/rkt/common"
	"github.com/coreos/rkt/pkg/aci"
	"github.com/coreos/rkt/pkg/sys"
	"github.com/coreos/rkt/version"
)

// configuration parameters required by Prepare
type PrepareConfig struct {
	CommonConfig
	// TODO(jonboulle): These images are partially-populated hashes, this should be clarified.
	ExecAppends [][]string     // appendages to each image's app.exec lines (empty when none, length should match length of Images)
	InheritEnv  bool           // inherit parent environment into apps
	ExplicitEnv []string       // always set these environment variables for all the apps
	Volumes     []types.Volume // list of volumes that rkt can provide to applications
	UseOverlay  bool           // prepare pod with overlay fs
}

// configuration parameters needed by Run
type RunConfig struct {
	CommonConfig
	PrivateNet           bool // pod should have its own network stack
	SpawnMetadataService bool // launch metadata service
	LockFd               int  // lock file descriptor
	Interactive          bool // whether the pod is interactive or not
}

// configuration shared by both Run and Prepare
type CommonConfig struct {
	Store       *cas.Store   // store containing all of the configured application images
	Stage1Image types.Hash   // stage1 image containing usable /init and /enter entrypoints
	UUID        *types.UUID  // UUID of the pod
	Images      []types.Hash // application images
	PodsDir     string       // root directory for rkt pods
	Debug       bool
}

func init() {
	log.SetOutput(ioutil.Discard)

	// this ensures that main runs only on main thread (thread group leader).
	// since namespace ops (unshare, setns) are done for a single thread, we
	// must ensure that the goroutine does not jump from OS thread to thread
	runtime.LockOSThread()
}

// MergeEnvs amends appEnv setting variables in setEnv before setting anything new from os.Environ if inheritEnv = true
// setEnv is expected to be in the os.Environ() key=value format
func MergeEnvs(appEnv *types.Environment, inheritEnv bool, setEnv []string) {
	for _, ev := range setEnv {
		pair := strings.SplitN(ev, "=", 2)
		appEnv.Set(pair[0], pair[1])
	}

	if inheritEnv {
		for _, ev := range os.Environ() {
			pair := strings.SplitN(ev, "=", 2)
			if _, exists := appEnv.Get(pair[0]); !exists {
				appEnv.Set(pair[0], pair[1])
			}
		}
	}
}

// Prepare sets up a pod based on the given config.
func Prepare(cfg PrepareConfig, dir string, uuid *types.UUID) error {
	if cfg.Debug {
		log.SetOutput(os.Stderr)
	}

	log.Printf("Preparing stage1")
	if err := prepareStage1Image(cfg, cfg.Stage1Image, dir, cfg.UseOverlay); err != nil {
		return fmt.Errorf("error preparing stage1: %v", err)
	}

	cm := schema.PodManifest{
		ACKind: "PodManifest",
		Apps:   make(schema.AppList, 0),
	}

	v, err := types.NewSemVer(version.Version)
	if err != nil {
		return fmt.Errorf("error creating version: %v", err)
	}
	cm.ACVersion = *v

	for i, img := range cfg.Images {
		am, err := prepareAppImage(cfg, img, dir, cfg.UseOverlay)
		if err != nil {
			return fmt.Errorf("error setting up image %s: %v", img, err)
		}
		if cm.Apps.Get(am.Name) != nil {
			return fmt.Errorf("error: multiple apps with name %s", am.Name)
		}
		if am.App == nil {
			return fmt.Errorf("error: image %s has no app section", img)
		}
		a := schema.RuntimeApp{
			// TODO(vc): leverage RuntimeApp.Name for disambiguating the apps
			Name: am.Name,
			Image: schema.RuntimeImage{
				Name: &am.Name,
				ID:   img,
			},
			Annotations: am.Annotations,
		}

		if len(cfg.ExecAppends[i]) > 0 {
			a.App = am.App
			a.App.Exec = append(a.App.Exec, cfg.ExecAppends[i]...)
		}

		if cfg.InheritEnv || len(cfg.ExplicitEnv) > 0 {
			if a.App == nil {
				a.App = am.App
			}
			MergeEnvs(&a.App.Environment, cfg.InheritEnv, cfg.ExplicitEnv)
		}
		cm.Apps = append(cm.Apps, a)
	}

	// TODO(jonboulle): check that app mountpoint expectations are
	// satisfied here, rather than waiting for stage1
	cm.Volumes = cfg.Volumes

	cdoc, err := json.Marshal(cm)
	if err != nil {
		return fmt.Errorf("error marshalling pod manifest: %v", err)
	}

	log.Printf("Writing pod manifest")
	fn := common.PodManifestPath(dir)
	if err := ioutil.WriteFile(fn, cdoc, 0700); err != nil {
		return fmt.Errorf("error writing pod manifest: %v", err)
	}

	fn = path.Join(dir, common.Stage1IDFilename)
	if err := ioutil.WriteFile(fn, []byte(cfg.Stage1Image.String()), 0700); err != nil {
		return fmt.Errorf("error writing stage1 ID: %v", err)
	}

	if cfg.UseOverlay {
		// mark the pod as prepared with overlay
		f, err := os.Create(filepath.Join(dir, common.OverlayPreparedFilename))
		if err != nil {
			return fmt.Errorf("error writing overlay marker file: %v", err)
		}
		defer f.Close()
	}

	return nil
}

func preparedWithOverlay(dir string) (bool, error) {
	_, err := os.Stat(filepath.Join(dir, common.OverlayPreparedFilename))
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}

	if !common.SupportsOverlay() {
		return false, fmt.Errorf("the pod was prepared with overlay but overlay is not supported")
	}

	return true, nil
}

// Run mounts the right overlay filesystems and actually runs the prepared
// pod by exec()ing the stage1 init inside the pod filesystem.
func Run(cfg RunConfig, dir string) {
	useOverlay, err := preparedWithOverlay(dir)
	if err != nil {
		log.Fatalf("error: %v", err)
	}

	if useOverlay {
		// create a separate mount namespace so the overlay mounts are
		// unmounted when exiting the pod
		if err := syscall.Unshare(syscall.CLONE_NEWNS); err != nil {
			log.Fatalf("error unsharing: %v", err)
		}

		// we recursively make / a "shared and slave" so mount events from the
		// new namespace don't propagate to the host namespace but mount events
		// from the host propagate to the new namespace and are forwarded to
		// its peer group
		// See https://www.kernel.org/doc/Documentation/filesystems/sharedsubtree.txt
		if err := syscall.Mount("", "/", "none", syscall.MS_REC|syscall.MS_SLAVE, ""); err != nil {
			log.Fatalf("error making / a slave mount: %v", err)
		}
		if err := syscall.Mount("", "/", "none", syscall.MS_REC|syscall.MS_SHARED, ""); err != nil {
			log.Fatalf("error making / a shared and slave mount: %v", err)
		}
	}

	log.Printf("Setting up stage1")
	if err := setupStage1Image(cfg, cfg.Stage1Image, dir, useOverlay); err != nil {
		log.Fatalf("error setting up stage1: %v", err)
	}
	log.Printf("Wrote filesystem to %s\n", dir)

	for _, img := range cfg.Images {
		if err := setupAppImage(cfg, img, dir, useOverlay); err != nil {
			log.Fatalf("error setting up app image: %v", err)
		}
	}

	if err := os.Setenv(common.EnvLockFd, fmt.Sprintf("%v", cfg.LockFd)); err != nil {
		log.Fatalf("setting lock fd environment: %v", err)
	}

	if cfg.SpawnMetadataService {
		log.Print("Launching metadata svc")
		if err := launchMetadataService(cfg.Debug); err != nil {
			log.Printf("Failed to launch metadata svc: %v", err)
		}
	}

	log.Printf("Pivoting to filesystem %s", dir)
	if err := os.Chdir(dir); err != nil {
		log.Fatalf("failed changing to dir: %v", err)
	}

	ep, err := getStage1Entrypoint(dir, runEntrypoint)
	if err != nil {
		log.Fatalf("error determining init entrypoint: %v", err)
	}
	log.Printf("Execing %s", ep)

	args := []string{filepath.Join(common.Stage1RootfsPath(dir), ep)}
	if cfg.Debug {
		args = append(args, "--debug")
	}
	if cfg.PrivateNet {
		args = append(args, "--private-net")
	}
	if cfg.Interactive {
		args = append(args, "--interactive")
	}
	args = append(args, cfg.UUID.String())

	// make sure the lock fd stays open across exec
	if err := sys.CloseOnExec(cfg.LockFd, false); err != nil {
		log.Fatalf("error clearing FD_CLOEXEC on lock fd")
	}

	if err := syscall.Exec(args[0], args, os.Environ()); err != nil {
		log.Fatalf("error execing init: %v", err)
	}
}

// prepareAppImage renders and verifies the tree cache of the app image that
// corresponds to the given hash.
// When useOverlay is false, it attempts to render and expand the app image
// TODO(jonboulle): tighten up the Hash type here; currently it is partially-populated (i.e. half-length sha512)
func prepareAppImage(cfg PrepareConfig, img types.Hash, cdir string, useOverlay bool) (*schema.ImageManifest, error) {
	log.Println("Loading image", img.String())

	if useOverlay {
		if err := cfg.Store.RenderTreeStore(img.String(), false); err != nil {
			return nil, fmt.Errorf("error rendering tree image: %v", err)
		}
		if err := cfg.Store.CheckTreeStore(img.String()); err != nil {
			log.Printf("Warning: tree cache is in a bad state. Rebuilding...")
			if err := cfg.Store.RenderTreeStore(img.String(), true); err != nil {
				return nil, fmt.Errorf("error rendering tree image: %v", err)
			}
		}
	} else {
		ad := common.AppImagePath(cdir, img)
		err := os.MkdirAll(ad, 0755)
		if err != nil {
			return nil, fmt.Errorf("error creating image directory: %v", err)
		}

		if err := aci.RenderACIWithImageID(img, ad, cfg.Store); err != nil {
			return nil, fmt.Errorf("error rendering ACI: %v", err)
		}
	}

	am, err := cfg.Store.GetImageManifest(img.String())
	if err != nil {
		return nil, fmt.Errorf("error getting the manifest: %v", err)
	}

	return am, nil
}

// setupAppImage mounts the overlay filesystem for the app image that
// corresponds to the given hash. Then, it creates the tmp directory.
// When useOverlay is false it just creates the tmp directory for this app.
func setupAppImage(cfg RunConfig, img types.Hash, cdir string, useOverlay bool) error {
	ad := common.AppImagePath(cdir, img)
	if useOverlay {
		err := os.MkdirAll(ad, 0776)
		if err != nil {
			return fmt.Errorf("error creating image directory: %v", err)
		}

		if err := overlayRender(cfg, img, cdir, ad); err != nil {
			return fmt.Errorf("error rendering overlay filesystem: %v", err)
		}
	}

	err := os.MkdirAll(filepath.Join(ad, "rootfs/tmp"), 0777)
	if err != nil {
		return fmt.Errorf("error creating tmp directory: %v", err)
	}

	return nil
}

// prepareStage1Image renders and verifies tree cache of the given hash
// when using overlay.
// When useOverlay is false, it attempts to render and expand the stage1.
func prepareStage1Image(cfg PrepareConfig, img types.Hash, cdir string, useOverlay bool) error {
	s1 := common.Stage1ImagePath(cdir)
	if err := os.MkdirAll(s1, 0755); err != nil {
		return fmt.Errorf("error creating stage1 directory: %v", err)
	}

	if err := cfg.Store.RenderTreeStore(img.String(), false); err != nil {
		return fmt.Errorf("error rendering tree image: %v", err)
	}
	if err := cfg.Store.CheckTreeStore(img.String()); err != nil {
		log.Printf("Warning: tree cache is in a bad state. Rebuilding...")
		if err := cfg.Store.RenderTreeStore(img.String(), true); err != nil {
			return fmt.Errorf("error rendering tree image: %v", err)
		}
	}

	if !useOverlay {
		if err := aci.RenderACIWithImageID(img, s1, cfg.Store); err != nil {
			return fmt.Errorf("error rendering ACI: %v", err)
		}
	}
	return nil
}

// setupStage1Image mounts the overlay filesystem for stage1.
// When useOverlay is false it is a noop
func setupStage1Image(cfg RunConfig, img types.Hash, cdir string, useOverlay bool) error {
	if useOverlay {
		s1 := common.Stage1ImagePath(cdir)
		if err := overlayRender(cfg, img, cdir, s1); err != nil {
			return fmt.Errorf("error rendering overlay filesystem: %v", err)
		}
	}

	return nil
}

// overlayRender renders the image that corresponds to the given hash using the
// overlay filesystem.
// It writes the manifest in the specified directory and mounts an overlay
// filesystem from the cached tree of the image as rootfs.
func overlayRender(cfg RunConfig, img types.Hash, cdir string, dest string) error {
	manifest, err := cfg.Store.GetImageManifest(img.String())
	if err != nil {
		return err
	}

	mb, err := json.Marshal(manifest)
	if err != nil {
		return fmt.Errorf("error marshalling image manifest: %v", err)
	}

	log.Printf("Writing image manifest")
	if err := ioutil.WriteFile(filepath.Join(dest, "manifest"), mb, 0700); err != nil {
		return fmt.Errorf("error writing pod manifest: %v", err)
	}

	destRootfs := path.Join(dest, "rootfs")
	if err := os.MkdirAll(destRootfs, 0755); err != nil {
		return err
	}

	cachedTreePath := cfg.Store.GetTreeStoreRootFS(img.String())

	overlayDir := path.Join(cdir, "overlay", img.String())
	if err := os.MkdirAll(overlayDir, 0755); err != nil {
		return err
	}

	upperDir := path.Join(overlayDir, "upper")
	if err := os.MkdirAll(upperDir, 0755); err != nil {
		return err
	}
	workDir := path.Join(overlayDir, "work")
	if err := os.MkdirAll(workDir, 0755); err != nil {
		return err
	}

	opts := fmt.Sprintf("lowerdir=%s,upperdir=%s,workdir=%s", cachedTreePath, upperDir, workDir)
	if err := syscall.Mount("overlay", destRootfs, "overlay", 0, opts); err != nil {
		return fmt.Errorf("error mounting: %v", err)
	}

	return nil
}

func launchMetadataService(debug bool) error {
	// readlink so arg[0] displays useful info
	exe, err := os.Readlink("/proc/self/exe")
	if err != nil {
		return fmt.Errorf("error reading /proc/self/exe link: %v", err)
	}

	args := []string{exe}
	if debug {
		args = append(args, "--debug")
	}
	args = append(args, "metadata-service", "--no-idle")

	cmd := exec.Cmd{
		Path:   exe,
		Args:   args,
		Stdout: os.Stdout,
		Stderr: os.Stderr,
	}

	return cmd.Start()
}
