package http

import (
	"encoding/json"
	"net/http"
)

/* Endpoint TAUTAN TAMU.
 *
 * Dua kelompok yang sengaja dipisah berkasnya:
 *   - dikelola host (requireAuth): menyalakan/mematikan & membaca tautannya
 *   - PUBLIK (tanpa auth sama sekali): dipakai orang luar untuk bergabung
 *
 * Jalur publik hanya dua, keduanya bertumpu pada token acak sebagai satu-satunya
 * bukti berhak — pembatasannya ada di internal/service/guest.go.
 */

// POST /api/meetings/{id}/guest — nyalakan / rotasi tautan tamu (host).
//
// Tidak ada isi permintaan yang dibaca: metodenyalah yang menyatakan maksud
// (POST = nyalakan, DELETE = matikan). Menerima flag di body hanya menambah
// keadaan yang bisa bertentangan dengan metodenya.
func (h *Handler) guestLinkOn(w http.ResponseWriter, r *http.Request) {
	m, token, code, err := h.svc.GuestLink(caller(r), r.PathValue("id"), true)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"guestToken": token, "guestCode": code, "meeting": m})
}

// DELETE /api/meetings/{id}/guest — matikan tautan tamu (host).
func (h *Handler) guestLinkOff(w http.ResponseWriter, r *http.Request) {
	m, _, _, err := h.svc.GuestLink(caller(r), r.PathValue("id"), false)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"guestToken": "", "guestCode": "", "meeting": m})
}

// GET /api/meetings/{id}/guest — baca token yang berlaku (host).
func (h *Handler) guestLinkGet(w http.ResponseWriter, r *http.Request) {
	token, code, err := h.svc.GuestToken(caller(r), r.PathValue("id"))
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"guestToken": token, "guestCode": code})
}

/* ---- PUBLIK: tanpa autentikasi ------------------------------------------- */

// GET /api/tamu/{token} — keterangan minimal untuk layar persiapan tamu.
func (h *Handler) guestMeeting(w http.ResponseWriter, r *http.Request) {
	info, err := h.svc.GuestMeeting(r.PathValue("token"))
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, info)
}

type guestJoinBody struct {
	Name string `json:"name"`
	// Code = kode pendek yang diketik tamu. Kosong sah untuk tautan lama yang
	// dibuat sebelum kode ada.
	Code string `json:"code"`
}

// POST /api/tamu/{token}/join — terbitkan token LiveKit untuk tamu.
func (h *Handler) guestJoin(w http.ResponseWriter, r *http.Request) {
	var body guestJoinBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "isi permintaan tidak valid")
		return
	}
	res, err := h.svc.JoinAsGuest(r.Context(), r.PathValue("token"), body.Name, body.Code)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}
