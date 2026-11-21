package httpapi

import (
	"clusbot/internal/work"
	"errors"
	"io"
	"mime"
	"net/http"
	"strconv"
)

func (s Server) materialFiles(w http.ResponseWriter, r *http.Request) {
	files, err := s.Store.MaterialFiles(r.URL.Query().Get("q"))
	if err != nil {
		writeError(w, err)
		return
	}
	jsonResponse(w, 200, files)
}

func (s Server) captureMaterial(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Path string `json:"path"`
	}
	if !decode(w, r, &input) {
		return
	}
	m, err := s.Store.CaptureMaterial(r.Context(), input.Path)
	if err != nil {
		writeError(w, err)
		return
	}
	jsonResponse(w, 201, m)
}

func (s Server) uploadMaterial(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, work.MaxUploadBytes+64*1024)
	reader, err := r.MultipartReader()
	if err != nil {
		writeError(w, work.ErrUpload)
		return
	}
	part, err := reader.NextPart()
	if err != nil {
		uploadReadError(w, err)
		return
	}
	_, disposition, err := mime.ParseMediaType(part.Header.Get("Content-Disposition"))
	if err != nil || part.FormName() != "file" || disposition["filename"] == "" {
		writeError(w, work.ErrUpload)
		return
	}
	body, err := io.ReadAll(io.LimitReader(part, work.MaxUploadBytes+1))
	if err != nil {
		uploadReadError(w, err)
		return
	}
	if len(body) > work.MaxUploadBytes {
		writeError(w, work.ErrUploadSize)
		return
	}
	if _, err = reader.NextPart(); !errors.Is(err, io.EOF) {
		uploadReadError(w, err)
		return
	}
	contentType := http.DetectContentType(body)
	if contentType == "image/png" || contentType == "image/jpeg" {
		settings, err := s.Settings.View(r.Context())
		if err != nil {
			writeError(w, err)
			return
		}
		if settings.Mode == "demo" || settings.Mode == "unconfigured" ||
			settings.Attributes == nil || settings.Attributes.Vision == nil || !*settings.Attributes.Vision {
			writeError(w, work.ErrImageUnsupported)
			return
		}
		material, err := s.Store.UploadImage(r.Context(), disposition["filename"], body)
		if err != nil {
			writeError(w, err)
			return
		}
		jsonResponse(w, 201, material)
		return
	}
	material, err := s.Store.UploadMaterial(r.Context(), disposition["filename"], body)
	if err != nil {
		writeError(w, err)
		return
	}
	jsonResponse(w, 201, material)
}

func uploadReadError(w http.ResponseWriter, err error) {
	var large *http.MaxBytesError
	if errors.As(err, &large) {
		writeError(w, work.ErrUploadSize)
		return
	}
	writeError(w, work.ErrUpload)
}

func (s Server) selectUploadPart(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Part int `json:"part"`
	}
	if !decode(w, r, &input) {
		return
	}
	id := r.PathValue("id")
	if !validID(id) {
		writeError(w, work.ErrUploadPart)
		return
	}
	material, err := s.Store.UploadMaterialPart(r.Context(), id, input.Part)
	if err != nil {
		writeError(w, err)
		return
	}
	jsonResponse(w, 200, material)
}

func (s Server) previewMaterial(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validID(id) {
		fail(w, http.StatusBadRequest, "invalid_material", "材料标识无效")
		return
	}
	limit := 20
	if q := r.URL.Query().Get("limit"); q != "" {
		if n, err := strconv.Atoi(q); err == nil && n > 0 && n <= 100 {
			limit = n
		}
	}
	preview, err := s.Store.PreviewMaterial(r.Context(), id, limit)
	if err != nil {
		writeError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	jsonResponse(w, http.StatusOK, preview)
}

func (s Server) deletePendingMaterial(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validID(id) {
		fail(w, http.StatusBadRequest, "invalid_material", "材料标识无效")
		return
	}
	if err := s.Store.DeletePendingMaterial(r.Context(), id); err != nil {
		writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
