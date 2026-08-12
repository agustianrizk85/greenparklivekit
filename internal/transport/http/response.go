package http

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"

	"greenpark/livekit/internal/domain"
)

type errorBody struct {
	Error string `json:"error"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("livekit: encode response: %v", err)
	}
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, errorBody{Error: msg})
}

// writeServiceError memetakan error domain ke status HTTP yang tepat.
func writeServiceError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, domain.ErrNotFound):
		writeError(w, http.StatusNotFound, "data tidak ditemukan")
	case errors.Is(err, domain.ErrValidation):
		writeError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, domain.ErrConflict):
		writeError(w, http.StatusConflict, err.Error())
	case errors.Is(err, domain.ErrForbidden):
		writeError(w, http.StatusForbidden, "tidak punya akses")
	case errors.Is(err, domain.ErrDisabled):
		writeError(w, http.StatusServiceUnavailable, "LiveKit belum dikonfigurasi: isi LIVEKIT_API_KEY dan LIVEKIT_API_SECRET")
	default:
		log.Printf("livekit: %v", err)
		writeError(w, http.StatusInternalServerError, "kesalahan server: "+err.Error())
	}
}

// decode membaca body JSON ke v; body kosong dianggap objek kosong.
func decode(r *http.Request, v any) error {
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	if err := dec.Decode(v); err != nil {
		if errors.Is(err, io.EOF) {
			return nil
		}
		return err
	}
	return nil
}
