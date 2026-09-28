package build

import (
	"fmt"
	"strings"
)

// The downloads a build starts from, pinned by SHA-256: a file whose hash is
// not listed, or does not match, is refused (Ubuntu's packages are verified
// by the archive's signature instead, see distro.go). They are the same pins as
// scripts/checksums.sha256 (a test keeps the two equal); a new version is
// added in both, after checking it as that file's header says.

// Base is a distribution an image starts from.
type Base struct {
	Distro  string // "alpine" or "ubuntu" (see distros)
	Version string // "3.22.0", "24.04"
	// Alpine: a minirootfs pinned by SHA-256 per architecture.
	Branch string            // "v3.22": the apk repository branch
	SHA256 map[string]string // by architecture (x86_64, aarch64)
	// Ubuntu: the release debootstrap lays down, verified against the
	// archive's signing keyring rather than pinned by hash.
	Suite string // "noble"
}

// Bases are the known bases by "<distro>:<version>"; baseAliases add the
// shorter names ("alpine:3.22", "ubuntu:noble").
var Bases = map[string]Base{
	"alpine:3.22.0": {Distro: "alpine", Branch: "v3.22", Version: "3.22.0", SHA256: map[string]string{
		"x86_64":  "18879884e35b0718f017a50ff85b5e6568279e97233fc42822229585feb2fa4d",
		"aarch64": "74eaa1f80aceb35a69f26a7772cd22848a51921323a32b2ae057b2a4c8188fd5",
	}},
	"ubuntu:24.04": {Distro: "ubuntu", Version: "24.04", Suite: "noble"},
}

var baseAliases = map[string]string{"alpine:3.22": "alpine:3.22.0", "ubuntu:noble": "ubuntu:24.04"}

// Kernels are the known Firecracker CI guest kernels, by version.
var Kernels = map[string]map[string]string{
	"6.1.102": {
		"x86_64":  "49ba99a5299444ac59dda2efc3569cc2d58a5d72ea6475a6bfc37aa0bf322e54",
		"aarch64": "bb1f50912d63a8ca5e92d488984875e1177eb9283050ffa592a8cb455cada52d",
	},
}

// DefaultKernel is the kernel of a spec that names none: make
// prepare-image's.
const DefaultKernel = "6.1.102"

func resolveBase(name string) (Base, error) {
	if alias, ok := baseAliases[name]; ok {
		name = alias
	}
	b, ok := Bases[name]
	if !ok {
		return Base{}, fmt.Errorf("%q is not a known base (known: %s)", name, strings.Join(knownBases(), ", "))
	}
	return b, nil
}

func knownBases() []string {
	names := sortedKeys(Bases)
	names = append(names, sortedKeys(baseAliases)...)
	return names
}

// URL is where the base's minirootfs for arch is downloaded from.
func (b Base) URL(arch string) string {
	return fmt.Sprintf("https://dl-cdn.alpinelinux.org/alpine/%s/releases/%s/%s", b.Branch, arch, b.File(arch))
}

// File is the name of the base's minirootfs for arch.
func (b Base) File(arch string) string {
	return fmt.Sprintf("alpine-minirootfs-%s-%s.tar.gz", b.Version, arch)
}

// KernelURL is where kernel version for arch is downloaded from.
func KernelURL(version, arch string) string {
	return fmt.Sprintf("https://s3.amazonaws.com/spec.ccfc.min/firecracker-ci/v1.10/%s/vmlinux-%s", arch, version)
}
