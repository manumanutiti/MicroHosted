package storage

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
)

// ErrNotWritten is a file InjectFiles could not place as asked: the image is
// out of space, or what is already in the image is in the way (a file where a
// directory must go). The request, not the host, is what has to change.
var ErrNotWritten = errors.New("file not written as asked")

// InjectSpec is one file InjectFiles writes into an image: its content, its
// permission bits (no setuid, setgid or sticky) and its owner inside the guest.
type InjectSpec struct {
	Path string
	Data []byte
	Mode os.FileMode
	UID  int
	GID  int
}

// InjectFiles writes files into the ext4 image at imagePath without mounting
// it, in one debugfs pass: parent directories are created as needed, an
// existing file at a path is replaced, and each file gets exactly its mode and
// owner. A second, read-only pass then checks every file is a regular file with
// that mode and owner, and reads it back to compare its bytes — debugfs exits 0
// when a write fails, and a write cut short by a full image still records the
// full size in the inode, so neither its exit code nor the size proves the
// content is there.
//
// The image must not be in use by a running VM. Contents are staged on the
// store (debugfs writes from host files) and removed before returning; error
// messages name paths, never contents.
func (o OfflineIO) InjectFiles(imagePath string, files []InjectSpec) error {
	if len(files) == 0 {
		return nil
	}
	var s debugfsScript
	var verify debugfsScript
	readBack := make([]string, len(files))
	for i := range files {
		f := &files[i]
		clean, err := cleanGuestPath(f.Path)
		if err != nil {
			return err
		}
		f.Path = clean
		if f.Mode&^os.ModePerm != 0 {
			return fmt.Errorf("file %s: mode %o has bits other than rwx permissions", clean, f.Mode)
		}

		tmp, err := o.stageFile("inject-*")
		if err != nil {
			return err
		}
		defer os.Remove(tmp.Name())
		_, werr := tmp.Write(f.Data)
		if cerr := tmp.Close(); werr == nil {
			werr = cerr
		}
		if werr != nil {
			return fmt.Errorf("staging %s: %w", clean, werr)
		}

		for _, dir := range parentDirs(clean) {
			s.add("mkdir", dir)
		}
		s.add("rm", clean)
		s.add("write", tmp.Name(), clean)
		// i_mode carries the file type too: 0100000 is a regular file.
		s.add("sif", clean, "mode", fmt.Sprintf("0%o", 0o100000|uint32(f.Mode)))
		s.add("sif", clean, "uid", strconv.Itoa(f.UID))
		s.add("sif", clean, "gid", strconv.Itoa(f.GID))
		verify.add("stat", clean)
		back, err := o.stageFile("verify-*")
		if err != nil {
			return err
		}
		_ = back.Close()
		defer os.Remove(back.Name())
		readBack[i] = back.Name()
		verify.add("dump", clean, back.Name())
	}
	script, err := s.script()
	if err != nil {
		return err
	}
	vscript, err := verify.script()
	if err != nil {
		return err
	}

	if _, err := o.runDebugfs(imagePath, true, script); err != nil {
		return err
	}
	out, err := o.runDebugfsStdout(imagePath, vscript)
	if err != nil {
		return err
	}
	got := parseStats(out)
	for i, f := range files {
		st, ok := got[f.Path]
		switch {
		case !ok || st.typ == "":
			return fmt.Errorf("%w: file %s was not written (no space in the image, or a parent is not a directory)", ErrNotWritten, f.Path)
		case st.typ != "regular":
			return fmt.Errorf("%w: file %s is a %s in the image, not a regular file", ErrNotWritten, f.Path, st.typ)
		case st.mode != uint32(f.Mode) || st.uid != f.UID || st.gid != f.GID:
			return fmt.Errorf("%w: file %s has mode %04o owner %d:%d, want %04o %d:%d", ErrNotWritten, f.Path, st.mode, st.uid, st.gid, uint32(f.Mode), f.UID, f.GID)
		case st.size != int64(len(f.Data)):
			return fmt.Errorf("%w: file %s has %d bytes in the image, want %d (out of space?)", ErrNotWritten, f.Path, st.size, len(f.Data))
		}
		back, err := os.ReadFile(readBack[i])
		if err != nil {
			return fmt.Errorf("reading back %s: %w", f.Path, err)
		}
		if !bytes.Equal(back, f.Data) {
			return fmt.Errorf("%w: file %s does not read back as written (out of space in the image?)", ErrNotWritten, f.Path)
		}
	}
	return nil
}

// inodeStat is what InjectFiles checks from a debugfs stat.
type inodeStat struct {
	typ      string
	mode     uint32
	uid, gid int
	size     int64
}

var (
	statHeaderRE = regexp.MustCompile(`^debugfs: stat "(.*)"$`)
	statTypeRE   = regexp.MustCompile(`^Inode: \d+\s+Type: (\S+)\s+Mode:\s+([0-7]+)`)
	statOwnerRE  = regexp.MustCompile(`^User:\s+(\d+)\s+Group:\s+(\d+).*\sSize: (\d+)`)
)

// parseStats reads the stdout of a debugfs script of `stat "path"` commands.
// debugfs -f echoes each command as "debugfs: <command>" before its output,
// which is what attributes each block to its path; a path that does not exist
// gets a header and no fields (its error goes to stderr).
//
// A stat block ends with what the guest wrote — xattr values, a symlink's
// target (printed raw, newlines and all) — so a line there can read like a
// header field, or even a command echo. The fields are the block's first lines,
// in the first block for a path: the type is taken only
// from the line right after the header, the owner and size only from the
// first "User:" line after it, and neither is ever overwritten.
func parseStats(out string) map[string]inodeStat {
	res := make(map[string]inodeStat)
	cur := ""
	n, owned := 0, false // lines into the current block; owner line seen
	for _, line := range strings.Split(out, "\n") {
		if m := statHeaderRE.FindStringSubmatch(line); m != nil {
			// The first block for a path is the real one; a later "header"
			// for it can only be guest text in a block's tail.
			if _, seen := res[m[1]]; seen {
				cur = ""
				continue
			}
			cur = m[1]
			res[cur] = inodeStat{}
			n, owned = 0, false
			continue
		}
		if strings.HasPrefix(line, "debugfs: ") {
			cur = "" // another command: its output belongs to no stat
			continue
		}
		if cur == "" {
			continue
		}
		n++
		st := res[cur]
		if n == 1 {
			if m := statTypeRE.FindStringSubmatch(line); m != nil {
				mode, _ := strconv.ParseUint(m[2], 8, 32)
				st.typ, st.mode = m[1], uint32(mode)
			}
		} else if m := statOwnerRE.FindStringSubmatch(line); m != nil && st.typ != "" && !owned {
			owned = true
			st.uid, _ = strconv.Atoi(m[1])
			st.gid, _ = strconv.Atoi(m[2])
			st.size, _ = strconv.ParseInt(m[3], 10, 64)
		}
		res[cur] = st
	}
	return res
}
