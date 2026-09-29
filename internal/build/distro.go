package build

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// distro is what differs between bases: how the tree is laid down, how
// packages are installed, which init starts the agent, what is cleaned.
type distro struct {
	// basePackages are installed in every image: the agent's socat, and
	// what the distro's minimal tree lacks for it.
	basePackages []string
	init         string // for the build log
	// check fails early, before anything is written, when the build host
	// cannot build this base.
	check     func(Base, Options) error
	bootstrap func(*builder) error
	install   func(*builder, []string) error
	configure func(*builder) error
	// cleanup runs in the chroot at the end (a shell command).
	cleanup string
	// keepDev: the base ships its own /dev nodes; the build's are not removed.
	keepDev bool
}

var distros = map[string]distro{"alpine": alpine, "ubuntu": ubuntu}

// Alpine: the minirootfs, apk, busybox init — make prepare-image's default.
var alpine = distro{
	basePackages: []string{"socat"},
	init:         "busybox init",
	check: func(b Base, o Options) error {
		if _, ok := b.SHA256[o.Arch]; !ok {
			return fmt.Errorf("base alpine:%s has no pinned build for %s", b.Version, o.Arch)
		}
		return nil
	},
	bootstrap: func(b *builder) error {
		tarball, err := b.pinned(b.base.URL(b.o.Arch), b.base.File(b.o.Arch), b.base.SHA256[b.o.Arch])
		if err != nil {
			return err
		}
		if err := b.r.Run(b.ctx, nil, "tar", "-xzf", tarball, "-C", b.tree); err != nil {
			return fmt.Errorf("unpacking %s: %w", tarball, err)
		}
		return nil
	},
	install: func(b *builder, pkgs []string) error {
		return b.chroot(nil, `apk add --no-cache "$@"`, pkgs...)
	},
	configure: func(b *builder) error {
		return b.chroot(strings.NewReader(inittab), `cat > /etc/inittab`)
	},
	cleanup: `rm -rf /var/cache/apk/*`,
}

// inittab is busybox init's configuration, as in build-rootfs-alpine.sh: no
// service manager, the agent respawned, ctrl-alt-del (Firecracker's power
// off) reboots — which ends the VM.
var inittab = fmt.Sprintf(`::sysinit:/bin/mount -t proc proc /proc
::sysinit:/bin/mount -t sysfs sysfs /sys
::sysinit:/bin/mount -t devtmpfs devtmpfs /dev
::sysinit:/bin/mkdir -p /dev/pts
::sysinit:/bin/mount -t devpts devpts /dev/pts
::respawn:/usr/bin/socat VSOCK-LISTEN:%d,fork,reuseaddr EXEC:/usr/local/bin/microhosted-exec
ttyS0::respawn:/sbin/getty -L 115200 ttyS0 vt100
::ctrlaltdel:/sbin/reboot
::shutdown:/bin/umount -a -r
`, AgentPort)

// ubuntuKeyring verifies what debootstrap fetches: the archive's Release
// file against it, every package against the Release file.
var ubuntuKeyring = "/usr/share/keyrings/ubuntu-archive-keyring.gpg"

// Ubuntu: debootstrap, apt, systemd — what make prepare-image FLAVOR=ubuntu
// builds, without SSH (access is the vsock agent; add openssh-server to
// packages for it). Packages are verified by the archive's signature, not
// pinned by hash; the .debs are cached between builds.
var ubuntu = distro{
	basePackages: []string{"socat"},
	init:         "systemd",
	check: func(b Base, o Options) error {
		if !haveDebootstrap() {
			return fmt.Errorf("base ubuntu:%s is built with debootstrap, which this host lacks: sudo apt-get install debootstrap", b.Version)
		}
		if _, err := os.Stat(ubuntuKeyring); err != nil {
			return fmt.Errorf("no Ubuntu archive keyring at %s (sudo apt-get install ubuntu-keyring): without it nothing debootstrap fetches is verified", ubuntuKeyring)
		}
		if b.mirror(o.Arch) == "" {
			return fmt.Errorf("base ubuntu:%s: no mirror for %s", b.Version, o.Arch)
		}
		return nil
	},
	bootstrap: func(b *builder) error {
		cache := filepath.Join(b.o.CacheDir, "debootstrap", b.base.Suite+"-"+debArch[b.o.Arch])
		if err := os.MkdirAll(cache, 0o755); err != nil {
			return err
		}
		if err := b.r.Run(b.ctx, nil, "debootstrap", "--keyring="+ubuntuKeyring, "--arch="+debArch[b.o.Arch],
			"--components=main,universe", "--cache-dir="+cache, b.base.Suite, b.tree, b.base.mirror(b.o.Arch)); err != nil {
			return fmt.Errorf("debootstrap %s: %w", b.base.Suite, err)
		}
		// Security fixes and updates too, not only the release as it was.
		m, suite := b.base.mirror(b.o.Arch), b.base.Suite
		sources := fmt.Sprintf("deb %[1]s %[2]s main universe\ndeb %[1]s %[2]s-updates main universe\ndeb %[1]s %[2]s-security main universe\n", m, suite)
		return b.chroot(strings.NewReader(sources), `cat > /etc/apt/sources.list`)
	},
	install: func(b *builder, pkgs []string) error {
		// policy-rc.d keeps package scripts from starting services on the
		// build host.
		return b.chrootEnv(nil, []string{"DEBIAN_FRONTEND=noninteractive"},
			`printf '#!/bin/sh\nexit 101\n' > /usr/sbin/policy-rc.d && chmod 0755 /usr/sbin/policy-rc.d && apt-get update && apt-get upgrade -y && apt-get install -y --no-install-recommends "$@"`, pkgs...)
	},
	configure: func(b *builder) error {
		unit := fmt.Sprintf(`[Unit]
Description=MicroHosted vsock exec listener

[Service]
ExecStart=/usr/bin/socat VSOCK-LISTEN:%d,fork,reuseaddr EXEC:/usr/local/bin/microhosted-exec
Restart=always

[Install]
WantedBy=multi-user.target
`, AgentPort)
		// As build-rootfs.sh: a passwordless root on the serial console
		// (reachable only from the host), the root disk by its device, no
		// timers that would wake apt in a VM. The hostname resolves locally:
		// otherwise every getfqdn() (Python's HTTPServer, rsyslog) asks the
		// DNS servers, and on a network without egress waits 20 s for them.
		return b.chroot(strings.NewReader(unit), `cat > /etc/systemd/system/microhosted-exec.service &&
systemctl enable microhosted-exec.service &&
passwd -d root && echo microvm > /etc/hostname &&
{ grep -qw microvm /etc/hosts || printf '127.0.1.1\tmicrovm\n' >> /etc/hosts; } &&
echo "/dev/vda / ext4 defaults,noatime 0 1" > /etc/fstab &&
systemctl mask apt-daily.timer apt-daily-upgrade.timer >/dev/null`)
	},
	cleanup: `rm -f /usr/sbin/policy-rc.d && apt-get clean && rm -rf /var/lib/apt/lists/*`,
	keepDev: true,
}

// haveDebootstrap reports whether debootstrap is installed (in root's PATH,
// which a user's may lack).
var haveDebootstrap = func() bool {
	if _, err := exec.LookPath("debootstrap"); err == nil {
		return true
	}
	_, err := os.Stat("/usr/sbin/debootstrap")
	return err == nil
}

var debArch = map[string]string{"x86_64": "amd64", "aarch64": "arm64"}

func (b Base) mirror(arch string) string {
	switch arch {
	case "x86_64":
		return "http://archive.ubuntu.com/ubuntu"
	case "aarch64":
		return "http://ports.ubuntu.com/ubuntu-ports"
	}
	return ""
}
