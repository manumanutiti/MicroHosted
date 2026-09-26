// Package images is the daemon's content-addressed image store: kernels and
// root filesystems kept read-only under their sha256, and the images — a
// kernel, a rootfs and VM defaults — that name them by digest.
//
// Hashing happens once, at import, over the store's own copy of each file
// (never the caller's, which could change between the hash and the copy).
// Booting from an image is a map lookup to a path: no file is read or hashed
// on the create path, so an image boots exactly as fast as a catalog
// template. What keeps the bytes equal to their digest afterwards is that
// nothing can write them: files are 0444 in a root-only directory, the
// daemon never opens them for writing, and a new build is a new digest.
package images

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"microhosted/internal/labels"
	"microhosted/internal/storage"
	"microhosted/pkg/types"
)

// Errors the API maps to a status of its own: ErrNotFound 404, ErrConflict
// 409 (a tag bound to another image, an image still in use). Malformed input
// is labels.ErrInvalid (400).
var (
	ErrNotFound = errors.New("image not found")
	ErrConflict = errors.New("image conflict")
)

const (
	manifestSchema = 1
	digestPrefix   = "sha256:"
	// Dir is the store's directory under the daemon's instances dir. It must
	// be on the same filesystem as the VM clones (reflink) and the jailer
	// chroot (the kernel is hard-linked into it).
	Dir = "images"

	maxVersion = 63
	maxVCPUs   = 32
	minMemMB   = 32
)

var (
	versionRE = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9._-]*[A-Za-z0-9])?$`)
	hexRE     = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// Persister is the slice of the state database the store uses.
type Persister interface {
	SaveImage(*types.Image) error
	DeleteImage(digest string) error
	ListImages() ([]*types.Image, error)
}

// Store holds the images. Safe for concurrent use; imports run one at a time.
type Store struct {
	dir string // <instances-dir>/images
	// root is the only tree files are imported from: the daemon's instances
	// dir, where goldens are built by root. Reading any path the caller
	// names would let whoever reaches the API copy any file root can read
	// (/etc/shadow) into a VM and read it back.
	root string
	db   Persister

	importMu sync.Mutex // one import at a time: each copies and hashes GBs

	// reserve, when set, admits an import against the store's free-space
	// reserve before it copies anything (see SetReserve).
	reserve func(what string, sizeMB int64) (release func(), err error)

	mu     sync.RWMutex
	images map[string]*types.Image // by digest
	tags   map[string]string       // "name:version" → digest
}

// SetReserve installs the store's free-space admission: reserve is asked for
// the size of an import's files before any is copied, and its error returned
// as is. Call once, before serving.
func (s *Store) SetReserve(reserve func(what string, sizeMB int64) (release func(), err error)) {
	s.reserve = reserve
}

// fileMB is the apparent size of path in MB, rounded up; 0 if unreadable (the
// copy then fails on its own).
func fileMB(path string) int64 {
	fi, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return (fi.Size() + 1<<20 - 1) >> 20
}

// Open loads the store rooted at instancesDir/images, creating its
// directories root-only. Records whose files are gone stay listed (reported
// as missing) rather than dropped: a VM or snapshot may still name them.
func Open(instancesDir string, db Persister) (*Store, error) {
	root, err := filepath.EvalSymlinks(instancesDir)
	if err != nil {
		return nil, fmt.Errorf("image store root %s: %w", instancesDir, err)
	}
	s := &Store{
		root:   root,
		dir:    filepath.Join(root, Dir),
		db:     db,
		images: make(map[string]*types.Image),
		tags:   make(map[string]string),
	}
	for _, d := range []string{s.dir, s.blobDir(), s.tmpDir()} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return nil, fmt.Errorf("creating image store %s: %w", d, err)
		}
		// MkdirAll leaves an existing directory's mode alone; this one must
		// be root-only whatever created it.
		if err := os.Chmod(d, 0o700); err != nil {
			return nil, fmt.Errorf("securing image store %s: %w", d, err)
		}
	}
	// Leftovers of an import the daemon died in the middle of.
	if entries, err := os.ReadDir(s.tmpDir()); err == nil {
		for _, e := range entries {
			_ = os.Remove(filepath.Join(s.tmpDir(), e.Name()))
		}
	}
	recs, err := db.ListImages()
	if err != nil {
		return nil, err
	}
	for _, img := range recs {
		s.images[img.Digest] = img
		for _, t := range img.Tags {
			s.tags[t] = img.Digest
		}
	}
	return s, nil
}

func (s *Store) blobDir() string { return filepath.Join(s.dir, "sha256") }
func (s *Store) tmpDir() string  { return filepath.Join(s.dir, "tmp") }

// BlobPath is where the file with digest d lives.
func (s *Store) BlobPath(d string) string {
	return filepath.Join(s.blobDir(), strings.TrimPrefix(d, digestPrefix))
}

// ParseTag checks a "name:version" reference: name a DNS label, version
// letters, digits, '.', '_' and '-'.
func ParseTag(tag string) (name, version string, err error) {
	name, version, ok := strings.Cut(tag, ":")
	if !ok || version == "" {
		return "", "", fmt.Errorf("%w: image %q: want name:version", labels.ErrInvalid, tag)
	}
	if err := labels.ValidateName("image name", name); err != nil {
		return "", "", err
	}
	if len(version) > maxVersion || !versionRE.MatchString(version) {
		return "", "", fmt.Errorf("%w: image %q: version: letters, digits, '.', '_' and '-', starting and ending with a letter or digit, at most %d characters", labels.ErrInvalid, tag, maxVersion)
	}
	return name, version, nil
}

// IsRef reports whether s has the shape of an image reference rather than a
// catalog template name: a digest, or anything with a ':'.
func IsRef(s string) bool {
	return strings.Contains(s, ":")
}

func validDigest(d string) bool {
	return strings.HasPrefix(d, digestPrefix) && hexRE.MatchString(strings.TrimPrefix(d, digestPrefix))
}

// Import copies req's kernel and rootfs into the store, hashes the copies,
// and records the image under req.Name. Importing the same files with the
// same defaults under the same tag again is a no-op that returns the image;
// a tag already bound to a different image is ErrConflict — tags never move.
func (s *Store) Import(req types.ImportImageRequest) (*types.Image, error) {
	if _, _, err := ParseTag(req.Name); err != nil {
		return nil, err
	}
	if req.VCPUs < 1 || req.VCPUs > maxVCPUs {
		return nil, fmt.Errorf("%w: vcpus %d: between 1 and %d", labels.ErrInvalid, req.VCPUs, maxVCPUs)
	}
	if req.MemMB < minMemMB {
		return nil, fmt.Errorf("%w: mem_mb %d: at least %d", labels.ErrInvalid, req.MemMB, minMemMB)
	}
	if req.DiskMB < 0 {
		return nil, fmt.Errorf("%w: disk_mb %d is negative", labels.ErrInvalid, req.DiskMB)
	}
	kernelSrc, err := s.checkSource(req.KernelPath)
	if err != nil {
		return nil, fmt.Errorf("%w: kernel_path: %v", labels.ErrInvalid, err)
	}
	rootfsSrc, err := s.checkSource(req.RootfsPath)
	if err != nil {
		return nil, fmt.Errorf("%w: rootfs_path: %v", labels.ErrInvalid, err)
	}

	s.importMu.Lock()
	defer s.importMu.Unlock()

	if s.reserve != nil {
		release, err := s.reserve("importing image "+req.Name, fileMB(kernelSrc)+fileMB(rootfsSrc))
		if err != nil {
			return nil, err
		}
		defer release()
	}

	// Refuse a taken tag before copying gigabytes; re-checked below against
	// the digest, which only the copy can tell.
	s.mu.RLock()
	bound, taken := s.tags[req.Name]
	s.mu.RUnlock()

	// Any failure from here on leaves no file behind that no image names.
	done := false
	defer func() {
		if !done {
			s.gcBlobs()
		}
	}()
	kernel, err := s.ingest(kernelSrc)
	if err != nil {
		return nil, fmt.Errorf("importing kernel: %w", err)
	}
	rootfs, err := s.ingest(rootfsSrc)
	if err != nil {
		return nil, fmt.Errorf("importing rootfs: %w", err)
	}
	m := types.ImageManifest{
		Schema: manifestSchema, Kernel: kernel, Rootfs: rootfs,
		VCPUs: req.VCPUs, MemMB: req.MemMB, DiskMB: req.DiskMB,
	}
	digest, err := manifestDigest(m)
	if err != nil {
		return nil, err
	}
	if taken && bound != digest {
		return nil, fmt.Errorf("%w: tag %s is bound to %s; a different build needs a new version", ErrConflict, req.Name, bound)
	}

	s.mu.Lock()
	img, exists := s.images[digest]
	var next types.Image
	if exists {
		next = *img
		if slices.Contains(next.Tags, req.Name) {
			s.mu.Unlock()
			done = true
			return img, nil
		}
		next.Tags = append(slices.Clone(next.Tags), req.Name)
		sort.Strings(next.Tags)
	} else {
		next = types.Image{Digest: digest, Manifest: m, Tags: []string{req.Name}, ImportedAt: time.Now()}
	}
	if err := s.db.SaveImage(&next); err != nil {
		s.mu.Unlock()
		return nil, fmt.Errorf("recording image %s: %w", digest, err)
	}
	s.images[digest] = &next
	s.tags[req.Name] = digest
	s.mu.Unlock()
	done = true
	return &next, nil
}

// checkSource resolves p and returns the real path to copy from. It must be a
// regular, non-empty file (a device would be copied whole) inside the
// daemon's instances dir and outside the image store itself. The resolved
// path is what gets copied, so a symlink changed after this check changes
// nothing — and the tree is root's, so only root could change one.
func (s *Store) checkSource(p string) (string, error) {
	if p == "" || !filepath.IsAbs(p) {
		return "", fmt.Errorf("%q: want an absolute path", p)
	}
	real, err := filepath.EvalSymlinks(p)
	if err != nil {
		return "", err
	}
	if !within(real, s.root) || within(real, s.dir) {
		return "", fmt.Errorf("%s: images are imported from files under %s (outside %s); build or copy it there first", p, s.root, s.dir)
	}
	fi, err := os.Stat(real)
	if err != nil {
		return "", err
	}
	if !fi.Mode().IsRegular() {
		return "", fmt.Errorf("%s is not a regular file", p)
	}
	if fi.Size() == 0 {
		return "", fmt.Errorf("%s is empty", p)
	}
	return real, nil
}

// within reports whether p is dir or below it (both clean, absolute).
func within(p, dir string) bool {
	rel, err := filepath.Rel(dir, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, "../") && !filepath.IsAbs(rel)
}

// reflink copies a file into the store (copy-on-write where the filesystem
// can); a variable so tests can make it fail.
var reflink = storage.ReflinkFile

// ingest copies src into the store and returns its digest. The hash is taken
// over the store's private copy, after the copy is complete, so the digest
// describes exactly the bytes that will boot whatever happens to src.
func (s *Store) ingest(src string) (string, error) {
	f, err := os.CreateTemp(s.tmpDir(), "import-*")
	if err != nil {
		return "", err
	}
	tmp := f.Name()
	_ = f.Close()
	keep := false
	defer func() {
		if !keep {
			_ = os.Remove(tmp)
		}
	}()
	if err := reflink(src, tmp); err != nil {
		return "", err
	}
	sum, err := hashFile(tmp)
	if err != nil {
		return "", err
	}
	digest := digestPrefix + sum
	dst := s.BlobPath(digest)
	if _, err := os.Stat(dst); err == nil {
		return digest, nil // already stored: the copy is dropped
	}
	if err := os.Chmod(tmp, 0o444); err != nil {
		return "", err
	}
	if err := syncFile(tmp); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, dst); err != nil {
		return "", err
	}
	keep = true
	return digest, nil
}

func hashFile(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func syncFile(p string) error {
	f, err := os.Open(p)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

func manifestDigest(m types.ImageManifest) (string, error) {
	b, err := json.Marshal(m)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return digestPrefix + hex.EncodeToString(sum[:]), nil
}

// Resolve finds the image a reference names: "sha256:<hex>", "name:version",
// or "name:version@sha256:<hex>" — which must agree, or it is refused: a
// reference whose tag and digest disagree is a mistake, never a choice.
func (s *Store) Resolve(ref string) (*types.Image, error) {
	tag, digest, pinned := strings.Cut(ref, "@")
	if !pinned && strings.HasPrefix(ref, digestPrefix) {
		tag, digest = "", ref
	}
	if digest != "" && !validDigest(digest) {
		return nil, fmt.Errorf("%w: image %q: a digest is sha256: and 64 lowercase hex characters", labels.ErrInvalid, ref)
	}
	if tag != "" {
		if _, _, err := ParseTag(tag); err != nil {
			return nil, err
		}
	}

	s.mu.RLock()
	defer s.mu.RUnlock()
	if tag != "" {
		bound, ok := s.tags[tag]
		if !ok {
			return nil, fmt.Errorf("%w: %s", ErrNotFound, tag)
		}
		if digest != "" && digest != bound {
			return nil, fmt.Errorf("%w: %s is %s, not %s", ErrConflict, tag, bound, digest)
		}
		digest = bound
	}
	img, ok := s.images[digest]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, digest)
	}
	return img, nil
}

// Template renders img as the template the VM manager boots from: the same
// structure a catalog entry has, with the store's paths. Everything past the
// lookup — clone, pre-grown copies, kernel hard-link — is then unchanged.
func (s *Store) Template(img *types.Image) types.Template {
	name := img.Digest
	if len(img.Tags) > 0 {
		name = img.Tags[0]
	}
	return types.Template{
		Name:       name,
		KernelPath: s.BlobPath(img.Manifest.Kernel),
		RootfsPath: s.BlobPath(img.Manifest.Rootfs),
		VCPUs:      img.Manifest.VCPUs,
		MemMB:      img.Manifest.MemMB,
		DiskMB:     img.Manifest.DiskMB,
	}
}

// Missing names the first file of img not in the store, or "".
func (s *Store) Missing(img *types.Image) string {
	for _, d := range []string{img.Manifest.Kernel, img.Manifest.Rootfs} {
		if _, err := os.Stat(s.BlobPath(d)); err != nil {
			return d
		}
	}
	return ""
}

// List returns every image, sorted by first tag.
func (s *Store) List() []*types.Image {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*types.Image, 0, len(s.images))
	for _, img := range s.images {
		out = append(out, img)
	}
	sort.Slice(out, func(i, j int) bool { return firstTag(out[i]) < firstTag(out[j]) })
	return out
}

func firstTag(img *types.Image) string {
	if len(img.Tags) > 0 {
		return img.Tags[0]
	}
	return img.Digest
}

// Delete removes the image ref names, with all its tags, and every stored
// file no remaining image needs. inUse reports who still boots from a digest
// (a VM, a snapshot); anything it names makes the delete ErrConflict.
func (s *Store) Delete(ref string, inUse func(digest string) string) (*types.Image, error) {
	s.importMu.Lock()
	defer s.importMu.Unlock()
	img, err := s.Resolve(ref)
	if err != nil {
		return nil, err
	}
	if user := inUse(img.Digest); user != "" {
		return nil, fmt.Errorf("%w: %s is used by %s", ErrConflict, firstTag(img), user)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.db.DeleteImage(img.Digest); err != nil {
		return nil, err
	}
	delete(s.images, img.Digest)
	for _, t := range img.Tags {
		delete(s.tags, t)
	}
	s.gcBlobsLocked()
	return img, nil
}

// gcBlobs removes stored files no image names (a failed import's, a deleted
// image's). Callers hold importMu, so no import is between ingest and record.
func (s *Store) gcBlobs() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gcBlobsLocked()
}

func (s *Store) gcBlobsLocked() {
	needed := make(map[string]bool)
	for _, img := range s.images {
		needed[strings.TrimPrefix(img.Manifest.Kernel, digestPrefix)] = true
		needed[strings.TrimPrefix(img.Manifest.Rootfs, digestPrefix)] = true
	}
	entries, err := os.ReadDir(s.blobDir())
	if err != nil {
		return
	}
	for _, e := range entries {
		if !needed[e.Name()] {
			_ = os.Remove(filepath.Join(s.blobDir(), e.Name()))
		}
	}
}

// Response renders img for the API.
func (s *Store) Response(img *types.Image) types.ImageResponse {
	var size int64
	for _, d := range []string{img.Manifest.Kernel, img.Manifest.Rootfs} {
		if fi, err := os.Stat(s.BlobPath(d)); err == nil {
			size += fi.Size()
		}
	}
	return types.ImageResponse{
		Digest:     img.Digest,
		Tags:       img.Tags,
		Kernel:     img.Manifest.Kernel,
		Rootfs:     img.Manifest.Rootfs,
		VCPUs:      img.Manifest.VCPUs,
		MemMB:      img.Manifest.MemMB,
		DiskMB:     img.Manifest.DiskMB,
		SizeMB:     (size + (1<<20 - 1)) >> 20,
		ImportedAt: img.ImportedAt.Format(time.RFC3339),
		Missing:    s.Missing(img),
	}
}

// Integrity lists what is wrong with the stored files, for the doctor: a file
// missing, or writable by anyone — the store never leaves one so, so either is
// a sign of tampering or of a hand-made change. Cheap (stat only); it does not
// re-hash (see Verify).
func (s *Store) Integrity() []types.DoctorFinding {
	var out []types.DoctorFinding
	if fi, err := os.Stat(s.dir); err == nil && fi.Mode().Perm()&0o077 != 0 {
		out = append(out, types.DoctorFinding{Kind: "image_store_open", Object: s.dir,
			Detail: fmt.Sprintf("the image store is accessible to non-root users (mode %v), want 0700", fi.Mode().Perm())})
	}
	for _, img := range s.List() {
		for _, d := range []string{img.Manifest.Kernel, img.Manifest.Rootfs} {
			fi, err := os.Stat(s.BlobPath(d))
			if err != nil {
				out = append(out, types.DoctorFinding{Kind: "image_file_missing", Object: firstTag(img),
					Detail: fmt.Sprintf("file %s is gone from the store; the image cannot boot", d)})
				continue
			}
			if fi.Mode().Perm()&0o222 != 0 {
				out = append(out, types.DoctorFinding{Kind: "image_file_writable", Object: firstTag(img),
					Detail: fmt.Sprintf("file %s is writable (mode %v): its content may no longer match its digest; run mh image verify", d, fi.Mode().Perm())})
			}
		}
	}
	return out
}

// Verify re-hashes img's files and reports any that no longer match their
// digest. Slow (reads every byte); never on the boot path.
func (s *Store) Verify(img *types.Image) error {
	var bad []string
	for _, d := range []string{img.Manifest.Kernel, img.Manifest.Rootfs} {
		sum, err := hashFile(s.BlobPath(d))
		if err != nil {
			bad = append(bad, fmt.Sprintf("%s: %v", d, err))
		} else if digestPrefix+sum != d {
			bad = append(bad, fmt.Sprintf("%s: content now hashes to sha256:%s", d, sum))
		}
	}
	if len(bad) > 0 {
		return fmt.Errorf("image %s failed verification: %s", firstTag(img), strings.Join(bad, "; "))
	}
	return nil
}
