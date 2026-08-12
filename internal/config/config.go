// Package config memuat konfigurasi runtime dari environment variable (dengan
// dukungan file .env di folder kerja / di samping executable), memakai default
// yang masuk akal supaya service langsung jalan untuk pengembangan lokal.
package config

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Port        string
	AllowOrigin string
	DataPath    string

	// Master auth (SSO): verifikasi access token via kunci publik auth pusat.
	AuthJWKSURL string
	AuthIssuer  string
	Department  string // kode departemen service ini (untuk pesan/guard authmw)

	// LiveKit (Cloud maupun self-hosted).
	LiveKitURL       string // wss://xxx.livekit.cloud atau ws://localhost:7880
	LiveKitAPIKey    string
	LiveKitAPISecret string
	TokenTTL         time.Duration

	// Egress
	RecordDir string // prefix path file rekaman (self-hosted / Cloud file output)

	// Voice AI agent (LiveKit Agents)
	AgentName string
}

func Load() Config {
	loadDotEnv()
	return Config{
		Port:        getenv("LIVEKIT_BE_PORT", "8089"),
		AllowOrigin: getenv("LIVEKIT_ALLOW_ORIGIN", "*"),
		DataPath:    getenv("LIVEKIT_DATA_PATH", "data/livekit-data.json"),

		AuthJWKSURL: getenv("AUTH_JWKS_URL", "http://localhost:8090/.well-known/jwks.json"),
		AuthIssuer:  getenv("AUTH_ISSUER", "greenpark-auth"),
		Department:  getenv("LIVEKIT_DEPARTMENT", "livekit"),

		LiveKitURL:       getenv("LIVEKIT_URL", "ws://localhost:7880"),
		LiveKitAPIKey:    os.Getenv("LIVEKIT_API_KEY"),
		LiveKitAPISecret: os.Getenv("LIVEKIT_API_SECRET"),
		TokenTTL:         getdur("LIVEKIT_TOKEN_TTL_MIN", 240) * time.Minute,

		RecordDir: getenv("LIVEKIT_RECORD_DIR", "recordings"),
		AgentName: getenv("LIVEKIT_AGENT_NAME", ""),
	}
}

// Configured melaporkan apakah kredensial LiveKit sudah diisi. Tanpa ini,
// service tetap jalan (CRUD meeting bisa dipakai) tapi endpoint token/room/
// egress akan menolak dengan pesan jelas.
func (c Config) Configured() bool {
	return c.LiveKitAPIKey != "" && c.LiveKitAPISecret != ""
}

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func getdur(key string, fallback int) time.Duration {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return time.Duration(n)
		}
	}
	return time.Duration(fallback)
}

// loadDotEnv membaca file .env (folder kerja, lalu di samping executable) tanpa
// dependency eksternal. Environment variable asli selalu menang.
func loadDotEnv() {
	candidates := []string{".env"}
	if exe, err := os.Executable(); err == nil {
		candidates = append(candidates, filepath.Join(filepath.Dir(exe), ".env"))
	}
	for _, p := range candidates {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(b), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			k, v, ok := strings.Cut(line, "=")
			k = strings.TrimSpace(k)
			if !ok || k == "" {
				continue
			}
			v = strings.TrimSpace(v)
			if len(v) >= 2 && (v[0] == '"' && v[len(v)-1] == '"' || v[0] == '\'' && v[len(v)-1] == '\'') {
				v = v[1 : len(v)-1]
			}
			if _, exists := os.LookupEnv(k); !exists {
				_ = os.Setenv(k, v)
			}
		}
		return // file pertama yang ketemu yang dipakai
	}
}
