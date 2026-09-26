package cli

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"microhosted/pkg/types"
)

// fileFlags declares the --file and --secret flags of the commands that boot a
// VM from a template or image.
func fileFlags(fs *flagSet, files, secrets *[]string) {
	fs.listVar(files, "file", "f", "write local file into the disk before boot: `GUEST=LOCAL[,mode=0640][,uid=N][,gid=N]` (repeatable)")
	fs.listVar(secrets, "secret", "", "like --file, for a secret: mode 0400 by default, no hash kept (repeatable)")
}

// parseFileSpecs reads each --file/--secret's local file — with the caller's
// own permissions: the daemon never reads host paths on its behalf — into the
// FileSpecs a create or replace carries.
func parseFileSpecs(files, secrets []string) ([]types.FileSpec, error) {
	var specs []types.FileSpec
	for _, group := range []struct {
		list   []string
		secret bool
	}{{files, false}, {secrets, true}} {
		for _, arg := range group.list {
			f, err := parseFileSpec(arg)
			if err != nil {
				return nil, err
			}
			f.Secret = group.secret
			specs = append(specs, f)
		}
	}
	return specs, nil
}

func parseFileSpec(arg string) (types.FileSpec, error) {
	guest, rest, ok := strings.Cut(arg, "=")
	if !ok || guest == "" || rest == "" {
		return types.FileSpec{}, fmt.Errorf("file %q: want GUEST=LOCAL[,mode=0640][,uid=N][,gid=N]", arg)
	}
	parts := strings.Split(rest, ",")
	f := types.FileSpec{Path: guest}
	for _, opt := range parts[1:] {
		k, v, _ := strings.Cut(opt, "=")
		var err error
		switch k {
		case "mode":
			f.Mode = v
		case "uid":
			f.UID, err = strconv.Atoi(v)
		case "gid":
			f.GID, err = strconv.Atoi(v)
		default:
			return types.FileSpec{}, fmt.Errorf("file %q: unknown option %q (mode, uid, gid)", arg, k)
		}
		if err != nil {
			return types.FileSpec{}, fmt.Errorf("file %q: %s: %v", arg, k, err)
		}
	}
	data, err := os.ReadFile(parts[0])
	if err != nil {
		return types.FileSpec{}, err
	}
	f.Content = data
	return f, nil
}
