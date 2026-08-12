package http

import (
	"context"
	"net/http"
	"strconv"
	"strings"

	"greenpark/livekit/internal/authmw"
	"greenpark/livekit/internal/domain"
	"greenpark/livekit/internal/service"
)

// Handler menautkan HTTP ke service, termasuk penjagaan akses berbasis SSO.
type Handler struct {
	svc    *service.Service
	verify *authmw.Verifier
	hub    *hub
}

// NewHandler membuat handler beserta hub realtime-nya.
func NewHandler(svc *service.Service, verify *authmw.Verifier) *Handler {
	h := &Handler{svc: svc, verify: verify, hub: newHub()}
	svc.SetBroadcaster(h.hub)
	return h
}

type ctxKey int

const userCtxKey ctxKey = 0

func bearer(r *http.Request) string {
	v := r.Header.Get("Authorization")
	if strings.HasPrefix(v, "Bearer ") {
		return strings.TrimSpace(v[len("Bearer "):])
	}
	return ""
}

// userFromClaims memetakan klaim SSO ke identitas domain. Layanan ini lintas
// divisi: siapa pun yang punya peran di divisi mana pun boleh memakainya.
func userFromClaims(c authmw.Claims) domain.User {
	return domain.User{
		ID:       c.Subject,
		Username: c.Username,
		Name:     c.Name,
		Email:    c.Email,
		Super:    c.Super,
		Roles:    c.Roles,
	}
}

// requireAuth hanya menuntut token SSO yang sah. Sengaja TIDAK menuntut
// keanggotaan divisi: layanan panggilan ini lintas divisi, dan di produksi ada
// akun sah yang peta `roles`-nya kosong (mis. direktur yang belum didaftarkan ke
// departemen mana pun). Menolak mereka membuat tombol panggilan mati dengan
// pesan "belum dikonfigurasi" yang menyesatkan. Pembatasan yang sesungguhnya
// ada di lapisan meeting: siapa yang diundang, siapa host — lihat
// domain.Meeting.RoleFor.
func (h *Handler) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims, err := h.verify.Verify(bearer(r))
		if err != nil {
			writeError(w, http.StatusUnauthorized, err.Error())
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), userCtxKey, userFromClaims(claims))))
	}
}

// requireDirector membatasi endpoint ke super/ceo/dirops/admin.
func (h *Handler) requireDirector(next http.HandlerFunc) http.HandlerFunc {
	return h.requireAuth(func(w http.ResponseWriter, r *http.Request) {
		if u := caller(r); !u.IsDirector() {
			writeError(w, http.StatusForbidden, "butuh akses direksi/admin")
			return
		}
		next(w, r)
	})
}

func caller(r *http.Request) domain.User {
	u, _ := r.Context().Value(userCtxKey).(domain.User)
	return u
}

// ---------- dasar ----------

func (h *Handler) health(w http.ResponseWriter, _ *http.Request) {
	cfg := h.svc.Config()
	writeJSON(w, http.StatusOK, map[string]any{
		"status":  "ok",
		"service": "livekit",
		"livekit": cfg.Enabled,
		"stats":   h.svc.Stats(),
	})
}

func (h *Handler) config(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, h.svc.Config())
}

func (h *Handler) me(w http.ResponseWriter, r *http.Request) {
	u := caller(r)
	writeJSON(w, http.StatusOK, map[string]any{
		"id": u.ID, "username": u.Username, "name": u.Name, "email": u.Email,
		"super": u.Super, "roles": u.Roles, "direktur": u.IsDirector(),
	})
}

func (h *Handler) ws(w http.ResponseWriter, r *http.Request) {
	h.hub.serve(w, r, func(tok string) bool {
		_, err := h.verify.Verify(tok)
		return err == nil // aturan sama dengan requireAuth: token sah sudah cukup
	})
}

// ---------- meeting ----------

func (h *Handler) listMeetings(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := domain.MeetingFilter{
		Division: q.Get("division"),
		Status:   domain.Status(q.Get("status")),
		Kind:     domain.Kind(q.Get("kind")),
		Room:     q.Get("room"),
		Query:    q.Get("q"),
	}
	writeJSON(w, http.StatusOK, h.svc.ListMeetings(caller(r), f))
}

func (h *Handler) createMeeting(w http.ResponseWriter, r *http.Request) {
	var in service.MeetingInput
	if err := decode(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, "JSON tidak valid: "+err.Error())
		return
	}
	m, err := h.svc.CreateMeeting(r.Context(), caller(r), in)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, m)
}

func (h *Handler) getMeeting(w http.ResponseWriter, r *http.Request) {
	m, err := h.svc.GetMeeting(caller(r), r.PathValue("id"))
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, m)
}

func (h *Handler) updateMeeting(w http.ResponseWriter, r *http.Request) {
	var in service.MeetingInput
	if err := decode(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, "JSON tidak valid: "+err.Error())
		return
	}
	m, err := h.svc.UpdateMeeting(caller(r), r.PathValue("id"), in)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, m)
}

func (h *Handler) deleteMeeting(w http.ResponseWriter, r *http.Request) {
	if err := h.svc.DeleteMeeting(r.Context(), caller(r), r.PathValue("id")); err != nil {
		writeServiceError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) joinMeeting(w http.ResponseWriter, r *http.Request) {
	res, err := h.svc.Join(r.Context(), caller(r), r.PathValue("id"))
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (h *Handler) startMeeting(w http.ResponseWriter, r *http.Request) {
	m, err := h.svc.Start(r.Context(), caller(r), r.PathValue("id"))
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, m)
}

func (h *Handler) endMeeting(w http.ResponseWriter, r *http.Request) {
	m, err := h.svc.End(r.Context(), caller(r), r.PathValue("id"))
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, m)
}

func (h *Handler) participants(w http.ResponseWriter, r *http.Request) {
	list, err := h.svc.Participants(r.Context(), caller(r), r.PathValue("id"))
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (h *Handler) kick(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Identity string `json:"identity"`
	}
	if err := decode(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "JSON tidak valid: "+err.Error())
		return
	}
	if err := h.svc.Kick(r.Context(), caller(r), r.PathValue("id"), body.Identity); err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (h *Handler) mute(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Identity string `json:"identity"`
		TrackSID string `json:"trackSid"`
		Muted    *bool  `json:"muted"`
	}
	if err := decode(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "JSON tidak valid: "+err.Error())
		return
	}
	muted := true
	if body.Muted != nil {
		muted = *body.Muted
	}
	if err := h.svc.Mute(r.Context(), caller(r), r.PathValue("id"), body.Identity, body.TrackSID, muted); err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "muted": muted})
}

// ---------- egress ----------

func (h *Handler) startRecord(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Layout string `json:"layout"`
	}
	if err := decode(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "JSON tidak valid: "+err.Error())
		return
	}
	info, err := h.svc.StartRecord(r.Context(), caller(r), r.PathValue("id"), body.Layout)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, info)
}

func (h *Handler) startStream(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Layout string   `json:"layout"`
		URLs   []string `json:"urls"`
		URL    string   `json:"url"`
	}
	if err := decode(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "JSON tidak valid: "+err.Error())
		return
	}
	urls := body.URLs
	if body.URL != "" {
		urls = append(urls, body.URL)
	}
	info, err := h.svc.StartStream(r.Context(), caller(r), r.PathValue("id"), body.Layout, urls)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, info)
}

func (h *Handler) stopEgress(w http.ResponseWriter, r *http.Request) {
	var body struct {
		EgressID string `json:"egressId"`
	}
	if err := decode(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "JSON tidak valid: "+err.Error())
		return
	}
	list, err := h.svc.StopEgress(r.Context(), caller(r), r.PathValue("id"), body.EgressID)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (h *Handler) listEgress(w http.ResponseWriter, r *http.Request) {
	activeOnly := r.URL.Query().Get("active") == "1" || r.URL.Query().Get("active") == "true"
	list, err := h.svc.ListEgress(r.Context(), caller(r), r.PathValue("id"), activeOnly)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

// ---------- voice AI agent ----------

func (h *Handler) dispatchAgent(w http.ResponseWriter, r *http.Request) {
	var body struct {
		AgentName string `json:"agentName"`
		Metadata  string `json:"metadata"`
	}
	if err := decode(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "JSON tidak valid: "+err.Error())
		return
	}
	id, err := h.svc.DispatchAgent(r.Context(), caller(r), r.PathValue("id"), body.AgentName, body.Metadata)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"dispatchId": id})
}

// ---------- token ad-hoc & room mentah ----------

func (h *Handler) token(w http.ResponseWriter, r *http.Request) {
	var req service.TokenRequest
	if err := decode(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "JSON tidak valid: "+err.Error())
		return
	}
	res, err := h.svc.AdHocToken(r.Context(), caller(r), req)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (h *Handler) listRooms(w http.ResponseWriter, r *http.Request) {
	rooms, err := h.svc.Rooms(r.Context(), caller(r))
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, rooms)
}

func (h *Handler) deleteRoom(w http.ResponseWriter, r *http.Request) {
	if err := h.svc.DeleteRoom(r.Context(), caller(r), r.PathValue("name")); err != nil {
		writeServiceError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---------- event & webhook ----------

func (h *Handler) events(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	writeJSON(w, http.StatusOK, h.svc.Events(caller(r), r.URL.Query().Get("room"), limit))
}

// webhook menerima event dari server LiveKit. Endpoint ini publik tetapi
// setiap permintaan diverifikasi dengan tanda tangan API key/secret LiveKit.
func (h *Handler) webhook(w http.ResponseWriter, r *http.Request) {
	ev, err := h.svc.ParseWebhook(r)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "webhook tidak sah: "+err.Error())
		return
	}
	h.svc.HandleWebhook(ev)
	w.WriteHeader(http.StatusOK)
}
