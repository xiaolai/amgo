// Package model defines the domain types shared by the store, adapters and
// application services. It has no I/O dependencies.
package model

import "time"

const (
	AdapterClaude   = "claude"
	AdapterCodex    = "codex"
	AdapterOpenCode = "opencode"
	AdapterAgy      = "agy"
	AdapterQwen     = "qwen"
)

const (
	MaturityDocumented     = "documented"
	MaturityFixtureTested  = "fixture-tested"
	MaturityNativeVerified = "native-verified"
)

// Capabilities is the per-adapter, per-capability maturity record.
type Capabilities struct {
	Adapter         string   `json:"adapter"`
	Maturity        string   `json:"maturity"`
	MinKnownVersion string   `json:"min_known_version,omitempty"`
	TestedVersions  []string `json:"tested_versions,omitempty"`
	LiveMessaging   bool     `json:"live_messaging"`
	NativeReceipts  bool     `json:"native_receipts"`
	Jobs            bool     `json:"jobs"`
	Resume          bool     `json:"resume"`
	Constraints     []string `json:"constraints,omitempty"`
}

// Target is the immutable native identity snapshot used for a send.
type Target struct {
	Adapter         string   `json:"adapter"`
	HostID          string   `json:"host_id"`
	ConfigHome      string   `json:"config_home"`
	NativeSessionID string   `json:"native_session_id"`
	Endpoint        string   `json:"endpoint,omitempty"`
	SocketPath      string   `json:"socket_path,omitempty"`
	PID             int      `json:"pid,omitempty"`
	ProcStart       string   `json:"proc_start,omitempty"`
	PIDDomain       string   `json:"pid_domain,omitempty"`
	Cwd             string   `json:"cwd,omitempty"`
	CheckoutID      string   `json:"checkout_id,omitempty"`
	RepoID          string   `json:"repo_id,omitempty"`
	Version         string   `json:"version,omitempty"`
	PeerProtocol    int      `json:"peer_protocol,omitempty"`
	PeerFeatures    []string `json:"peer_features,omitempty"`
	Status          string   `json:"status,omitempty"`
	Kind            string   `json:"kind,omitempty"`
}

// Candidate is a native session that has not necessarily been registered.
type Candidate struct {
	Target
	Name          string `json:"name"`
	EvidenceLevel string `json:"evidence_level"`
	Live          bool   `json:"live"`
	Reason        string `json:"reason,omitempty"`
}

// Session is an explicit amgo registration.
type Session struct {
	RecordVersion         int          `json:"record_version"`
	ID                    string       `json:"id"`
	Alias                 string       `json:"alias"`
	Target                Target       `json:"target"`
	Capabilities          Capabilities `json:"capabilities"`
	RegisteredAt          time.Time    `json:"registered_at"`
	UnregisteredAt        *time.Time   `json:"unregistered_at,omitempty"`
	AcceptUnknownDelivery bool         `json:"accept_unknown_delivery,omitempty"`
}

// MessageState is the amgo-observed state, never a provider task outcome.
type MessageState string

const (
	MessageDispatching        MessageState = "dispatching"
	MessageSubmitted          MessageState = "submitted"
	MessageFailedBeforeSubmit MessageState = "failed_before_submit"
	MessageUnknown            MessageState = "unknown"
	MessageAcknowledged       MessageState = "acknowledged"
	MessageAbandoned          MessageState = "abandoned"
	MessageExpired            MessageState = "expired"
)

// Native dispositions are provider evidence, separate from message state.
const (
	DispositionUnknown      = "unknown"
	DispositionQueued       = "queued"
	DispositionStarted      = "started"
	DispositionHeld         = "held"
	DispositionRefused      = "refused"
	DispositionDenied       = "denied"
	DispositionExpired      = "expired"
	DispositionDropped      = "dropped"
	DispositionMisaddressed = "misaddressed"
	DispositionLocal        = "local"
)

type Message struct {
	RecordVersion      int          `json:"record_version"`
	ID                 string       `json:"id"`
	ConversationID     string       `json:"conversation_id"`
	SenderSessionID    string       `json:"sender_session_id,omitempty"`
	SenderLabel        string       `json:"sender_label"`
	RecipientSessionID string       `json:"recipient_session_id"`
	ReplyTo            string       `json:"reply_to,omitempty"`
	RetryOf            string       `json:"retry_of,omitempty"`
	RetriedBy          []string     `json:"retried_by,omitempty"`
	Body               string       `json:"body"`
	BodySHA256         string       `json:"body_sha256"`
	BodyBytes          int          `json:"body_bytes"`
	State              MessageState `json:"state"`
	NativeDisposition  string       `json:"native_disposition"`
	ExpectReply        bool         `json:"expect_reply"`
	FramingVersion     int          `json:"framing_version"`
	CreatedAt          time.Time    `json:"created_at"`
	ExpiresAt          time.Time    `json:"expires_at"`
}

type Conversation struct {
	RecordVersion int        `json:"record_version"`
	ID            string     `json:"id"`
	Name          string     `json:"name,omitempty"`
	Participants  []string   `json:"participants"`
	MaxMessages   int        `json:"max_messages"`
	SpentCount    int        `json:"spent_count"`
	RefundedCount int        `json:"refunded_count"`
	ExpiresAt     time.Time  `json:"expires_at"`
	ClosedAt      *time.Time `json:"closed_at,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
}

func (c Conversation) Open(now time.Time) bool {
	return c.ClosedAt == nil && now.Before(c.ExpiresAt)
}

func (c Conversation) Remaining() int {
	return c.MaxMessages - (c.SpentCount - c.RefundedCount)
}

type AttemptState string

const (
	AttemptDispatching AttemptState = "dispatching"
	AttemptDispatched  AttemptState = "dispatched"
	AttemptFinalized   AttemptState = "finalized"
)

type Attempt struct {
	RecordVersion        int          `json:"record_version"`
	ID                   string       `json:"id"`
	MessageID            string       `json:"message_id"`
	ReservationID        string       `json:"reservation_id"`
	Target               Target       `json:"target"`
	State                AttemptState `json:"state"`
	StartedAt            time.Time    `json:"started_at"`
	DispatchDeadline     time.Time    `json:"dispatch_deadline"`
	DispatchedAt         *time.Time   `json:"dispatched_at,omitempty"`
	FinalizationDeadline *time.Time   `json:"finalization_deadline,omitempty"`
	OwnerPID             int          `json:"owner_pid"`
	OwnerProcStart       string       `json:"owner_proc_start"`
	Outcome              string       `json:"outcome,omitempty"`
	NativeSubmissionID   string       `json:"native_submission_id,omitempty"`
	NativeDisposition    string       `json:"native_disposition,omitempty"`
	ErrorCode            string       `json:"error_code,omitempty"`
	ErrorDetail          string       `json:"error_detail,omitempty"`
	FinalizedAt          *time.Time   `json:"finalized_at,omitempty"`
	Inferred             bool         `json:"inferred,omitempty"`
}

type ReservationState string

const (
	ReservationReserved ReservationState = "reserved"
	ReservationRefunded ReservationState = "refunded"
	ReservationConsumed ReservationState = "consumed"
)

type Reservation struct {
	RecordVersion    int              `json:"record_version"`
	ID               string           `json:"id"`
	MessageID        string           `json:"message_id"`
	ConversationID   string           `json:"conversation_id"`
	State            ReservationState `json:"state"`
	CreatedAt        time.Time        `json:"created_at"`
	DispatchDeadline time.Time        `json:"dispatch_deadline"`
}

// IdemRecord is the compact idempotency membership record. It never stores a
// body.
type IdemRecord struct {
	RecordVersion  int       `json:"record_version"`
	Fingerprint    string    `json:"fingerprint"`
	MessageID      string    `json:"message_id"`
	ConversationID string    `json:"conversation_id"`
	FirstSeen      time.Time `json:"first_seen"`
	Outcome        string    `json:"outcome"`
	MessagePruned  bool      `json:"message_pruned"`
	// Replay evidence that remains checkable when the target can no longer be
	// resolved. Fingerprint covers these too, but only when resolution works.
	BodySHA256       string `json:"body_sha256,omitempty"`
	ReplyTo          string `json:"reply_to,omitempty"`
	ExpectReply      bool   `json:"expect_reply,omitempty"`
	RecipientAlias   string `json:"recipient_alias,omitempty"`
	ConversationName string `json:"conversation_name,omitempty"`
}

type Event struct {
	Seq       uint64         `json:"seq"`
	MessageID string         `json:"message_id,omitempty"`
	AttemptID string         `json:"attempt_id,omitempty"`
	Kind      string         `json:"kind"`
	At        time.Time      `json:"at"`
	Payload   map[string]any `json:"payload,omitempty"`
}

// AckProvenance distinguishes an application acknowledgement by the bound
// recipient from an operator override.
type AckProvenance string

const (
	ProvenanceRecipient AckProvenance = "recipient"
	ProvenanceOperator  AckProvenance = "operator"
)

type Receipt struct {
	RecordVersion int           `json:"record_version"`
	ID            string        `json:"id"`
	MessageID     string        `json:"message_id"`
	Kind          string        `json:"kind"` // ack | reply | native_status
	Provenance    AckProvenance `json:"provenance,omitempty"`
	Actor         string        `json:"actor,omitempty"`
	ReplyMessage  string        `json:"reply_message,omitempty"`
	CreatedAt     time.Time     `json:"created_at"`
}
