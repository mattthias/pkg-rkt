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

package cas

import (
	"bufio"
	"bytes"
	"crypto/sha512"
	"database/sql"
	"encoding/json"
	"fmt"
	"hash"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/coreos/rkt/pkg/lock"

	"github.com/coreos/rkt/Godeps/_workspace/src/github.com/appc/spec/aci"
	"github.com/coreos/rkt/Godeps/_workspace/src/github.com/appc/spec/schema"
	"github.com/coreos/rkt/Godeps/_workspace/src/github.com/appc/spec/schema/types"

	"github.com/coreos/rkt/Godeps/_workspace/src/github.com/peterbourgon/diskv"
)

const (
	blobType int64 = iota
	imageManifestType

	defaultPathPerm os.FileMode = 0777
	defaultFilePerm os.FileMode = 0660

	// To ameliorate excessively long paths, keys for the (blob)store use
	// only the first half of a sha512 rather than the entire sum
	hashPrefix = "sha512-"
	lenHash    = sha512.Size       // raw byte size
	lenHashKey = (lenHash / 2) * 2 // half length, in hex characters
	lenKey     = len(hashPrefix) + lenHashKey
	minlenKey  = len(hashPrefix) + 2 // at least sha512-aa
)

var diskvStores = [...]string{
	"blob",
	"imageManifest",
}

// Store encapsulates a content-addressable-storage for storing ACIs on disk.
type Store struct {
	base             string
	stores           []*diskv.Diskv
	db               *DB
	treestore        *TreeStore
	imageLockDir     string
	treeStoreLockDir string
}

func NewStore(base string) (*Store, error) {
	casDir := filepath.Join(base, "cas")

	ds := &Store{
		base:   base,
		stores: make([]*diskv.Diskv, len(diskvStores)),
	}

	ds.imageLockDir = filepath.Join(casDir, "imagelocks")
	err := os.MkdirAll(ds.imageLockDir, defaultPathPerm)
	if err != nil {
		return nil, err
	}

	ds.treeStoreLockDir = filepath.Join(casDir, "treestorelocks")
	err = os.MkdirAll(ds.treeStoreLockDir, defaultPathPerm)
	if err != nil {
		return nil, err
	}

	for i, p := range diskvStores {
		ds.stores[i] = diskv.New(diskv.Options{
			BasePath:  filepath.Join(casDir, p),
			Transform: blockTransform,
		})
	}
	db, err := NewDB(filepath.Join(casDir, "db"))
	if err != nil {
		return nil, err
	}
	ds.db = db

	fn := func(tx *sql.Tx) error {
		ok, err := dbIsPopulated(tx)
		if err != nil {
			return err
		}
		// populate the db
		if !ok {
			for _, stmt := range dbCreateStmts {
				_, err = tx.Exec(stmt)
				if err != nil {
					return err
				}
			}
		}
		// if db is populated check its version
		version, err := getDBVersion(tx)
		if err != nil {
			return err
		}
		if version < dbVersion {
			// TODO(sgotti) execute migration functions
			return fmt.Errorf("Current cas db version: %d lesser than the current rkt expected version: %d", version, dbVersion)
		}
		if version > dbVersion {
			return fmt.Errorf("Current cas db version: %d greater than the current rkt expected version: %d", version, dbVersion)
		}
		return nil
	}

	if err = db.Do(fn); err != nil {
		return nil, err
	}

	ds.treestore = &TreeStore{path: filepath.Join(base, "cas", "tree")}

	return ds, nil
}

// TmpFile returns an *os.File local to the same filesystem as the Store, or
// any error encountered
func (ds Store) TmpFile() (*os.File, error) {
	dir, err := ds.TmpDir()
	if err != nil {
		return nil, err
	}
	return ioutil.TempFile(dir, "")
}

// TmpDir creates and returns dir local to the same filesystem as the Store,
// or any error encountered
func (ds Store) TmpDir() (string, error) {
	dir := filepath.Join(ds.base, "tmp")
	if err := os.MkdirAll(dir, defaultPathPerm); err != nil {
		return "", err
	}
	return dir, nil
}

// ResolveKey resolves a partial key (of format `sha512-0c45e8c0ab2`) to a full
// key by considering the key a prefix and using the store for resolution.
// If the key is longer than the full key length, it is first truncated.
func (ds Store) ResolveKey(key string) (string, error) {
	if !strings.HasPrefix(key, hashPrefix) {
		return "", fmt.Errorf("wrong key prefix")
	}
	if len(key) < minlenKey {
		return "", fmt.Errorf("key too short")
	}
	if len(key) > lenKey {
		key = key[:lenKey]
	}

	aciInfos := []*ACIInfo{}
	err := ds.db.Do(func(tx *sql.Tx) error {
		var err error
		aciInfos, err = GetACIInfosWithKeyPrefix(tx, key)
		return err
	})
	if err != nil {
		return "", fmt.Errorf("error retrieving ACI Infos: %v", err)
	}

	keyCount := len(aciInfos)
	if keyCount == 0 {
		return "", fmt.Errorf("no keys found")
	}
	if keyCount != 1 {
		return "", fmt.Errorf("ambiguous key: %q", key)
	}
	return aciInfos[0].BlobKey, nil
}

func (ds Store) ReadStream(key string) (io.ReadCloser, error) {
	key, err := ds.ResolveKey(key)
	if err != nil {
		return nil, fmt.Errorf("error resolving key: %v", err)
	}
	keyLock, err := lock.SharedKeyLock(ds.imageLockDir, key)
	if err != nil {
		return nil, fmt.Errorf("error locking image: %v", err)
	}
	defer keyLock.Close()

	return ds.stores[blobType].ReadStream(key, false)
}

// WriteACI takes an ACI encapsulated in an io.Reader, decompresses it if
// necessary, and then stores it in the store under a key based on the image ID
// (i.e. the hash of the uncompressed ACI)
// latest defines if the aci has to be marked as the latest. For example an ACI
// discovered without asking for a specific version (latest pattern).
func (ds Store) WriteACI(r io.Reader, latest bool) (string, error) {
	// Peek at the first 512 bytes of the reader to detect filetype
	br := bufio.NewReaderSize(r, 32768)
	hd, err := br.Peek(512)
	switch err {
	case nil:
	case io.EOF: // We may have still peeked enough to guess some types, so fall through
	default:
		return "", fmt.Errorf("error reading image header: %v", err)
	}
	typ, err := aci.DetectFileType(bytes.NewBuffer(hd))
	if err != nil {
		return "", fmt.Errorf("error detecting image type: %v", err)
	}
	dr, err := decompress(br, typ)
	if err != nil {
		return "", fmt.Errorf("error decompressing image: %v", err)
	}

	// Write the decompressed image (tar) to a temporary file on disk, and
	// tee so we can generate the hash
	h := sha512.New()
	tr := io.TeeReader(dr, h)
	fh, err := ds.TmpFile()
	if err != nil {
		return "", fmt.Errorf("error creating image: %v", err)
	}
	if _, err := io.Copy(fh, tr); err != nil {
		return "", fmt.Errorf("error copying image: %v", err)
	}
	im, err := aci.ManifestFromImage(fh)
	if err != nil {
		return "", fmt.Errorf("error extracting image manifest: %v", err)
	}
	if err := fh.Close(); err != nil {
		return "", fmt.Errorf("error closing image: %v", err)
	}

	// Import the uncompressed image into the store at the real key
	key := ds.HashToKey(h)
	keyLock, err := lock.ExclusiveKeyLock(ds.imageLockDir, key)
	if err != nil {
		return "", fmt.Errorf("error locking image: %v", err)
	}
	defer keyLock.Close()

	if err = ds.stores[blobType].Import(fh.Name(), key, true); err != nil {
		return "", fmt.Errorf("error importing image: %v", err)
	}

	// Save the imagemanifest using the same key used for the image
	imj, err := json.Marshal(im)
	if err != nil {
		return "", fmt.Errorf("error marshalling image manifest: %v", err)
	}
	if err = ds.stores[imageManifestType].Write(key, imj); err != nil {
		return "", fmt.Errorf("error importing image manifest: %v", err)
	}

	// Save aciinfo
	if err = ds.db.Do(func(tx *sql.Tx) error {
		aciinfo := &ACIInfo{
			BlobKey:    key,
			AppName:    im.Name.String(),
			ImportTime: time.Now(),
			Latest:     latest,
		}
		return WriteACIInfo(tx, aciinfo)
	}); err != nil {
		return "", fmt.Errorf("error writing ACI Info: %v", err)
	}

	// The treestore for this ACI is not written here as ACIs downloaded as
	// dependencies of another ACI will be exploded also if never directly used.
	// Users of treestore should call ds.RenderTreeStore before using it.

	return key, nil
}

// RenderTreeStore renders a treestore for the given image key if it's not
// already fully rendered.
// Users of treestore should call ds.RenderTreeStore before using it to ensure
// that the treestore is completely rendered.
func (ds Store) RenderTreeStore(key string, rebuild bool) error {
	// this lock references the treestore dir for the specified key. This
	// is different from a lock on an image key as internally
	// treestore.Write calls the acirenderer functions that use GetACI and
	// GetImageManifest which are taking the image(s) lock.
	treeStoreKeyLock, err := lock.ExclusiveKeyLock(ds.treeStoreLockDir, key)
	if err != nil {
		return fmt.Errorf("error locking tree store: %v", err)
	}
	defer treeStoreKeyLock.Close()

	if !rebuild {
		rendered, err := ds.treestore.IsRendered(key)
		if err != nil {
			return fmt.Errorf("cannot determine if tree is already rendered: %v", err)
		}
		if rendered {
			return nil
		}
	}
	// Firstly remove a possible partial treestore if existing.
	// This is needed as a previous ACI removal operation could have failed
	// cleaning the tree store leaving some stale files.
	err = ds.treestore.Remove(key)
	if err != nil {
		return err
	}
	err = ds.treestore.Write(key, &ds)
	if err != nil {
		return err
	}
	return nil
}

// CheckTreeStore verifies the treestore consistency for the specified key.
func (ds Store) CheckTreeStore(key string) error {
	treeStoreKeyLock, err := lock.SharedKeyLock(ds.treeStoreLockDir, key)
	if err != nil {
		return fmt.Errorf("error locking tree store: %v", err)
	}
	defer treeStoreKeyLock.Close()

	return ds.treestore.Check(key)
}

// GetTreeStorePath returns the absolute path of the treestore for the specified key.
// It doesn't ensure that the path exists and is fully rendered. This should
// be done calling IsRendered()
func (ds Store) GetTreeStorePath(key string) string {
	return ds.treestore.GetPath(key)
}

// GetTreeStoreRootFS returns the absolute path of the rootfs in the treestore
// for specified key.
// It doesn't ensure that the rootfs exists and is fully rendered. This should
// be done calling IsRendered()
func (ds Store) GetTreeStoreRootFS(key string) string {
	return ds.treestore.GetRootFS(key)
}

// GetRemote tries to retrieve a remote with the given ACIURL. found will be
// false if remote doesn't exist.
func (ds Store) GetRemote(aciURL string) (*Remote, bool, error) {
	var remote *Remote
	found := false
	err := ds.db.Do(func(tx *sql.Tx) error {
		var err error
		remote, found, err = GetRemote(tx, aciURL)
		return err
	})
	return remote, found, err
}

// WriteRemote adds or updates the provided Remote.
func (ds Store) WriteRemote(remote *Remote) error {
	err := ds.db.Do(func(tx *sql.Tx) error {
		return WriteRemote(tx, remote)
	})
	return err
}

// Get the ImageManifest with the specified key.
func (ds Store) GetImageManifest(key string) (*schema.ImageManifest, error) {
	key, err := ds.ResolveKey(key)
	if err != nil {
		return nil, fmt.Errorf("error resolving key: %v", err)
	}
	keyLock, err := lock.SharedKeyLock(ds.imageLockDir, key)
	if err != nil {
		return nil, fmt.Errorf("error locking image: %v", err)
	}
	defer keyLock.Close()

	imj, err := ds.stores[imageManifestType].Read(key)
	if err != nil {
		return nil, fmt.Errorf("error retrieving image manifest: %v", err)
	}
	var im *schema.ImageManifest
	if err = json.Unmarshal(imj, &im); err != nil {
		return nil, fmt.Errorf("error unmarshalling image manifest: %v", err)
	}
	return im, nil
}

// GetACI retrieves the ACI that best matches the provided app name and labels.
// The returned value is the blob store key of the retrieved ACI.
// If there are multiple matching ACIs choose the latest one (defined as the
// last one imported in the store).
// If no version label is requested, ACIs marked as latest in the ACIInfo are
// preferred.
func (ds Store) GetACI(name types.ACName, labels types.Labels) (string, error) {
	var curaciinfo *ACIInfo
	versionRequested := false
	if _, ok := labels.Get("version"); ok {
		versionRequested = true
	}

	var aciinfos []*ACIInfo
	err := ds.db.Do(func(tx *sql.Tx) error {
		var err error
		aciinfos, _, err = GetACIInfosWithAppName(tx, name.String())
		return err
	})
	if err != nil {
		return "", err
	}

nextKey:
	for _, aciinfo := range aciinfos {
		im, err := ds.GetImageManifest(aciinfo.BlobKey)
		if err != nil {
			return "", fmt.Errorf("error getting image manifest: %v", err)
		}

		// The image manifest must have all the requested labels
		for _, l := range labels {
			ok := false
			for _, rl := range im.Labels {
				if l.Name == rl.Name && l.Value == rl.Value {
					ok = true
					break
				}
			}
			if !ok {
				continue nextKey
			}
		}

		if curaciinfo != nil {
			// If no version is requested prefer the acis marked as latest
			if !versionRequested {
				if !curaciinfo.Latest && aciinfo.Latest {
					curaciinfo = aciinfo
					continue nextKey
				}
				if curaciinfo.Latest && !aciinfo.Latest {
					continue nextKey
				}
			}
			// If multiple matching image manifests are found, choose the latest imported in the cas.
			if aciinfo.ImportTime.After(curaciinfo.ImportTime) {
				curaciinfo = aciinfo
			}
		} else {
			curaciinfo = aciinfo
		}
	}

	if curaciinfo != nil {
		return curaciinfo.BlobKey, nil
	}
	return "", fmt.Errorf("aci not found")
}

func (ds Store) Dump(hex bool) {
	for _, s := range ds.stores {
		var keyCount int
		for key := range s.Keys(nil) {
			val, err := s.Read(key)
			if err != nil {
				panic(fmt.Sprintf("key %s had no value", key))
			}
			if len(val) > 128 {
				val = val[:128]
			}
			out := string(val)
			if hex {
				out = fmt.Sprintf("%x", val)
			}
			fmt.Printf("%s/%s: %s\n", s.BasePath, key, out)
			keyCount++
		}
		fmt.Printf("%d total keys\n", keyCount)
	}
}

// HashToKey takes a hash.Hash (which currently _MUST_ represent a full SHA512),
// calculates its sum, and returns a string which should be used as the key to
// store the data matching the hash.
func (ds Store) HashToKey(h hash.Hash) string {
	return hashToKey(h)
}

func hashToKey(h hash.Hash) string {
	s := h.Sum(nil)
	return keyToString(s)
}

// keyToString takes a key and returns a shortened and prefixed hexadecimal string version
func keyToString(k []byte) string {
	if len(k) != lenHash {
		panic(fmt.Sprintf("bad hash passed to hashToKey: %x", k))
	}
	return fmt.Sprintf("%s%x", hashPrefix, k)[0:lenKey]
}
