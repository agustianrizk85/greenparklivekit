package http

import "net/http"

// NewRouter merangkai seluruh rute beserta middleware global.
//
// Tingkat akses:
//   - publik: health + webhook LiveKit (webhook diverifikasi tanda tangan)
//   - terautentikasi: siapa pun yang punya peran di divisi mana pun
//   - host/direksi: pengelolaan meeting (dijaga di lapisan service)
//   - direksi: room mentah di server LiveKit
func NewRouter(h *Handler, allowOrigin string) http.Handler {
	mux := http.NewServeMux()

	// publik
	mux.HandleFunc("GET /api/health", h.health)
	mux.HandleFunc("POST /api/livekit/webhook", h.webhook)

	// PUBLIK — tautan tamu (peserta luar tanpa akun Greenpark).
	// Token acak di URL adalah SATU-SATUNYA bukti berhak di sini, dan tamu tidak
	// pernah mendapat hak admin room. Pembatasan lengkapnya di service/guest.go.
	mux.HandleFunc("GET /api/tamu/{token}", h.guestMeeting)
	mux.HandleFunc("POST /api/tamu/{token}/join", h.guestJoin)

	// sesi
	mux.HandleFunc("GET /api/config", h.requireAuth(h.config))
	mux.HandleFunc("GET /api/auth/me", h.requireAuth(h.me))

	// realtime — auth lewat ?token=
	mux.HandleFunc("GET /api/ws", h.ws)

	// meeting
	mux.HandleFunc("GET /api/meetings", h.requireAuth(h.listMeetings))
	mux.HandleFunc("POST /api/meetings", h.requireAuth(h.createMeeting))
	mux.HandleFunc("GET /api/meetings/{id}", h.requireAuth(h.getMeeting))
	mux.HandleFunc("PATCH /api/meetings/{id}", h.requireAuth(h.updateMeeting))
	mux.HandleFunc("PUT /api/meetings/{id}", h.requireAuth(h.updateMeeting))
	mux.HandleFunc("DELETE /api/meetings/{id}", h.requireAuth(h.deleteMeeting))

	// masuk room & kendali sesi
	mux.HandleFunc("POST /api/meetings/{id}/join", h.requireAuth(h.joinMeeting))
	mux.HandleFunc("POST /api/meetings/{id}/start", h.requireAuth(h.startMeeting))
	mux.HandleFunc("POST /api/meetings/{id}/end", h.requireAuth(h.endMeeting))
	mux.HandleFunc("GET /api/meetings/{id}/participants", h.requireAuth(h.participants))
	mux.HandleFunc("POST /api/meetings/{id}/kick", h.requireAuth(h.kick))
	mux.HandleFunc("POST /api/meetings/{id}/mute", h.requireAuth(h.mute))

	// tautan tamu — dikelola host/direksi (dijaga CanManage di service)
	mux.HandleFunc("GET /api/meetings/{id}/guest", h.requireAuth(h.guestLinkGet))
	mux.HandleFunc("POST /api/meetings/{id}/guest", h.requireAuth(h.guestLinkOn))
	mux.HandleFunc("DELETE /api/meetings/{id}/guest", h.requireAuth(h.guestLinkOff))

	// egress: rekam & streaming
	mux.HandleFunc("GET /api/meetings/{id}/egress", h.requireAuth(h.listEgress))
	mux.HandleFunc("POST /api/meetings/{id}/record/start", h.requireAuth(h.startRecord))
	mux.HandleFunc("POST /api/meetings/{id}/record/stop", h.requireAuth(h.stopEgress))
	mux.HandleFunc("POST /api/meetings/{id}/stream/start", h.requireAuth(h.startStream))
	mux.HandleFunc("POST /api/meetings/{id}/stream/stop", h.requireAuth(h.stopEgress))

	// voice AI agent
	mux.HandleFunc("POST /api/meetings/{id}/agent", h.requireAuth(h.dispatchAgent))

	// token ad-hoc
	mux.HandleFunc("POST /api/token", h.requireAuth(h.token))

	// room mentah (direksi)
	mux.HandleFunc("GET /api/rooms", h.requireDirector(h.listRooms))
	mux.HandleFunc("DELETE /api/rooms/{name}", h.requireDirector(h.deleteRoom))

	// jejak event
	mux.HandleFunc("GET /api/events", h.requireAuth(h.events))

	return chain(mux, logger, cors(allowOrigin))
}
