package cli

import (
	"fmt"
	"sort"

	"microhosted/pkg/types"
)

var volumeGroup = &group{
	name:    "volume",
	aliases: []string{"vol", "volumes"},
	summary: "Manage persistent volumes (disks that outlive VMs)",
	cmds: []*command{
		{name: "create", aliases: []string{"new"}, args: "NAME --size SIZE", summary: "Create a volume", run: volCreate},
		{name: "ls", aliases: []string{"list"}, summary: "List volumes", run: volList},
		{name: "inspect", aliases: []string{"show"}, args: "VOLUME...", summary: "Show a volume's detail as JSON", run: volInspect},
		{name: "rm", aliases: []string{"remove", "delete"}, args: "VOLUME...", summary: "Delete detached volumes", run: volRemove},
		{name: "cp", args: "VOLUME:PATH LOCAL | LOCAL VOLUME:PATH", summary: "Copy a file in/out of a DETACHED volume (offline)", run: volCopy},
	},
}

func volCreate(e *env, cmd *command, p string, args []string) error {
	var req types.CreateVolumeRequest
	fs := newCmdFlags(e, p, cmd)
	fs.Var((*mbValue)(&req.SizeMB), "size", "`SIZE` of the ext4 disk, e.g. 512 (MiB) or 2G — required")
	fs.alias("size", "s")
	pos, err := fs.parse(args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usagef(p, "expected exactly one NAME")
	}
	if req.SizeMB <= 0 {
		return usagef(p, "--size is required")
	}
	req.Name = pos[0]
	c, err := e.api()
	if err != nil {
		return err
	}
	var v types.VolumeResponse
	if err := c.Do("POST", "/v1/volumes", req, &v); err != nil {
		return err
	}
	fmt.Fprintln(e.stdout, v.ID)
	return nil
}

func volList(e *env, cmd *command, p string, args []string) error {
	var quiet, asJSON bool
	fs := newCmdFlags(e, p, cmd)
	fs.boolVar(&quiet, "quiet", "q", "print IDs only")
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
	vols, err := c.listVolumes()
	if err != nil {
		return err
	}
	sort.Slice(vols, func(i, j int) bool { return vols[i].Name < vols[j].Name })
	switch {
	case asJSON:
		return printJSON(e.stdout, vols)
	case quiet:
		for _, v := range vols {
			fmt.Fprintln(e.stdout, v.ID)
		}
		return nil
	}
	rows := make([][]string, 0, len(vols))
	for _, v := range vols {
		rows = append(rows, []string{v.ID, v.Name, fmtMB(v.SizeMB), orDash(v.AttachedTo), fmtAgo(v.CreatedAt)})
	}
	table(e.stdout, []string{"VOLUME ID", "NAME", "SIZE", "ATTACHED TO", "CREATED"}, rows)
	return nil
}

func volInspect(e *env, cmd *command, p string, args []string) error {
	fs := newCmdFlags(e, p, cmd)
	pos, err := fs.parse(args)
	if err != nil {
		return err
	}
	if len(pos) == 0 {
		return usagef(p, "expected at least one VOLUME")
	}
	c, err := e.api()
	if err != nil {
		return err
	}
	var out []types.VolumeResponse
	for _, ref := range pos {
		id, err := c.resolveVolume(ref)
		if err != nil {
			return err
		}
		var v types.VolumeResponse
		if err := c.Do("GET", "/v1/volumes/"+id, nil, &v); err != nil {
			return err
		}
		out = append(out, v)
	}
	return printInspect(e.stdout, out)
}

func volRemove(e *env, cmd *command, p string, args []string) error {
	fs := newCmdFlags(e, p, cmd)
	pos, err := fs.parse(args)
	if err != nil {
		return err
	}
	if len(pos) == 0 {
		return usagef(p, "expected at least one VOLUME")
	}
	c, err := e.api()
	if err != nil {
		return err
	}
	return eachArg(e, pos, func(ref string) error {
		id, err := c.resolveVolume(ref)
		if err != nil {
			return err
		}
		if err := c.Do("DELETE", "/v1/volumes/"+id, nil, nil); err != nil {
			return err
		}
		fmt.Fprintln(e.stdout, id)
		return nil
	})
}

func volCopy(e *env, cmd *command, p string, args []string) error {
	fs := newCmdFlags(e, p, cmd)
	pos, err := fs.parse(args)
	if err != nil {
		return err
	}
	if len(pos) != 2 {
		return usagef(p, "expected SOURCE and DESTINATION")
	}
	c, err := e.api()
	if err != nil {
		return err
	}
	return copyFiles(e, p, pos[0], pos[1], "VOLUME", func(ref string) (string, error) {
		id, err := c.resolveVolume(ref)
		return "/v1/volumes/" + id + "/files", err
	})
}
