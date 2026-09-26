package api

import (
	"errors"
	"net/http"

	"microhosted/internal/images"
	"microhosted/internal/labels"
	"microhosted/internal/vm"
	"microhosted/pkg/types"
)

// registerImageRoutes serves the content-addressed image store. {ref} is
// "name:version", "sha256:<hex>" or "name:version@sha256:<hex>".
func registerImageRoutes(mux *http.ServeMux, mgr *vm.Manager) {
	store := func(w http.ResponseWriter) *images.Store {
		s := mgr.Images()
		if s == nil {
			writeError(w, http.StatusNotFound, errors.New("this daemon has no image store"))
		}
		return s
	}

	// Import copies both files into the store and hashes the copies: seconds
	// for a large rootfs, once. Creating VMs from the image reads nothing.
	mux.HandleFunc("POST /v1/images", func(w http.ResponseWriter, r *http.Request) {
		s := store(w)
		if s == nil {
			return
		}
		var req types.ImportImageRequest
		if !decodeJSON(w, r, &req, false) {
			return
		}
		img, err := s.Import(req)
		if err != nil {
			writeError(w, imageErrStatus(err), err)
			return
		}
		writeJSON(w, http.StatusCreated, s.Response(img))
	})

	mux.HandleFunc("GET /v1/images", func(w http.ResponseWriter, r *http.Request) {
		s := store(w)
		if s == nil {
			return
		}
		list := s.List()
		resp := make([]types.ImageResponse, 0, len(list))
		for _, img := range list {
			resp = append(resp, s.Response(img))
		}
		writeJSON(w, http.StatusOK, resp)
	})

	mux.HandleFunc("GET /v1/images/{ref}", func(w http.ResponseWriter, r *http.Request) {
		s := store(w)
		if s == nil {
			return
		}
		img, err := s.Resolve(r.PathValue("ref"))
		if err != nil {
			writeError(w, imageErrStatus(err), err)
			return
		}
		writeJSON(w, http.StatusOK, s.Response(img))
	})

	// Refused (409) while any VM or snapshot boots from the image.
	mux.HandleFunc("DELETE /v1/images/{ref}", func(w http.ResponseWriter, r *http.Request) {
		s := store(w)
		if s == nil {
			return
		}
		if _, err := s.Delete(r.PathValue("ref"), mgr.ImageInUse); err != nil {
			writeError(w, imageErrStatus(err), err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})

	// Verify re-hashes the image's files: slow, explicit, never on the boot
	// path. 200 with ok=false when the content no longer matches.
	mux.HandleFunc("POST /v1/images/{ref}/verify", func(w http.ResponseWriter, r *http.Request) {
		s := store(w)
		if s == nil {
			return
		}
		img, err := s.Resolve(r.PathValue("ref"))
		if err != nil {
			writeError(w, imageErrStatus(err), err)
			return
		}
		resp := types.ImageVerifyResponse{Digest: img.Digest, OK: true}
		if err := s.Verify(img); err != nil {
			resp.OK, resp.Error = false, err.Error()
		}
		writeJSON(w, http.StatusOK, resp)
	})
}

func imageErrStatus(err error) int {
	switch {
	case errors.Is(err, labels.ErrInvalid):
		return http.StatusBadRequest
	case errors.Is(err, images.ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, images.ErrConflict):
		return http.StatusConflict
	case errors.Is(err, vm.ErrCapacity):
		return http.StatusServiceUnavailable
	}
	return http.StatusInternalServerError
}
