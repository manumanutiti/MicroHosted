package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"

	"microhosted/internal/vm"
	"microhosted/pkg/types"
)

// NewServer builds the HTTP API meant to sit behind a future web panel:
// create/list/destroy VMs and browse the template catalog. This is the
// integration point Sesión 7 originally planned for — it exists from the
// start here so a panel can be built against it without reshaping the
// manager underneath.
func NewServer(mgr *vm.Manager, addr string) *http.Server {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /v1/templates", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, mgr.Templates())
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
