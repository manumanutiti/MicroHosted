package vm

import (
	"errors"
	"strings"
	"testing"

	"microhosted/pkg/types"
)

func TestPrepareFiles(t *testing.T) {
	inject, recs, err := prepareFiles([]types.FileSpec{
		{Path: "/etc/app//parser.conf", Content: []byte("a=1\n")},
		{Path: "/etc/app/key", Content: []byte("hunter2"), Secret: true, UID: 1000, GID: 1000},
		{Path: "/opt/run.sh", Content: []byte("#!/bin/sh\n"), Mode: "755"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if inject[0].Path != "/etc/app/parser.conf" || inject[0].Mode != 0o644 {
		t.Errorf("default: %+v", inject[0])
	}
	if inject[1].Mode != 0o400 || recs[1].SHA256 != "" || !recs[1].Secret {
		t.Errorf("secret: %+v %+v — default 0400 and no hash", inject[1], recs[1])
	}
	if inject[2].Mode != 0o755 || recs[2].Mode != "0755" {
		t.Errorf("explicit mode: %+v %+v", inject[2], recs[2])
	}
	if len(recs[0].SHA256) != 64 || recs[0].Size != 4 {
		t.Errorf("record: %+v", recs[0])
	}

	many := make([]types.FileSpec, maxFiles+1)
	for i := range many {
		many[i] = types.FileSpec{Path: "/f" + strings.Repeat("x", i)}
	}
	for name, specs := range map[string][]types.FileSpec{
		"relative":  {{Path: "etc/x"}},
		"injection": {{Path: "/x\nwrite /etc/shadow /y"}},
		"duplicate": {{Path: "/a"}, {Path: "/b/../a"}},
		"setuid":    {{Path: "/a", Mode: "4755"}},
		"not octal": {{Path: "/a", Mode: "rw-r--r--"}},
		"owner":     {{Path: "/a", UID: -1}},
		"too many":  many,
		"too large": {{Path: "/a", Content: make([]byte, maxFilesBytes+1)}},
	} {
		if _, _, err := prepareFiles(specs); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v, want ErrInvalid", name, err)
		}
	}

	_, _, err = prepareFiles([]types.FileSpec{{Path: "/a", Mode: "bad", Content: []byte("topsecret"), Secret: true}})
	if err == nil || strings.Contains(err.Error(), "topsecret") {
		t.Errorf("error %v carries the content", err)
	}
}
