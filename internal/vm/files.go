package vm

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"regexp"
	"strconv"

	"microhosted/internal/storage"
	"microhosted/pkg/types"
)

// Files written into a new VM's disk before its first boot (FileSpec) are
// configuration, not data: small, few, and carried inside the API request.
// Anything larger belongs in the image or on a volume.
const (
	maxFiles      = 64
	maxFilesBytes = 512 << 10 // all contents together: 683 KiB of base64, inside the API's 1 MiB body cap
	maxGuestID    = 1<<32 - 2 // (uid_t)-1 means "no change" to chown, never an owner
)

var modeRE = regexp.MustCompile(`^0?[0-7]{3}$`)

// prepareFiles validates specs and turns them into what storage writes and
// what the VM's record keeps. Every problem is ErrInvalid, reported before
// anything is touched; messages name paths, never contents.
func prepareFiles(specs []types.FileSpec) ([]storage.InjectSpec, []types.InjectedFile, error) {
	if len(specs) == 0 {
		return nil, nil, nil
	}
	if len(specs) > maxFiles {
		return nil, nil, fmt.Errorf("%w: %d files, at most %d", ErrInvalid, len(specs), maxFiles)
	}
	inject := make([]storage.InjectSpec, 0, len(specs))
	recs := make([]types.InjectedFile, 0, len(specs))
	seen := make(map[string]bool, len(specs))
	var total int
	for _, f := range specs {
		p, err := storage.ValidateGuestPath(f.Path)
		if err != nil {
			return nil, nil, fmt.Errorf("%w: file: %v", ErrInvalid, err)
		}
		if seen[p] {
			return nil, nil, fmt.Errorf("%w: file %s given twice", ErrInvalid, p)
		}
		seen[p] = true

		mode := f.Mode
		if mode == "" {
			mode = "0644"
			if f.Secret {
				mode = "0400"
			}
		}
		if !modeRE.MatchString(mode) {
			return nil, nil, fmt.Errorf("%w: file %s: mode %q: want octal permission bits such as 0640 (no setuid, setgid or sticky)", ErrInvalid, p, f.Mode)
		}
		bits, _ := strconv.ParseUint(mode, 8, 32)
		if f.UID < 0 || f.UID > maxGuestID || f.GID < 0 || f.GID > maxGuestID {
			return nil, nil, fmt.Errorf("%w: file %s: owner %d:%d out of range", ErrInvalid, p, f.UID, f.GID)
		}
		total += len(f.Content)
		if total > maxFilesBytes {
			return nil, nil, fmt.Errorf("%w: files add up to more than %d KiB; larger data belongs in the image or a volume", ErrInvalid, maxFilesBytes>>10)
		}

		inject = append(inject, storage.InjectSpec{Path: p, Data: f.Content, Mode: os.FileMode(bits), UID: f.UID, GID: f.GID})
		rec := types.InjectedFile{Path: p, Mode: fmt.Sprintf("%04o", bits), UID: f.UID, GID: f.GID, Size: int64(len(f.Content)), Secret: f.Secret}
		if !f.Secret {
			sum := sha256.Sum256(f.Content)
			rec.SHA256 = hex.EncodeToString(sum[:])
		}
		recs = append(recs, rec)
	}
	return inject, recs, nil
}
