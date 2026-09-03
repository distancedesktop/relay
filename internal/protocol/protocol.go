// Package protocol holds the newline-delimited JSON control messages that the
// Distance agent speaks over its single client-initiated bidi stream, plus the
// synthetic H.264 Annex B frame generator the fake agent uses for video.
//
// Ground truth (SPIKE.md, verified against the live agent, not assumed):
//   - control is one client-initiated bidi stream carrying NDJSON
//   - the agent pushes `fingerprint-refresh` then an unsolicited `displays`
//     immediately after the control stream opens, before any request
//   - implemented verbs: list-displays, start, stop
//   - anything else -> {"type":"error","message":"unknown type: <t>"}
//     (there is no `input` and no `ping` handler)
package protocol

import "encoding/json"

// Message types the agent sends or accepts. Only these exist; see SPIKE.md.
const (
	// Client -> agent.
	TypeListDisplays = "list-displays"
	TypeStart        = "start"
	TypeStop         = "stop"

	// Agent -> client.
	TypeFingerprintRefresh = "fingerprint-refresh"
	TypeDisplays           = "displays"
	TypeError              = "error"
	TypeStarted            = "started"
	TypeStopped            = "stopped"
)

// Envelope is the common shape of every control frame: a discriminating "type".
// Decoding into Envelope and keeping the raw bytes around lets a reader dispatch
// without normalising traffic it does not understand.
type Envelope struct {
	Type string `json:"type"`
}

// Display mirrors the shape the agent reports for a capturable display.
type Display struct {
	ID      string  `json:"id"`
	Name    string  `json:"name"`
	Width   int     `json:"width"`
	Height  int     `json:"height"`
	Scale   float64 `json:"scale,omitempty"`
	Primary bool    `json:"primary,omitempty"`
}

// DisplaysMsg is both the unsolicited push after connect and the list-displays reply.
type DisplaysMsg struct {
	Type     string    `json:"type"`
	Displays []Display `json:"displays"`
}

// FingerprintRefreshMsg is pushed first, before displays. The real agent uses it to
// hand the viewer the certificate hash to pin on the next reconnect — precisely the
// thing a terminating relay is meant to make unnecessary.
type FingerprintRefreshMsg struct {
	Type        string `json:"type"`
	Fingerprint string `json:"fingerprint"`
	NotAfter    string `json:"notAfter,omitempty"`
}

// StartMsg asks the agent to begin capturing a display.
type StartMsg struct {
	Type      string `json:"type"`
	DisplayID string `json:"displayId,omitempty"`
	Width     int    `json:"width,omitempty"`
	Height    int    `json:"height,omitempty"`
	FPS       int    `json:"fps,omitempty"`
	Bitrate   int    `json:"bitrate,omitempty"`
}

// StopMsg asks the agent to stop capturing.
type StopMsg struct {
	Type string `json:"type"`
}

// StartedMsg acknowledges a start, echoing the parameters actually in effect.
type StartedMsg struct {
	Type      string `json:"type"`
	DisplayID string `json:"displayId,omitempty"`
	Width     int    `json:"width,omitempty"`
	Height    int    `json:"height,omitempty"`
	FPS       int    `json:"fps,omitempty"`
	Bitrate   int    `json:"bitrate,omitempty"`
}

// StoppedMsg acknowledges a stop.
type StoppedMsg struct {
	Type string `json:"type"`
}

// ErrorMsg is the agent's only error shape.
type ErrorMsg struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

// UnknownType builds the exact error the agent returns for an unhandled verb.
// The wording is load-bearing: it is what the viewer matches on.
func UnknownType(t string) ErrorMsg {
	return ErrorMsg{Type: TypeError, Message: "unknown type: " + t}
}

// Encode marshals v and appends the newline delimiter.
func Encode(v any) ([]byte, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}
