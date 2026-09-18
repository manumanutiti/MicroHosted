package cli

import (
	"fmt"
	"sort"
	"strings"

	"microhosted/pkg/types"
)

// The API addresses VMs, volumes and snapshots by ID only. Typing IDs is what
// makes curl slow, so the CLI resolves what a person naturally types — the
// full ID, a unique ID prefix, or (volumes, snapshots) the name — against one
// list call, like docker does.

type candidate struct{ id, name string }

// match resolves ref among cands: exact ID, then exact name, then unique ID
// prefix. Ambiguity is an error naming the contenders — guessing which VM to
// destroy is not a call a CLI should make.
func match(kind, ref string, cands []candidate) (string, error) {
	for _, c := range cands {
		if c.id == ref {
			return c.id, nil
		}
	}
	var hits []string
	for _, c := range cands {
		if c.name != "" && c.name == ref {
			hits = append(hits, c.id)
		}
	}
	if len(hits) == 0 {
		for _, c := range cands {
			if strings.HasPrefix(c.id, ref) {
				hits = append(hits, c.id)
			}
		}
	}
	switch len(hits) {
	case 0:
		return "", fmt.Errorf("no %s matches %q", kind, ref)
	case 1:
		return hits[0], nil
	}
	sort.Strings(hits)
	return "", fmt.Errorf("%q matches several %ss: %s — use the full ID", ref, kind, strings.Join(hits, ", "))
}

func (c *Client) listVMs() ([]types.VMResponse, error) {
	var vms []types.VMResponse
	return vms, c.Do("GET", "/v1/vms", nil, &vms)
}

func (c *Client) resolveVM(ref string) (string, error) {
	vms, err := c.listVMs()
	if err != nil {
		return "", err
	}
	cands := make([]candidate, 0, len(vms))
	for _, v := range vms {
		cands = append(cands, candidate{id: v.ID})
	}
	return match("VM", ref, cands)
}

func (c *Client) listVolumes() ([]types.VolumeResponse, error) {
	var vols []types.VolumeResponse
	return vols, c.Do("GET", "/v1/volumes", nil, &vols)
}

func (c *Client) resolveVolume(ref string) (string, error) {
	vols, err := c.listVolumes()
	if err != nil {
		return "", err
	}
	cands := make([]candidate, 0, len(vols))
	for _, v := range vols {
		cands = append(cands, candidate{id: v.ID, name: v.Name})
	}
	return match("volume", ref, cands)
}

func (c *Client) listSnapshots() ([]types.SnapshotResponse, error) {
	var snaps []types.SnapshotResponse
	return snaps, c.Do("GET", "/v1/snapshots", nil, &snaps)
}

// resolveSnapshot resolves ref; when sourceVM is set, only that VM's snapshots
// are considered. That is what makes "mh restore VM clean" work when every VM
// has a snapshot called "clean" — restore only accepts the VM's own anyway.
func (c *Client) resolveSnapshot(ref, sourceVM string) (string, error) {
	snaps, err := c.listSnapshots()
	if err != nil {
		return "", err
	}
	cands := make([]candidate, 0, len(snaps))
	for _, s := range snaps {
		if sourceVM == "" || s.SourceVM == sourceVM {
			cands = append(cands, candidate{id: s.ID, name: s.Name})
		}
	}
	return match("snapshot", ref, cands)
}
