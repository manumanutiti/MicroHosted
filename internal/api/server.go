package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"

	"microhosted/internal/network"
	"microhosted/internal/vm"
	"microhosted/pkg/types"
)

// NewServer builds the HTTP API meant to sit behind a future web panel:
// create/list/destroy VMs and browse the template catalog. This is the
// integration point Sesión 7 originally planned for — it exists from the
// start here so a panel can be built against it without reshaping the
// manager underneath.
func NewServer(mgr *vm.Manager, netmgr *network.Manager, addr string) *http.Server {
	mux := http.NewServeMux()

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
		n, err := netmgr.Create(req)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
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
		writeJSON(w, http.StatusCreated, types.NewVMResponse(record))
	})

	mux.HandleFunc("GET /v1/vms", func(w http.ResponseWriter, r *http.Request) {
		vms := mgr.List()
		resp := make([]types.VMResponse, 0, len(vms))
		for _, v := range vms {
			resp = append(resp, types.NewVMResponse(v))
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
		writeJSON(w, http.StatusOK, types.NewVMResponse(record))
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
		writeJSON(w, http.StatusOK, types.NewVMResponse(record))
	})

	mux.HandleFunc("POST /v1/vms/{id}/start", func(w http.ResponseWriter, r *http.Request) {
		record, err := mgr.Start(r.Context(), r.PathValue("id"))
		if err != nil {
			writeVMOpError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, types.NewVMResponse(record))
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
		writeJSON(w, http.StatusCreated, types.NewVMResponse(record))
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
		writeJSON(w, http.StatusCreated, types.NewVMResponse(record))
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
		writeJSON(w, http.StatusOK, types.NewVMResponse(record))
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

	return &http.Server{Addr: addr, Handler: logRequests(mux)}
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
