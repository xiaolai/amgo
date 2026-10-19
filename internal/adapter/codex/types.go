package codex

import "encoding/json"

type initializeParams struct {
	ClientInfo   clientInfo   `json:"clientInfo"`
	Capabilities capabilities `json:"capabilities"`
}

type clientInfo struct {
	Name    string `json:"name"`
	Title   string `json:"title,omitempty"`
	Version string `json:"version"`
}

type capabilities struct {
	ExperimentalAPI bool `json:"experimentalApi"`
}

type initializeResponse struct {
	UserAgent      string `json:"userAgent"`
	CodexHome      string `json:"codexHome"`
	PlatformFamily string `json:"platformFamily"`
	PlatformOS     string `json:"platformOS"`
}

type threadListParams struct {
	Limit          int    `json:"limit,omitempty"`
	Cursor         string `json:"cursor,omitempty"`
	UseStateDBOnly bool   `json:"useStateDbOnly,omitempty"`
	Archived       *bool  `json:"archived,omitempty"`
}

type threadStatus struct {
	Type        string   `json:"type"`
	ActiveFlags []string `json:"activeFlags,omitempty"`
}

type thread struct {
	ID                   string          `json:"id"`
	SessionID            string          `json:"sessionId"`
	Ephemeral            bool            `json:"ephemeral"`
	Status               threadStatus    `json:"status"`
	CanAcceptDirectInput *bool           `json:"canAcceptDirectInput"`
	Cwd                  string          `json:"cwd"`
	Path                 string          `json:"path"`
	CLIVersion           string          `json:"cliVersion"`
	Originator           string          `json:"originator"`
	Source               json.RawMessage `json:"source"`
	ModelProvider        string          `json:"modelProvider"`
	Model                string          `json:"model"`
	CreatedAt            int64           `json:"createdAt"`
	UpdatedAt            int64           `json:"updatedAt"`
	Preview              string          `json:"preview"`
}

type threadListResponse struct {
	Data       []thread `json:"data"`
	NextCursor string   `json:"nextCursor"`
}

type loadedListParams struct {
	Cursor string `json:"cursor,omitempty"`
	Limit  int    `json:"limit,omitempty"`
}

type loadedListResponse struct {
	Data       []string `json:"data"`
	NextCursor string   `json:"nextCursor"`
}

type threadReadParams struct {
	ThreadID string `json:"threadId"`
}

type threadReadResponse struct {
	Thread thread `json:"thread"`
}

type userInput struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type queueAddParams struct {
	ThreadID            string      `json:"threadId"`
	ClientUserMessageID string      `json:"clientUserMessageId"`
	Input               []userInput `json:"input"`
}

type queuedSubmission struct {
	ID                  string      `json:"id"`
	ClientUserMessageID string      `json:"clientUserMessageId"`
	Input               []userInput `json:"input"`
}

type queueAddResponse struct {
	QueuedSubmission queuedSubmission `json:"queuedSubmission"`
}

type rpcNotification struct {
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

type itemStartedParams struct {
	ThreadID string `json:"threadId"`
	TurnID   string `json:"turnId"`
	Item     struct {
		Type     string      `json:"type"`
		ID       string      `json:"id"`
		ClientID string      `json:"clientId"`
		Content  []userInput `json:"content"`
	} `json:"item"`
}

type statusChangedParams struct {
	ThreadID string       `json:"threadId"`
	Status   threadStatus `json:"status"`
}
