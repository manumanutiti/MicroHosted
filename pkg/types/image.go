package types

import "time"

// Image is a content-addressed boot image in the daemon's image store: a
// kernel and a root filesystem, each kept read-only under its own sha256, and
// the defaults a VM gets from it. Unlike a catalog template, whose files are
// addressed by path and can be rewritten in place, an image never changes: a
// new build is a new digest. A VM created from an image boots exactly the
// bytes that were hashed at import.
type Image struct {
	// Digest identifies the image: "sha256:<hex>" of its canonical manifest
	// (ImageManifest). Two imports of the same files with the same defaults
	// are the same image.
	Digest   string
	Manifest ImageManifest
	// Tags are the "name:version" references that resolve to this image. A
	// tag is bound once and never moves: importing a different image under a
	// taken tag is refused, so a tag means the same bytes forever.
	Tags       []string
	ImportedAt time.Time
}

// ImageManifest is what an image's digest covers. Its JSON encoding, with
// fields in this order, is the canonical form that is hashed; a field added
// later is part of the digest of the images that set it.
type ImageManifest struct {
	Schema int `json:"schema"`
	// Kernel and Rootfs are the "sha256:<hex>" digests of the two files.
	Kernel string `json:"kernel"`
	Rootfs string `json:"rootfs"`
	VCPUs  int64  `json:"vcpus"`
	MemMB  int64  `json:"mem_mb"`
	DiskMB int64  `json:"disk_mb"`
	// Command and Health are what the image says it runs and how to check
	// it, for whoever starts its VMs (the orchestrator; the engine runs
	// neither). Omitted when unset, so images without them keep the digest
	// they had before these fields existed.
	Command string       `json:"command,omitempty"`
	Health  *ImageHealth `json:"health,omitempty"`
}

// ImageHealth is an image's default health check: Command run inside the VM,
// exit 0 healthy, every Every, given Timeout, unhealthy after Failures in a
// row. Durations are Go durations in canonical form ("10s", "1m30s"); empty
// or zero leaves the consumer's default.
type ImageHealth struct {
	Command  string `json:"command"`
	Every    string `json:"every,omitempty"`
	Timeout  string `json:"timeout,omitempty"`
	Failures int    `json:"failures,omitempty"`
}

// ImportImageRequest is the payload accepted by POST /v1/images: the daemon
// copies both files into its store, hashes its own copies, and tags the
// result.
type ImportImageRequest struct {
	// Name "name:version", e.g. "parser-modbus:1.2".
	Name string `json:"name"`
	// KernelPath and RootfsPath are files on the daemon's host.
	KernelPath string `json:"kernel_path"`
	RootfsPath string `json:"rootfs_path"`
	// Defaults for VMs created from the image; a create may override them.
	VCPUs  int64 `json:"vcpus"`
	MemMB  int64 `json:"mem_mb"`
	DiskMB int64 `json:"disk_mb,omitempty"`
	// Optional defaults for whoever runs the image (see ImageManifest).
	Command string       `json:"command,omitempty"`
	Health  *ImageHealth `json:"health,omitempty"`
}

// ImageResponse is the JSON representation of an Image.
type ImageResponse struct {
	Digest     string       `json:"digest"`
	Tags       []string     `json:"tags"`
	Kernel     string       `json:"kernel"`
	Rootfs     string       `json:"rootfs"`
	VCPUs      int64        `json:"vcpus"`
	MemMB      int64        `json:"mem_mb"`
	DiskMB     int64        `json:"disk_mb"`
	Command    string       `json:"command,omitempty"`
	Health     *ImageHealth `json:"health,omitempty"`
	SizeMB     int64        `json:"size_mb"`
	ImportedAt string       `json:"imported_at"`
	// Missing names a file of the image that is not in the store any more:
	// the image cannot boot until it is imported again.
	Missing string `json:"missing,omitempty"`
}

// ImageVerifyResponse is what POST /v1/images/{ref}/verify returns.
type ImageVerifyResponse struct {
	Digest string `json:"digest"`
	OK     bool   `json:"ok"`
	Error  string `json:"error,omitempty"`
}

// ImageDeleteResponse is what DELETE /v1/images/{ref} returns: the tags it
// removed and whether the image itself went with them (Deleted false: only
// the tag was removed, the image stays under its other tags).
type ImageDeleteResponse struct {
	Digest   string   `json:"digest"`
	Untagged []string `json:"untagged"`
	Deleted  bool     `json:"deleted"`
}
