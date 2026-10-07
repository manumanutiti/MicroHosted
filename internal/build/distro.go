package build

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// distro is what differs between bases: how the tree is laid down, how it is
// brought up to date, how packages are installed, which init starts the
// agent, what is cleaned. Everything but bootstrap is a shell script run in
// the image's chroot, and the script's text is part of its layer's key: a
// change here is a new layer, never the cached one built by the old text.
type distro struct {
	// basePackages are installed in every image: the agent's socat, and
	// what the distro's minimal tree lacks for it.
	basePackages []string
	init         string // for the build log
	// check fails early, before anything is written, when the build host
	// cannot build this base.
	check func(Base, Options) error
	// bootstrap lays the base down into dir, an empty directory of root's
	// (FROM). fromInputs is what it depends on besides the base's pins: a
	// new keyring or other arguments are a new base layer.
	bootstrap  func(b *builder, dir string) error
	fromInputs func(b *builder) (string, error)
	// update names prepare in the build log.
	update string
	// prepare brings the base up to date and points the package manager at
	// /.mh-cache, the build's package cache (a layer of its own: packages
	// change far more often than the base).
	prepare      string
	prepareStdin func(b *builder) string
	// install installs "$@".
	install string
	env     []string
	// configure makes the init start the agent; stdin is written by it.
	configure      string
	configureStdin string
	// cleanup undoes what prepare pointed at the build and drops the
	// package manager's indexes (FINISH, never cached).
	cleanup string
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
	bootstrap: func(b *builder, dir string) error {
		tarball, err := b.pinned(b.base.URL(b.o.Arch), b.base.File(b.o.Arch), b.base.SHA256[b.o.Arch])
		if err != nil {
			return err
		}
		if err := b.r.Run(b.ctx, nil, "tar", "-xzf", tarball, "-C", dir); err != nil {
			return fmt.Errorf("unpacking %s: %w", tarball, err)
		}
		return nil
	},
	fromInputs: func(*builder) (string, error) { return "minirootfs", nil },
	// /etc/apk/cache is where apk keeps what it downloads; apk checks every
	// cached index against its signature and every package against the
	// index. The minirootfs is the release as it shipped: the upgrade brings
	// the branch's security fixes.
	update:  "apk upgrade",
	prepare: `mkdir -p /.mh-cache/apk && ln -sfn /.mh-cache/apk /etc/apk/cache && apk upgrade -U`,
	install: `apk add -U "$@"`,
	// No service manager: the agent respawned, ctrl-alt-del (Firecracker's
	// power off) reboots — which ends the VM.
	configure:      `cat > /etc/inittab`,
	configureStdin: inittab,
	cleanup:        `rm -f /etc/apk/cache && rm -rf /var/cache/apk/*`,
}

// inittab is busybox init's configuration, as in build-rootfs-alpine.sh.
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

// ubuntuInclude is what debootstrap's minbase (the Essential packages and
// apt) lacks for a VM: an init, and udev for the device units systemd
// waits on (the serial console's). The rest of the default variant —
// netplan, rsyslog, cron, ubuntu-pro-client… — is for a machine someone
// administers, not a VM that is replaced; a spec's packages add what it
// needs.
const ubuntuInclude = "systemd-sysv,udev"

// Ubuntu: debootstrap (minbase), apt, systemd — without SSH (access is the
// vsock agent; add openssh-server to packages for it). Packages are verified
// by the archive's signature, not pinned by hash.
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
	bootstrap: func(b *builder, dir string) error {
		// debootstrap's own cache is root's, next to the layers: root
		// installs what is in it.
		cache := filepath.Join(b.o.Store, "build", "cache", "debootstrap-"+b.base.Suite+"-"+debArch[b.o.Arch])
		if err := b.r.Run(b.ctx, nil, "mkdir", "-p", "-m", "0700", cache); err != nil {
			return err
		}
		// In its own mount and PID namespaces: what debootstrap mounts in
		// the tree and whatever its package scripts leave running end with it.
		argv := isolated(append([]string{"debootstrap"}, debootstrapArgs(b)...)...)
		argv = append(argv, "--cache-dir="+cache, b.base.Suite, dir, b.base.mirror(b.o.Arch))
		if err := b.r.Run(b.ctx, nil, "unshare", argv...); err != nil {
			return fmt.Errorf("debootstrap %s: %w", b.base.Suite, err)
		}
		return nil
	},
	fromInputs: func(b *builder) (string, error) {
		keys, err := hashFile(ubuntuKeyring)
		if err != nil {
			return "", err
		}
		return strings.Join(append(debootstrapArgs(b), "keyring-sha256="+keys, b.base.mirror(b.o.Arch)), " "), nil
	},
	// policy-rc.d keeps package scripts from starting services on the build
	// host; dpkg skips its fsyncs (the tree is written out whole at the end);
	// apt keeps its downloads in /.mh-cache, and checks each cached .deb
	// against the signed index before using it. Security fixes and updates
	// too, not only the release as it was.
	update: "apt-get upgrade",
	prepare: `cat > /etc/apt/sources.list &&
printf '#!/bin/sh\nexit 101\n' > /usr/sbin/policy-rc.d && chmod 0755 /usr/sbin/policy-rc.d &&
echo force-unsafe-io > /etc/dpkg/dpkg.cfg.d/00mh-build &&
printf 'Dir::Cache::archives "/.mh-cache/apt/";\n' > /etc/apt/apt.conf.d/00mh-build &&
mkdir -p /.mh-cache/apt/partial &&
apt-get update && apt-get upgrade -y`,
	prepareStdin: func(b *builder) string {
		return fmt.Sprintf("deb %[1]s %[2]s main universe\ndeb %[1]s %[2]s-updates main universe\ndeb %[1]s %[2]s-security main universe\n",
			b.base.mirror(b.o.Arch), b.base.Suite)
	},
	install: `apt-get update && apt-get install -y --no-install-recommends "$@"`,
	env:     []string{"DEBIAN_FRONTEND=noninteractive"},
	// As build-rootfs.sh: a passwordless root on the serial console
	// (reachable only from the host), the root disk by its device, no
	// timers that would wake apt in a VM. The hostname resolves locally:
	// otherwise every getfqdn() (Python's HTTPServer, rsyslog) asks the DNS
	// servers, and on a network without egress waits 20 s for them.
	configure: `cat > /etc/systemd/system/microhosted-exec.service &&
systemctl enable microhosted-exec.service &&
passwd -d root && echo microvm > /etc/hostname &&
{ grep -qw microvm /etc/hosts || printf '127.0.1.1\tmicrovm\n' >> /etc/hosts; } &&
echo "/dev/vda / ext4 defaults,noatime 0 1" > /etc/fstab &&
systemctl mask apt-daily.timer apt-daily-upgrade.timer >/dev/null`,
	configureStdin: fmt.Sprintf(`[Unit]
Description=MicroHosted vsock exec listener

[Service]
ExecStart=/usr/bin/socat VSOCK-LISTEN:%d,fork,reuseaddr EXEC:/usr/local/bin/microhosted-exec
Restart=always

[Install]
WantedBy=multi-user.target
`, AgentPort),
	cleanup: `rm -f /usr/sbin/policy-rc.d /etc/dpkg/dpkg.cfg.d/00mh-build /etc/apt/apt.conf.d/00mh-build && apt-get clean && rm -rf /var/lib/apt/lists/*`,
}

func debootstrapArgs(b *builder) []string {
	return []string{"--keyring=" + ubuntuKeyring, "--arch=" + debArch[b.o.Arch], "--variant=minbase",
		"--include=" + ubuntuInclude, "--components=main,universe"}
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

// mirror is where Ubuntu's packages come from: MH_UBUNTU_MIRROR when set
// (a country's, http://es.archive.ubuntu.com/ubuntu, when the main archive
// is slow or unreachable; ports' mirrors for aarch64), else Canonical's.
// Any mirror will do: what it serves is checked against the archive's
// signature (ubuntuKeyring), and it is part of the base's inputs, so a
// change builds the base again.
func (b Base) mirror(arch string) string {
	if m := strings.TrimRight(os.Getenv("MH_UBUNTU_MIRROR"), "/"); m != "" {
		return m
	}
	switch arch {
	case "x86_64":
		return "http://archive.ubuntu.com/ubuntu"
	case "aarch64":
		return "http://ports.ubuntu.com/ubuntu-ports"
	}
	return ""
}

func sha256Hex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}
