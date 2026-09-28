package build

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Fingerprint identifies what a build of s would take in: the spec as parsed
// (comments and formatting do not count; its name does not either) and every
// file its sources name — paths, modes, link targets and contents — for arch.
// Two builds with the same fingerprint start from the same inputs; mh build
// tags an image with it (sha-<first 12 hex>) and skips a build whose tag the
// store already holds, as Docker's build cache skips an unchanged step.
//
// What it cannot see is what the network serves: a package without a pinned
// version is whatever the repository has at build time. Rebuild with
// --no-cache to pick up newer packages.
func Fingerprint(s *Spec, contextDir, arch string) (string, error) {
	sources, err := resolveSources(contextDir, s)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	spec := *s
	spec.Name = ""
	b, err := json.Marshal(struct {
		Arch string
		Spec Spec
	}{arch, spec})
	if err != nil {
		return "", err
	}
	h.Write(b)
	for _, guest := range s.FileOrder {
		fmt.Fprintf(h, "\x00file %s\x00", guest)
		if err := hashTree(h, sources[guest].path); err != nil {
			return "", fmt.Errorf("files[%s]: %w", guest, err)
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// hashTree writes root's entries to h in lexical order: relative path, mode,
// and a link's target or a file's SHA-256.
func hashTree(h io.Writer, root string) error {
	return filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		fmt.Fprintf(h, "%s\x00%o\x00", rel, fi.Mode())
		switch {
		case fi.Mode()&fs.ModeSymlink != 0:
			target, err := os.Readlink(p)
			if err != nil {
				return err
			}
			fmt.Fprintf(h, "%s\x00", target)
		case fi.Mode().IsRegular():
			f, err := os.Open(p)
			if err != nil {
				return err
			}
			sum := sha256.New()
			_, err = io.Copy(sum, f)
			f.Close()
			if err != nil {
				return err
			}
			fmt.Fprintf(h, "%x\x00", sum.Sum(nil))
		}
		return nil
	})
}

// CacheVersion is the tag version a fingerprint gives.
func CacheVersion(fingerprint string) string { return "sha-" + fingerprint[:12] }

var nonName = regexp.MustCompile(`[^a-z0-9-]+`)

// DefaultName is the image name for a build context directory that the spec
// names none for: the directory's name, made a valid image name.
func DefaultName(contextDir string) string {
	abs, err := filepath.Abs(contextDir)
	if err != nil {
		abs = contextDir
	}
	n := strings.Trim(nonName.ReplaceAllString(strings.ToLower(filepath.Base(abs)), "-"), "-")
	if len(n) > 63 {
		n = strings.Trim(n[:63], "-")
	}
	if n == "" {
		n = "image"
	}
	return n
}
