package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"

	"microhosted/internal/network"
	"microhosted/internal/storage"
	"microhosted/internal/vm"
	"microhosted/pkg/types"
)

// NewServer builds the HTTP API meant to sit behind a future web panel:
// create/list/destroy VMs and browse the template catalog. This is the
// integration point Stage 7 originally planned for — it exists from the
// start here so a panel can be built against it without reshaping the
// manager underneath.
func NewServer(mgr *vm.Manager, netmgr *network.Manager, sysCfg SystemConfig) *http.Server {
	mux := http.NewServeMux()

	// Observability: GET /v1/system (full report) + GET /v1/health (cheap
	// 200/503 probe). See system.go.
	registerSystemRoutes(mux, mgr, netmgr, sysCfg)

	mux.HandleFunc("GET /v1/templates", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, mgr.Templates())
	})

	mux.HandleFunc("POST /v1/networks", func(w http.ResponseWriter, r *http.Request) {
		var req types.CreateNetworkRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		if req.Name == "" {
			writeError(w, http.StatusBadRequest, errors.New("name is required"))
			return
		}
		// Bad egress rules are the caller's fault (400), so vet them here;
		// Create re-checks as defense in depth but reports 500.
		if req.Egress && len(req.AllowedEgress) > 0 {
			writeError(w, http.StatusBadRequest, errors.New("egress and allowed_egress are mutually exclusive"))
			return
		}
		// Includes rules naming a managed interface: one this daemon was not
		// told to manage is the caller's mistake, not a server fault, so it
		// is a 400 carrying the declared list to make it actionable.
		if err := network.ValidateEgressRules(req.AllowedEgress, netmgr.ManagedIfaceNames()); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		n, err := netmgr.Create(req)
		if err != nil {
			writeError(w, networkErrStatus(err), err)
			return
		}
		writeJSON(w, http.StatusCreated, types.NewNetworkResponse(n))
	})

	mux.HandleFunc("GET /v1/networks", func(w http.ResponseWriter, r *http.Request) {
		nets := netmgr.List()
		resp := make([]types.NetworkResponse, 0, len(nets))
		for _, n := range nets {
			resp = append(resp, types.NewNetworkResponse(n))
		}
		writeJSON(w, http.StatusOK, resp)
	})

	mux.HandleFunc("GET /v1/networks/{name}", func(w http.ResponseWriter, r *http.Request) {
		n, ok := netmgr.Get(r.PathValue("name"))
		if !ok {
			writeError(w, http.StatusNotFound, fmt.Errorf("network %q not found", r.PathValue("name")))
			return
		}
		writeJSON(w, http.StatusOK, types.NewNetworkResponse(n))
	})

	// Live egress-policy update: replaces the network's whole egress policy
	// atomically without touching attached VMs. Old flows are cut immediately
	// (the forward chain matches statelessly — see internal/network).
	mux.HandleFunc("PUT /v1/networks/{name}/egress", func(w http.ResponseWriter, r *http.Request) {
		var req types.UpdateNetworkEgressRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		if req.Egress && len(req.AllowedEgress) > 0 {
			writeError(w, http.StatusBadRequest, errors.New("egress and allowed_egress are mutually exclusive"))
			return
		}
		if err := network.ValidateEgressRules(req.AllowedEgress, netmgr.ManagedIfaceNames()); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		if _, ok := netmgr.Get(r.PathValue("name")); !ok {
			writeError(w, http.StatusNotFound, fmt.Errorf("network %q not found", r.PathValue("name")))
			return
		}
		n, err := netmgr.UpdateEgress(r.PathValue("name"), req)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, types.NewNetworkResponse(n))
	})

	// Live ingress-policy update: replaces the network's whole ingress policy
	// (the inbound DNAT holes from managed interfaces). Like egress, removing
	// a rule cuts its live flows on their next packet.
	mux.HandleFunc("PUT /v1/networks/{name}/ingress", func(w http.ResponseWriter, r *http.Request) {
		var req types.UpdateNetworkIngressRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		if err := network.ValidateIngressRules(req.AllowedIngress, netmgr.ManagedIfaceNames()); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		if _, ok := netmgr.Get(r.PathValue("name")); !ok {
			writeError(w, http.StatusNotFound, fmt.Errorf("network %q not found", r.PathValue("name")))
			return
		}
		n, err := netmgr.UpdateIngress(r.PathValue("name"), req.AllowedIngress)
		if err != nil {
			writeError(w, networkErrStatus(err), err)
			return
		}
		writeJSON(w, http.StatusOK, types.NewNetworkResponse(n))
	})

	// Live intra toggle: flips VM↔VM reachability. The flag is persisted
	// first, then every live TAP on the network converges (bridge-port
	// isolation). If any tap fails to converge we say so loudly — after a
	// tightening, a stale-reachable tap is a hole, not a detail.
	mux.HandleFunc("PUT /v1/networks/{name}/intra", func(w http.ResponseWriter, r *http.Request) {
		var req types.UpdateNetworkIntraRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		if _, ok := netmgr.Get(r.PathValue("name")); !ok {
			writeError(w, http.StatusNotFound, fmt.Errorf("network %q not found", r.PathValue("name")))
			return
		}
		n, err := netmgr.UpdateIntra(r.PathValue("name"), req.Intra)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		if err := mgr.SyncTapIsolation(n.Name, !req.Intra); err != nil {
			writeError(w, http.StatusInternalServerError, fmt.Errorf("intra flag saved, but not all live taps converged: %w", err))
			return
		}
		writeJSON(w, http.StatusOK, types.NewNetworkResponse(n))
	})

	mux.HandleFunc("DELETE /v1/networks/{name}", func(w http.ResponseWriter, r *http.Request) {
		if err := netmgr.Delete(r.PathValue("name")); err != nil {
			// A network that still has VMs attached is a client error (409),
			// not a server fault — surface it as a conflict.
			writeError(w, http.StatusConflict, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})

	// Bulk-cleanup step for DELETE /v1/networks/{name}, which otherwise
	// refuses while any VM is still attached — this clears them first.
	mux.HandleFunc("DELETE /v1/networks/{name}/vms", func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		if _, ok := netmgr.Get(name); !ok {
			writeError(w, http.StatusNotFound, fmt.Errorf("network %q not found", name))
			return
		}
		writeJSON(w, http.StatusOK, bulkDeleteResponse(mgr.DestroyByNetwork(r.Context(), name)))
	})

	// Volumes: persistent ext4 disks that outlive VMs — the data plane. A sample
	// attached read-only, or a writable disk that collects a detonation's
	// artifacts and can be read back after the VM is gone.
	mux.HandleFunc("POST /v1/volumes", func(w http.ResponseWriter, r *http.Request) {
		var req types.CreateVolumeRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		if req.Name == "" {
			writeError(w, http.StatusBadRequest, errors.New("name is required"))
			return
		}
		if req.SizeMB <= 0 {
			writeError(w, http.StatusBadRequest, errors.New("size_mb must be positive"))
			return
		}
		vol, err := mgr.CreateVolume(req)
		if err != nil {
			writeVMOpError(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, types.NewVolumeResponse(vol))
	})

	mux.HandleFunc("GET /v1/volumes", func(w http.ResponseWriter, r *http.Request) {
		vols := mgr.Volumes()
		resp := make([]types.VolumeResponse, 0, len(vols))
		for _, v := range vols {
			resp = append(resp, types.NewVolumeResponse(v))
		}
		writeJSON(w, http.StatusOK, resp)
	})

	mux.HandleFunc("GET /v1/volumes/{id}", func(w http.ResponseWriter, r *http.Request) {
		vol, ok := mgr.GetVolume(r.PathValue("id"))
		if !ok {
			writeError(w, http.StatusNotFound, fmt.Errorf("volume %q not found", r.PathValue("id")))
			return
		}
		writeJSON(w, http.StatusOK, types.NewVolumeResponse(vol))
	})

	mux.HandleFunc("DELETE /v1/volumes/{id}", func(w http.ResponseWriter, r *http.Request) {
		if err := mgr.DeleteVolume(r.PathValue("id")); err != nil {
			writeVMOpError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})

	// Offline I/O on a DETACHED volume, host-side via debugfs (never mounting the
	// guest fs on the host). Inject a sample before attaching it; extract results
	// after the VM that wrote them is gone. Refused while the volume is attached —
	// use the VM's live channel then (see /v1/vms/{id}/files).
	mux.HandleFunc("PUT /v1/volumes/{id}/files", func(w http.ResponseWriter, r *http.Request) {
		path, ok := guestPathParam(w, r)
		if !ok {
			return
		}
		defer r.Body.Close()
		if err := mgr.InjectToVolume(r.PathValue("id"), path, r.Body); err != nil {
			writeVMOpError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})

	mux.HandleFunc("GET /v1/volumes/{id}/files", func(w http.ResponseWriter, r *http.Request) {
		path, ok := guestPathParam(w, r)
		if !ok {
			return
		}
		rc, size, err := mgr.ExtractFromVolumeStream(r.PathValue("id"), path)
		if err != nil {
			writeVMOpError(w, err)
			return
		}
		streamFile(w, rc, size)
	})

	mux.HandleFunc("POST /v1/vms", func(w http.ResponseWriter, r *http.Request) {
		var req types.CreateVMRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		if req.Template == "" {
			writeError(w, http.StatusBadRequest, errors.New("template is required"))
			return
		}

		record, err := mgr.Create(r.Context(), req)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusCreated, liveVMResponse(record))
	})

	mux.HandleFunc("GET /v1/vms", func(w http.ResponseWriter, r *http.Request) {
		vms := mgr.List()
		resp := make([]types.VMResponse, 0, len(vms))
		for _, v := range vms {
			resp = append(resp, liveVMResponse(v))
		}
		writeJSON(w, http.StatusOK, resp)
	})

	mux.HandleFunc("GET /v1/vms/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		record, ok := mgr.Get(id)
		if !ok {
			writeError(w, http.StatusNotFound, fmt.Errorf("vm %q not found", id))
			return
		}
		writeJSON(w, http.StatusOK, liveVMResponse(record))
	})

	mux.HandleFunc("DELETE /v1/vms/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if err := mgr.Destroy(r.Context(), id); err != nil {
			writeError(w, http.StatusNotFound, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})

	// Bulk delete: resets the environment (e.g. between test runs) without
	// destroying VMs one at a time. Distinct registered pattern from
	// "DELETE /v1/vms/{id}" above — no path-matching ambiguity. Always 200,
	// even on partial failure: destroying many independent VMs isn't an
	// all-or-nothing operation, so the response body (not the status code)
	// carries which ones failed.
	mux.HandleFunc("DELETE /v1/vms", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, bulkDeleteResponse(mgr.DestroyAll(r.Context())))
	})

	// Stop powers a VM off but keeps its disk and IP (see vm.Manager.Stop) —
	// as opposed to DELETE /v1/vms/{id}, which erases everything. Start below
	// brings it back at the same address.
	mux.HandleFunc("POST /v1/vms/{id}/stop", func(w http.ResponseWriter, r *http.Request) {
		record, err := mgr.Stop(r.Context(), r.PathValue("id"))
		if err != nil {
			writeVMOpError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, liveVMResponse(record))
	})

	mux.HandleFunc("POST /v1/vms/{id}/start", func(w http.ResponseWriter, r *http.Request) {
		record, err := mgr.Start(r.Context(), r.PathValue("id"))
		if err != nil {
			writeVMOpError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, liveVMResponse(record))
	})

	// Snapshot: freeze a running VM's memory+disk as a restorable point in
	// time (the VM is paused sub-second and resumed). The snapshot is an
	// independent entity — it survives its source VM being destroyed.
	// Verb-style route (like stop/start/fork/restore/exec): the created
	// resource lives at /v1/snapshots/{id}, this is the action that makes it.
	mux.HandleFunc("POST /v1/vms/{id}/snapshot", func(w http.ResponseWriter, r *http.Request) {
		var req types.CreateSnapshotRequest
		// Body is optional (name only) — an empty body is a valid request.
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil && err != io.EOF {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		snap, err := mgr.Snapshot(r.Context(), r.PathValue("id"), req.Name)
		if err != nil {
			writeVMOpError(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, types.NewSnapshotResponse(snap))
	})

	mux.HandleFunc("GET /v1/snapshots", func(w http.ResponseWriter, r *http.Request) {
		snaps := mgr.Snapshots()
		resp := make([]types.SnapshotResponse, 0, len(snaps))
		for _, s := range snaps {
			resp = append(resp, types.NewSnapshotResponse(s))
		}
		writeJSON(w, http.StatusOK, resp)
	})

	mux.HandleFunc("GET /v1/snapshots/{id}", func(w http.ResponseWriter, r *http.Request) {
		snap, ok := mgr.GetSnapshot(r.PathValue("id"))
		if !ok {
			writeError(w, http.StatusNotFound, fmt.Errorf("snapshot %q not found", r.PathValue("id")))
			return
		}
		writeJSON(w, http.StatusOK, types.NewSnapshotResponse(snap))
	})

	mux.HandleFunc("DELETE /v1/snapshots/{id}", func(w http.ResponseWriter, r *http.Request) {
		if err := mgr.DeleteSnapshot(r.PathValue("id")); err != nil {
			writeVMOpError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})

	// Fork: create a NEW VM from a snapshot — the guest resumes mid-execution
	// where the snapshot froze it. Default mode rejoins the origin network at
	// the snapshot's IP (409 if taken); quarantine=true gives the fork a TAP
	// connected to nothing (vsock-only access, N simultaneous forks allowed).
	// Same verb, body and response as POST /v1/vms/{id}/fork below — the only
	// difference is what you fork from.
	mux.HandleFunc("POST /v1/snapshots/{id}/fork", func(w http.ResponseWriter, r *http.Request) {
		var req types.ForkVMRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil && err != io.EOF {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		record, err := mgr.Fork(r.Context(), r.PathValue("id"), req.Quarantine)
		if err != nil {
			writeVMOpError(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, liveVMResponse(record))
	})

	// Direct fork: clone a RUNNING VM in one call, no user-managed snapshot —
	// the manager takes an ephemeral snapshot, forks from it and deletes it.
	// Same body and network semantics as forking a snapshot; since the source
	// VM stays alive holding its IP, quarantine=true is the usual mode here.
	mux.HandleFunc("POST /v1/vms/{id}/fork", func(w http.ResponseWriter, r *http.Request) {
		var req types.ForkVMRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil && err != io.EOF {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		record, err := mgr.ForkVM(r.Context(), r.PathValue("id"), req.Quarantine)
		if err != nil {
			writeVMOpError(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, liveVMResponse(record))
	})

	// Restore: rewind THIS VM, in place, to one of its own snapshots — same
	// ID, IP and TAP, only memory+disk are rewound. The reset-to-clean
	// primitive between samples.
	mux.HandleFunc("POST /v1/vms/{id}/restore", func(w http.ResponseWriter, r *http.Request) {
		var req types.RestoreVMRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		if req.Snapshot == "" {
			writeError(w, http.StatusBadRequest, errors.New("snapshot is required"))
			return
		}
		record, err := mgr.Restore(r.Context(), r.PathValue("id"), req.Snapshot)
		if err != nil {
			writeVMOpError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, liveVMResponse(record))
	})

	mux.HandleFunc("POST /v1/vms/{id}/exec", func(w http.ResponseWriter, r *http.Request) {
		var req types.ExecRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		if req.Cmd == "" {
			writeError(w, http.StatusBadRequest, errors.New("cmd is required"))
			return
		}

		output, exitCode, err := mgr.Exec(r.PathValue("id"), req.Cmd)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, types.ExecResponse{Output: output, ExitCode: exitCode})
	})

	// File transfer to/from a VM. Transparent over the VM's state: running →
	// streamed over vsock (works even with no network), stopped → read/written
	// offline with debugfs straight on the disk. The bulk data channel and the
	// post-mortem artifact path in one endpoint.
	mux.HandleFunc("PUT /v1/vms/{id}/files", func(w http.ResponseWriter, r *http.Request) {
		path, ok := guestPathParam(w, r)
		if !ok {
			return
		}
		// Stream the body straight through: no io.ReadAll, so a multi-GB upload
		// costs a fixed buffer, not its size. r.ContentLength (-1 when unknown,
		// e.g. chunked) is passed on; the manager stages to disk to learn the
		// length only when the vsock path needs it and it wasn't given.
		if err := mgr.PutFile(r.PathValue("id"), path, r.Body, r.ContentLength); err != nil {
			writeVMOpError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})

	mux.HandleFunc("GET /v1/vms/{id}/files", func(w http.ResponseWriter, r *http.Request) {
		path, ok := guestPathParam(w, r)
		if !ok {
			return
		}
		rc, size, err := mgr.GetFileStream(r.PathValue("id"), path)
		if err != nil {
			writeVMOpError(w, err)
			return
		}
		streamFile(w, rc, size)
	})

	// No Addr: the caller owns the listener (see cmd/microhosted's apiListener),
	// because whether this is a Unix socket or a port is an access-control
	// decision, not an HTTP one.
	return &http.Server{Handler: logRequests(mux)}
}

// guestPathParam reads and validates the ?path= of a file endpoint, answering
// 400 itself when it is missing or unsafe. Validated here, for both channels,
// so a path is accepted or refused the same way whether the VM is running
// (vsock) or stopped (debugfs) — see storage.ValidateGuestPath for why a
// newline in it is a security matter, not a typo.
func guestPathParam(w http.ResponseWriter, r *http.Request) (string, bool) {
	path := r.URL.Query().Get("path")
	if path == "" {
		writeError(w, http.StatusBadRequest, errors.New("path query parameter is required"))
		return "", false
	}
	clean, err := storage.ValidateGuestPath(path)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return "", false
	}
	return clean, true
}

// streamFile copies an extracted file to the response in constant memory,
// advertising its length so clients get a real Content-Length rather than a
// chunked stream of unknown size. rc is always closed.
func streamFile(w http.ResponseWriter, rc io.ReadCloser, size int64) {
	defer rc.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, rc)
}

func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.Printf("%s %s", r.Method, r.URL.Path)
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

// writeVMOpError maps a vm.Manager lifecycle error to an HTTP status: an
// unknown VM or snapshot is 404, an operation invalid for the VM's current
// state (stop on a stopped VM, start on a running one) or colliding with
// current resources (forking onto a taken address) is 409 Conflict, anything
// else is 500.
func writeVMOpError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, vm.ErrVMNotFound), errors.Is(err, vm.ErrSnapshotNotFound):
		writeError(w, http.StatusNotFound, err)
	case errors.Is(err, vm.ErrVMState), errors.Is(err, vm.ErrConflict):
		writeError(w, http.StatusConflict, err)
	default:
		writeError(w, http.StatusInternalServerError, err)
	}
}

// bulkDeleteResponse converts a vm.Manager bulk-destroy result (IDs + a
// per-ID error) into the wire DTO.
func bulkDeleteResponse(deleted []string, failedErrs map[string]error) types.BulkDeleteResponse {
	resp := types.BulkDeleteResponse{Deleted: deleted}
	if len(failedErrs) > 0 {
		resp.Failed = make(map[string]string, len(failedErrs))
		for id, err := range failedErrs {
			resp.Failed[id] = err.Error()
		}
	}
	return resp
}

// networkErrStatus maps a network-manager error to its status: an ingress
// policy the caller got wrong (to_ip outside the subnet, a clash with another
// network) is only detectable inside the manager, but it is still a 400.
func networkErrStatus(err error) int {
	if errors.Is(err, network.ErrInvalidIngress) {
		return http.StatusBadRequest
	}
	return http.StatusInternalServerError
}
