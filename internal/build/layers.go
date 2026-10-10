package build

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// A build is a chain of layers, as Docker's: each step (FROM, PACKAGES,
// COPY, RUN…) runs on an overlayfs of the layers before it and its changes —
// the overlay's upper directory — become a layer of their own, kept for the
// next build. A step whose key is in the store is not run again: its layer
// is taken instead. The key is the hash of the parent layer's id and of all
// the step takes in (its scripts, their arguments and input, the contents of
// a COPY's source), so a change anywhere reruns that step and every step
// after it, and nothing before it.
//
// The layers are root's: Store/build/layers, 0700, next to the build's
// working directories (one filesystem: a layer is moved in, never copied,
// and overlayfs needs it for inode numbers mkfs can trust). A layer is
// written once, by the step that makes it, and from then on only mounted as
// a read-only lower directory.
//
// Layers are chained by the parent's id, not its key: a base rebuilt when its
// layer expired (or with --no-cache) has a new id, so nothing built on the old
// one is ever stacked on it.
//
//	layers/<id>/diff   the layer's files (an overlay upper directory)
//	layers/<id>/key    the key it was built for
//	layers/<id>/used   touched whenever a build takes it (prune reads it)
//	layers/keys/<key>  "<id> <unix time built>": the layer a key gives

// DefaultLayerMaxAge is how long a layer is reused. Steps take the network's
// state as it was when they ran — the base's upgrade, unpinned packages, a
// RUN's downloads — so a layer never hides security fixes for longer than
// this; --no-cache rebuilds them all now.
const DefaultLayerMaxAge = 7 * 24 * time.Hour

// maxLayers bounds a build's chain: overlayfs takes its lower directories in
// one page of mount options.
const maxLayers = 64

var (
	layerIDRE  = regexp.MustCompile(`^[0-9a-f]{32}$`)
	layerKeyRE = regexp.MustCompile(`^[0-9a-f]{64}$`)
	// safePathRE: a path that can be written into overlayfs's options, which
	// take ':' ',' and '\' as separators and escapes.
	safePathRE = regexp.MustCompile(`^/[A-Za-z0-9._/-]*$`)
)

// now is the clock layers are dated by (tests set it).
var now = time.Now

func layersDir(store string) string { return filepath.Join(store, "build", "layers") }

// pkgCacheDir is the package manager's cache for a base, mounted at
// /.mh-cache during the steps. Root's: packages are installed from it.
func pkgCacheDir(store string, base Base, arch string) string {
	return filepath.Join(store, "build", "cache", base.Distro+"-"+base.Version+"-"+arch)
}

// isolated is the unshare command line that runs argv in mount, PID, UTS
// and IPC namespaces of its own: what it mounts is gone when it ends, and so
// is every process it started (a RUN step's daemon cannot go on writing into
// a layer that is already kept), and it never sees the host's processes.
// The network is the host's: steps download.
func isolated(argv ...string) []string {
	return append([]string{"--mount", "--uts", "--ipc", "--pid", "--", "/bin/sh", "-c", superviseScript, "sh"}, argv...)
}

// superviseScript is the one process of an isolated command outside its PID
// namespace. Its first child — "$@" — is the namespace's PID 1: when it
// ends, the kernel kills everything else in it. A signal (sudo passes on
// Ctrl-C and SIGTERM) kills that PID 1, and so the whole namespace, at once.
// Everything else, mounts included, is done by "$@" inside.
// (Standard input goes through fd 3: sh gives a background command
// /dev/null before its own redirections.)
const superviseScript = `exec 3<&0
"$@" <&3 3<&- &
child=$!
exec 3<&-
trap 'kill -KILL "$child" 2>/dev/null; wait "$child"; exit 143' HUP INT TERM
wait "$child"`

// overlayOptions: no redirects, index or metacopy, so a layer holds whole
// files and plain whiteouts, and means the same stacked under any other.
const overlayOptions = "index=off,redirect_dir=off,metacopy=off"

// overlayMount is mount(8) for the overlays: forced onto mount(2), whose
// options take a page — maxLayers lower directories. util-linux 2.40+ mounts
// with fsconfig(2) otherwise, which takes a value of at most 256 bytes: a
// lowerdir of four layers already fails, as "wrong fs type, bad option"
// (Debian 13's 2.41, for one).
const overlayMount = "LIBMOUNT_FORCE_MOUNT2=always mount -t overlay overlay"

// stepScript mounts a step's view of the image in its own namespaces and
// runs "$@" (env, then the step's shell) chrooted into it. The scaffold,
// the top lower directory, is the build's own: it holds /etc/resolv.conf
// (DNS for the step), /proc, /dev, /.mh-cache and the staged COPY sources.
// Every mount point below is a directory of the scaffold's, which hides
// whatever the image has at that path — a symlink left by an earlier step
// cannot send a mount to the host — and the step's upper directory is empty
// until the chroot starts.
const stepScript = `set -e
layers=$1 lower=$2 upper=$3 work=$4 merged=$5 cache=$6
shift 6
cd "$layers"
` + overlayMount + ` -o "lowerdir=$lower,upperdir=$upper,workdir=$work,` + overlayOptions + `" "$merged"
cd /
mount -t proc -o nosuid,nodev,noexec proc "$merged/proc"
mount -t tmpfs -o nosuid,noexec,mode=0755,size=4m tmpfs "$merged/dev"
mknod -m 666 "$merged/dev/null" c 1 3
mknod -m 666 "$merged/dev/zero" c 1 5
mknod -m 666 "$merged/dev/full" c 1 7
mknod -m 666 "$merged/dev/random" c 1 8
mknod -m 666 "$merged/dev/urandom" c 1 9
mknod -m 666 "$merged/dev/tty" c 5 0
mkdir "$merged/dev/pts" "$merged/dev/shm"
mount -t tmpfs -o nosuid,nodev,mode=1777 tmpfs "$merged/dev/shm"
ln -s /proc/self/fd "$merged/dev/fd"
ln -s /proc/self/fd/0 "$merged/dev/stdin"
ln -s /proc/self/fd/1 "$merged/dev/stdout"
ln -s /proc/self/fd/2 "$merged/dev/stderr"
mount --bind "$cache" "$merged/.mh-cache"
exec chroot "$merged" "$@"`

// finalScript mounts the finished image read-only — the layers alone, no
// scaffold — in a mount namespace of its own and runs "$@" on it (du, find,
// mkfs.ext4 -d).
const finalScript = `set -e
layers=$1 lower=$2 merged=$3
shift 3
cd "$layers"
` + overlayMount + ` -o "lowerdir=$lower,` + overlayOptions + `" "$merged"
cd /
for d in proc dev sys; do
	if [ -L "$merged/$d" ] || [ ! -d "$merged/$d" ]; then
		echo "the image has no /$d directory" >&2
		exit 1
	fi
done
exec "$@"`

// lookupScript prints the layer keys/$2 names in $1 as "<id> <built>", or
// nothing: no such key, a malformed entry or a layer gone. It marks the
// layer as used.
const lookupScript = `f=$1/keys/$2
[ -f "$f" ] || exit 0
read -r id built < "$f" || exit 0
case $id in *[!0-9a-f]*|"") exit 0 ;; esac
[ ${#id} -eq 32 ] && [ -d "$1/$id/diff" ] || exit 0
touch "$1/$id/used"
printf '%s %s\n' "$id" "$built"`

// commitScript moves a step's upper directory $4 into the store as layer $2
// and points key $3 at it; the pointer is replaced by a rename, so a
// concurrent build sees either layer, whole.
const commitScript = `set -e
layers=$1 id=$2 key=$3 dir=$4 built=$5
mkdir -p -m 0700 "$layers/keys"
mkdir -m 0700 "$layers/$id"
mv "$dir" "$layers/$id/diff"
printf '%s\n' "$key" > "$layers/$id/key"
touch "$layers/$id/used"
printf '%s %s\n' "$id" "$built" > "$layers/keys/$key.$id"
mv -f "$layers/keys/$key.$id" "$layers/keys/$key"`

// action is one command of a step, run in the image: a shell script with
// its positional arguments, environment and standard input.
type action struct {
	Script string   `json:"script"`
	Args   []string `json:"args,omitempty"`
	Env    []string `json:"env,omitempty"`
	Stdin  string   `json:"stdin,omitempty"`
}

// step is one layer of the image.
type step struct {
	desc string // for the log: "RUN make"
	// inputs is what the step takes in besides its actions: a COPY's source
	// tree, FROM's pins. Part of the key.
	inputs string
	// from lays the tree down into an empty directory instead of running
	// actions on the layers below (FROM).
	from    func(dir string) error
	stage   func() error // before the actions, when the step runs
	actions []action
}

// key is the step's cache key on top of parent (a layer id; "" for FROM).
func (s step) key(arch, parent string) string {
	b, _ := json.Marshal(struct {
		Recipe  int
		Arch    string
		Parent  string
		Desc    string
		Inputs  string
		Actions []action
	}{Recipe, arch, parent, s.desc, s.inputs, s.actions})
	return sha256Hex(b)
}

type layer struct {
	id    string
	built time.Time
}

// lookup finds the layer for key, if the store has one younger than maxAge.
func (b *builder) lookup(key string) (layer, bool, error) {
	out, err := b.r.Output(b.ctx, "sh", "-c", lookupScript, "sh", b.layers, key)
	if err != nil {
		return layer{}, false, fmt.Errorf("looking up a layer: %w", err)
	}
	f := strings.Fields(string(out))
	if len(f) != 2 || !layerIDRE.MatchString(f[0]) {
		return layer{}, false, nil
	}
	sec, err := strconv.ParseInt(f[1], 10, 64)
	if err != nil {
		return layer{}, false, nil
	}
	l := layer{id: f[0], built: time.Unix(sec, 0)}
	if age := now().Sub(l.built); age > b.maxAge || age < -time.Hour {
		return layer{}, false, nil
	}
	return l, true, nil
}

// commit keeps dir, a finished step's upper directory, as the layer for key.
func (b *builder) commit(key, dir string) (layer, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return layer{}, err
	}
	l := layer{id: hex.EncodeToString(raw[:]), built: now()}
	if err := b.r.Run(b.ctx, nil, "sh", "-c", commitScript, "sh", b.layers, l.id, key, dir, strconv.FormatInt(l.built.Unix(), 10)); err != nil {
		return layer{}, fmt.Errorf("keeping the layer: %w", err)
	}
	return l, nil
}

// lowerdir is overlayfs's lowerdir= for the chain, topmost first, with top
// (the scaffold, or FINISH's changes) above it. The layers are relative to
// the layer store, where the scripts mount from: short enough for
// maxLayers of them in a page.
func lowerdir(top string, chain []layer) string {
	parts := []string{top}
	for i := len(chain) - 1; i >= 0; i-- {
		parts = append(parts, chain[i].id+"/diff")
	}
	return strings.Join(parts, ":")
}

// runStep runs a step's actions, in order, in an overlay of chain with upper
// as its writable layer.
func (b *builder) runStep(chain []layer, dir string, actions []action) error {
	upper, work, merged := filepath.Join(dir, "upper"), filepath.Join(dir, "work"), filepath.Join(dir, "merged")
	if err := b.r.Run(b.ctx, nil, "mkdir", "-p", "-m", "0755", upper, work, merged); err != nil {
		return err
	}
	lower := lowerdir(b.scaffold, chain)
	for _, a := range actions {
		argv := isolated("/bin/sh", "-c", stepScript, "sh", b.layers, lower, upper, work, merged, b.pkgCache)
		argv = append(argv, "/usr/bin/env", "-i", "PATH=/usr/sbin:/usr/bin:/sbin:/bin", "HOME=/root")
		argv = append(argv, a.Env...)
		argv = append(append(argv, "/bin/sh", "-c", a.Script, "sh"), a.Args...)
		var stdin io.Reader
		if a.Stdin != "" {
			stdin = strings.NewReader(a.Stdin)
		}
		if err := b.r.Run(b.ctx, stdin, "unshare", argv...); err != nil {
			return err
		}
	}
	return nil
}

// onFinal runs name with args on the finished image: chain topped by top,
// mounted read-only at merged ("$MERGED" in args is replaced by it).
func (b *builder) onFinal(ctx context.Context, top, merged string, chain []layer, output bool, name string, args ...string) ([]byte, error) {
	argv := []string{"--mount", "--", "/bin/sh", "-c", finalScript, "sh", b.layers, lowerdir(top, chain), merged, name}
	for _, a := range args {
		argv = append(argv, strings.ReplaceAll(a, "$MERGED", merged))
	}
	if output {
		return b.r.Output(ctx, "unshare", argv...)
	}
	return nil, b.r.Run(ctx, nil, "unshare", argv...)
}

// PruneOptions says which layers Prune removes.
type PruneOptions struct {
	// OlderThan removes the layers no build has taken for this long, and
	// every layer superseded by a newer one for its key after an hour. 0
	// takes DefaultLayerMaxAge: layers that could not be reused anyway.
	OlderThan time.Duration
	// All removes every layer and the package caches. A build running now
	// may lose layers it stands on and fail.
	All bool
}

// PruneResult is what Prune removed.
type PruneResult struct {
	Layers int
	Caches bool
}

// listScript prints every layer of $1 as "layer <id> <last used>" and every
// key as "key <key> <id>".
const listScript = `cd "$1" 2>/dev/null || exit 0
for d in */; do
	d=${d%/}
	[ "$d" != keys ] && [ -d "$d" ] || continue
	printf 'layer %s %s\n' "$d" "$(stat -c %Y "$d/used" 2>/dev/null || echo 0)"
done
for f in keys/*; do
	[ -f "$f" ] || continue
	read -r id built < "$f" || continue
	printf 'key %s %s\n' "${f#keys/}" "$id"
done`

// Prune removes the layer store's unused layers (mh builder prune).
func Prune(ctx context.Context, r Runner, store string, o PruneOptions) (PruneResult, error) {
	var res PruneResult
	dir := layersDir(store)
	out, err := r.Output(ctx, "sh", "-c", listScript, "sh", dir)
	if err != nil {
		return res, err
	}
	used := map[string]time.Time{}
	current := map[string]bool{} // layers a key points at
	keys := map[string]string{}  // key → id
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		switch {
		case len(f) == 3 && f[0] == "layer" && layerIDRE.MatchString(f[1]):
			sec, _ := strconv.ParseInt(f[2], 10, 64)
			used[f[1]] = time.Unix(sec, 0)
		case len(f) == 3 && f[0] == "key" && layerKeyRE.MatchString(f[1]):
			keys[f[1]] = f[2]
			current[f[2]] = true
		}
	}
	older := o.OlderThan
	if older <= 0 {
		older = DefaultLayerMaxAge
	}
	var gone []string
	for id, t := range used {
		age := now().Sub(t)
		if o.All || age > older || !current[id] && age > time.Hour {
			gone = append(gone, id)
		}
	}
	sort.Strings(gone)
	removed := map[string]bool{}
	for _, id := range gone {
		if err := r.Run(ctx, nil, "rm", "-rf", "--one-file-system", filepath.Join(dir, id)); err != nil {
			return res, err
		}
		removed[id] = true
		res.Layers++
	}
	// Keys whose layer is gone, however it went.
	for key, id := range keys {
		if _, ok := used[id]; removed[id] || !ok {
			if err := r.Run(ctx, nil, "rm", "-f", filepath.Join(dir, "keys", key)); err != nil {
				return res, err
			}
		}
	}
	if o.All {
		if err := r.Run(ctx, nil, "rm", "-rf", "--one-file-system", filepath.Join(store, "build", "cache")); err != nil {
			return res, err
		}
		res.Caches = true
	}
	return res, nil
}
