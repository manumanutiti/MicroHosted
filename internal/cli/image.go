package cli

import (
	"fmt"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"

	"microhosted/pkg/types"
)

var imageGroup = &group{
	name:    "image",
	aliases: []string{"img"},
	summary: "Manage the content-addressed image store",
	cmds: []*command{
		{name: "import", args: "NAME:VERSION", summary: "Copy a kernel and a rootfs into the store under their digest", help: imageImportHelp, run: imageImport},
		{name: "ls", aliases: []string{"list"}, summary: "List stored images", run: imageList},
		{name: "inspect", aliases: []string{"show"}, args: "IMAGE...", summary: "Show an image's full detail as JSON", run: imageInspect},
		{name: "verify", args: "IMAGE...", summary: "Re-hash an image's files and check they still match its digest", run: imageVerify},
		{name: "rm", aliases: []string{"remove", "delete"}, args: "IMAGE...", summary: "Delete images no VM or snapshot uses", run: imageRemove},
	},
}

const imageImportHelp = `The files must be on the daemon's host, under its store directory (where
make prepare-image builds them). They are copied once and hashed; the tag then
names those bytes forever: a rebuild needs a new version. Create VMs with
  mh run NAME:VERSION        or, pinned,   mh run NAME:VERSION@sha256:…`

func imageImport(e *env, cmd *command, p string, args []string) error {
	var req types.ImportImageRequest
	fs := newCmdFlags(e, p, cmd)
	fs.stringVar(&req.KernelPath, "kernel", "k", "", "kernel `FILE` (e.g. /var/lib/microhosted/store/kernels/vmlinux-6.1.102)")
	fs.stringVar(&req.RootfsPath, "rootfs", "r", "", "root filesystem `FILE` (ext4)")
	fs.int64Var(&req.VCPUs, "cpus", "c", "default vCPUs of its VMs (default 1)")
	fs.Var((*mbValue)(&req.MemMB), "mem", "default memory `SIZE` of its VMs, e.g. 128 or 1G (default 128)")
	fs.Var((*mbValue)(&req.DiskMB), "disk", "default disk `SIZE` its VMs are grown to, e.g. 512 (default: the rootfs's)")
	pos, err := fs.parse(args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usagef(p, "expected exactly one NAME:VERSION")
	}
	if req.KernelPath == "" || req.RootfsPath == "" {
		return usagef(p, "--kernel and --rootfs are required")
	}
	req.Name = pos[0]
	if req.VCPUs == 0 {
		req.VCPUs = 1
	}
	if req.MemMB == 0 {
		req.MemMB = 128
	}
	// The daemon reads the paths on its host; relative ones are the caller's.
	for _, p := range []*string{&req.KernelPath, &req.RootfsPath} {
		if abs, err := filepath.Abs(*p); err == nil {
			*p = abs
		}
	}
	c, err := e.api()
	if err != nil {
		return err
	}
	var img types.ImageResponse
	if err := c.Do("POST", "/v1/images", req, &img); err != nil {
		return err
	}
	fmt.Fprintln(e.stdout, req.Name+"@"+img.Digest)
	return nil
}

func imageList(e *env, cmd *command, p string, args []string) error {
	var quiet, asJSON bool
	fs := newCmdFlags(e, p, cmd)
	fs.boolVar(&quiet, "quiet", "q", "print pinned references only (NAME:VERSION@DIGEST)")
	fs.boolVar(&asJSON, "json", "", "print the API's JSON")
	pos, err := fs.parse(args)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return usagef(p, "unexpected argument %q", pos[0])
	}
	c, err := e.api()
	if err != nil {
		return err
	}
	var imgs []types.ImageResponse
	if err := c.Do("GET", "/v1/images", nil, &imgs); err != nil {
		return err
	}
	switch {
	case asJSON:
		return printJSON(e.stdout, imgs)
	case quiet:
		for _, img := range imgs {
			for _, t := range img.Tags {
				fmt.Fprintln(e.stdout, t+"@"+img.Digest)
			}
		}
		return nil
	}
	rows := make([][]string, 0, len(imgs))
	for _, img := range imgs {
		status := "ready"
		if img.Missing != "" {
			status = "MISSING FILE"
		}
		disk := "-"
		if img.DiskMB > 0 {
			disk = fmtMB(img.DiskMB)
		}
		rows = append(rows, []string{
			strings.Join(img.Tags, ","), shortDigest(img.Digest), strconv.FormatInt(img.VCPUs, 10),
			fmtMB(img.MemMB), disk, fmtMB(img.SizeMB), status, fmtAgo(img.ImportedAt),
		})
	}
	table(e.stdout, []string{"IMAGE", "DIGEST", "VCPU", "MEM", "DISK", "SIZE", "STATUS", "IMPORTED"}, rows)
	return nil
}

// shortDigest is the first 12 hex characters, as docker prints them. Enough
// to tell images apart on screen; references always carry the full digest.
func shortDigest(d string) string {
	hex := strings.TrimPrefix(d, "sha256:")
	if len(hex) > 12 {
		hex = hex[:12]
	}
	return hex
}

func getImage(c *Client, ref string) (types.ImageResponse, error) {
	var img types.ImageResponse
	return img, c.Do("GET", "/v1/images/"+url.PathEscape(ref), nil, &img)
}

func imageInspect(e *env, cmd *command, p string, args []string) error {
	fs := newCmdFlags(e, p, cmd)
	pos, err := fs.parse(args)
	if err != nil {
		return err
	}
	if len(pos) == 0 {
		return usagef(p, "expected at least one IMAGE")
	}
	c, err := e.api()
	if err != nil {
		return err
	}
	var out []types.ImageResponse
	for _, ref := range pos {
		img, err := getImage(c, ref)
		if err != nil {
			return err
		}
		out = append(out, img)
	}
	return printInspect(e.stdout, out)
}

func imageVerify(e *env, cmd *command, p string, args []string) error {
	fs := newCmdFlags(e, p, cmd)
	pos, err := fs.parse(args)
	if err != nil {
		return err
	}
	if len(pos) == 0 {
		return usagef(p, "expected at least one IMAGE")
	}
	c, err := e.api()
	if err != nil {
		return err
	}
	failed := 0
	for _, ref := range pos {
		var res types.ImageVerifyResponse
		if err := c.Do("POST", "/v1/images/"+url.PathEscape(ref)+"/verify", nil, &res); err != nil {
			return err
		}
		if res.OK {
			fmt.Fprintf(e.stdout, "%s: ok\n", ref)
		} else {
			failed++
			fmt.Fprintf(e.stdout, "%s: FAILED: %s\n", ref, res.Error)
		}
	}
	if failed > 0 {
		return exitError{code: 1}
	}
	return nil
}

func imageRemove(e *env, cmd *command, p string, args []string) error {
	fs := newCmdFlags(e, p, cmd)
	pos, err := fs.parse(args)
	if err != nil {
		return err
	}
	if len(pos) == 0 {
		return usagef(p, "expected at least one IMAGE")
	}
	c, err := e.api()
	if err != nil {
		return err
	}
	for _, ref := range pos {
		if err := c.Do("DELETE", "/v1/images/"+url.PathEscape(ref), nil, nil); err != nil {
			return err
		}
		fmt.Fprintln(e.stdout, ref)
	}
	return nil
}
