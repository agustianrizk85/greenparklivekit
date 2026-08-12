// Package domain berisi tipe inti layanan LiveKit Greenpark: rapat/room
// (meeting), undangan peserta, rekaman/streaming (egress), dan jejak event
// webhook. Tidak bergantung pada SDK LiveKit maupun transport HTTP.
package domain

import (
	"errors"
	"strings"
	"time"
)

// Error sentinel — dipetakan ke status HTTP di transport.
var (
	ErrNotFound   = errors.New("data tidak ditemukan")
	ErrValidation = errors.New("data tidak lengkap")
	ErrConflict   = errors.New("data sudah ada")
	ErrForbidden  = errors.New("tidak punya akses")
	ErrDisabled   = errors.New("LiveKit belum dikonfigurasi")
)

// Kind membedakan peruntukan sebuah room.
type Kind string

const (
	KindMeeting Kind = "meeting" // rapat video/audio antar divisi
	KindStream  Kind = "stream"  // siaran satu-ke-banyak / share layar
	KindVoiceAI Kind = "voiceai" // sesi voice AI agent
)

// Status siklus hidup meeting.
type Status string

const (
	StatusScheduled Status = "scheduled"
	StatusLive      Status = "live"
	StatusEnded     Status = "ended"
)

// Role peserta di dalam room (menentukan grant token LiveKit).
type Role string

const (
	RoleHost    Role = "host"    // boleh publish + admin room
	RoleSpeaker Role = "speaker" // boleh publish
	RoleViewer  Role = "viewer"  // hanya subscribe
	RoleAgent   Role = "agent"   // worker AI (publish audio + data)
)

// CanPublish melaporkan apakah role boleh mengirim media.
func (r Role) CanPublish() bool { return r == RoleHost || r == RoleSpeaker || r == RoleAgent }

// IsAdmin melaporkan apakah role memegang kendali room (kick/mute/akhiri).
func (r Role) IsAdmin() bool { return r == RoleHost }

// Valid melaporkan apakah nilai role dikenal.
func (r Role) Valid() bool {
	switch r {
	case RoleHost, RoleSpeaker, RoleViewer, RoleAgent:
		return true
	}
	return false
}

// User adalah identitas pemanggil hasil pemetaan klaim SSO.
type User struct {
	ID       string            `json:"id"`
	Username string            `json:"username"`
	Name     string            `json:"name"`
	Email    string            `json:"email,omitempty"`
	Super    bool              `json:"super"`
	Roles    map[string]string `json:"roles,omitempty"` // divisi -> peran
}

// Divisions mengembalikan daftar kode divisi yang dimiliki user.
func (u User) Divisions() []string {
	out := make([]string, 0, len(u.Roles))
	for d := range u.Roles {
		out = append(out, d)
	}
	return out
}

// InDivision melaporkan apakah user punya peran di divisi tersebut.
func (u User) InDivision(dept string) bool {
	if u.Super || dept == "" {
		return true
	}
	_, ok := u.Roles[dept]
	return ok
}

// IsDirector melaporkan apakah user berperan lintas divisi (ceo/dirops/admin),
// yang berhak mengelola room milik divisi mana pun.
func (u User) IsDirector() bool {
	if u.Super {
		return true
	}
	for _, role := range u.Roles {
		switch strings.ToLower(role) {
		case "ceo", "dirops", "admin", "direktur":
			return true
		}
	}
	return false
}

// Invitee adalah peserta yang diundang beserta perannya di room.
type Invitee struct {
	UserID   string `json:"userId,omitempty"`
	Username string `json:"username"`
	Name     string `json:"name,omitempty"`
	Division string `json:"division,omitempty"`
	Role     Role   `json:"role"`
}

// Recording mencatat satu egress (rekaman file atau streaming RTMP).
type Recording struct {
	EgressID  string     `json:"egressId"`
	Kind      string     `json:"kind"` // "record" | "stream"
	Status    string     `json:"status"`
	Layout    string     `json:"layout,omitempty"`
	Filepath  string     `json:"filepath,omitempty"`
	URLs      []string   `json:"urls,omitempty"`
	StartedAt time.Time  `json:"startedAt"`
	EndedAt   *time.Time `json:"endedAt,omitempty"`
	StartedBy string     `json:"startedBy,omitempty"`
	Error     string     `json:"error,omitempty"`
}

// Meeting adalah satu room LiveKit beserta metadata organisasinya.
type Meeting struct {
	ID          string `json:"id"`
	Room        string `json:"room"` // nama room LiveKit, unik
	Title       string `json:"title"`
	Description string `json:"description,omitempty"`
	Kind        Kind   `json:"kind"`
	Status      Status `json:"status"`

	Division  string   `json:"division"`            // divisi pemilik
	Divisions []string `json:"divisions,omitempty"` // divisi yang diundang (kosong = semua)
	Open      bool     `json:"open"`                // true = semua karyawan boleh gabung

	HostID   string `json:"hostId,omitempty"`
	HostName string `json:"hostName,omitempty"`

	ScheduledAt *time.Time `json:"scheduledAt,omitempty"`
	StartedAt   *time.Time `json:"startedAt,omitempty"`
	EndedAt     *time.Time `json:"endedAt,omitempty"`

	MaxParticipants uint32 `json:"maxParticipants,omitempty"`
	EmptyTimeout    uint32 `json:"emptyTimeout,omitempty"` // detik
	RecordEnabled   bool   `json:"recordEnabled"`
	AgentEnabled    bool   `json:"agentEnabled"` // voice AI agent
	AgentName       string `json:"agentName,omitempty"`
	AgentMetadata   string `json:"agentMetadata,omitempty"`

	Invitees   []Invitee   `json:"invitees,omitempty"`
	Recordings []Recording `json:"recordings,omitempty"`

	NumParticipants int `json:"numParticipants"` // cache dari webhook

	CreatedBy string    `json:"createdBy,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// RoleFor mengembalikan peran user di meeting ini, dan apakah ia berhak masuk.
func (m Meeting) RoleFor(u User) (Role, bool) {
	if u.ID != "" && u.ID == m.HostID {
		return RoleHost, true
	}
	for _, inv := range m.Invitees {
		if (inv.UserID != "" && inv.UserID == u.ID) || (inv.Username != "" && strings.EqualFold(inv.Username, u.Username)) {
			role := inv.Role
			if !role.Valid() {
				role = RoleViewer
			}
			return role, true
		}
	}
	if u.IsDirector() {
		return RoleHost, true
	}
	if m.Open {
		if m.Kind == KindStream {
			return RoleViewer, true
		}
		return RoleSpeaker, true
	}
	for _, d := range m.Divisions {
		if u.InDivision(d) {
			if m.Kind == KindStream {
				return RoleViewer, true
			}
			return RoleSpeaker, true
		}
	}
	if len(m.Divisions) == 0 && m.Division != "" && u.InDivision(m.Division) {
		if m.Kind == KindStream {
			return RoleViewer, true
		}
		return RoleSpeaker, true
	}
	return "", false
}

// CanManage melaporkan apakah user boleh mengubah/mengakhiri meeting.
func (m Meeting) CanManage(u User) bool {
	if u.IsDirector() {
		return true
	}
	if u.ID != "" && u.ID == m.HostID {
		return true
	}
	for _, inv := range m.Invitees {
		if inv.Role == RoleHost && ((inv.UserID != "" && inv.UserID == u.ID) || strings.EqualFold(inv.Username, u.Username)) {
			return true
		}
	}
	return false
}

// Visible melaporkan apakah meeting boleh muncul di daftar milik user.
func (m Meeting) Visible(u User) bool {
	if u.IsDirector() || m.Open {
		return true
	}
	if _, ok := m.RoleFor(u); ok {
		return true
	}
	return false
}

// MeetingFilter menyaring daftar meeting di store.
type MeetingFilter struct {
	Division string
	Status   Status
	Kind     Kind
	Room     string
	Query    string
}

// Event adalah jejak webhook LiveKit (ring buffer, untuk audit + realtime FE).
type Event struct {
	ID       string    `json:"id"`
	Type     string    `json:"type"`
	Room     string    `json:"room,omitempty"`
	Identity string    `json:"identity,omitempty"`
	EgressID string    `json:"egressId,omitempty"`
	At       time.Time `json:"at"`
	Detail   string    `json:"detail,omitempty"`
}
