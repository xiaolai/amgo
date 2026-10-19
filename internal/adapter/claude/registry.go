package claude

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"runtime"
	"strings"

	"amgo/internal/platform"
)

const (
	maxKeyBytes    = 4096
	maxRecordBytes = 64 << 10
)

type SessionRecord struct {
	PID                 int      `json:"pid"`
	SessionID           string   `json:"sessionId"`
	ProcStart           string   `json:"procStart"`
	ProcStartFt         string   `json:"procStartFt"`
	Version             string   `json:"version"`
	PeerProtocol        int      `json:"peerProtocol"`
	PeerFeatures        []string `json:"peerFeatures"`
	Kind                string   `json:"kind"`
	Entrypoint          string   `json:"entrypoint"`
	PIDDomain           string   `json:"pidDomain"`
	MessagingSocketPath string   `json:"messagingSocketPath"`
	Name                string   `json:"name"`
	Status              string   `json:"status"`
	Spare               bool     `json:"spare"`
	ParkedJobID         string   `json:"parkedJobId"`
}

type KeyFile struct {
	PeerToken   string `json:"peerToken"`
	ProcStart   string `json:"procStart"`
	ProcStartFt string `json:"procStartFt"`
	PIDDomain   string `json:"pidDomain"`
}

func sessionsDir(home string) string { return filepath.Join(home, "sessions") }

// KeyPath derives the published peer key path from the record's pid and exact
// socket string. It is kept separate from sessionsDir so the hash rule is
// testable.
func KeyPath(home string, pid int, socketPath string) string {
	h := sha256.Sum256([]byte(socketPath))
	return filepath.Join(sessionsDir(home), fmt.Sprintf("%d.%s.key", pid, hex.EncodeToString(h[:])))
}

func ReadKey(path string) (KeyFile, error) {
	var k KeyFile
	raw, err := platform.ReadRegularFile(path, maxKeyBytes, platform.UID())
	if err != nil {
		return k, err
	}
	if err := json.Unmarshal(raw, &k); err != nil {
		return k, fmt.Errorf("malformed key file: %w", err)
	}
	if len(k.PeerToken) != 32 {
		return k, fmt.Errorf("peer token length %d, want 32", len(k.PeerToken))
	}
	for _, c := range k.PeerToken {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return k, fmt.Errorf("peer token is not lowercase hex")
		}
	}
	return k, nil
}

func readRecord(path string) (SessionRecord, error) {
	var r SessionRecord
	raw, err := platform.ReadBoundedFile(path, maxRecordBytes)
	if err != nil {
		return r, err
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return r, err
	}
	return r, nil
}

func localDomain() string { return runtime.GOOS }

func normalizeProcStart(s string) string { return strings.Join(strings.Fields(s), " ") }
