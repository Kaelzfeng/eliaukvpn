// Package webviewhost hosts the Eliauk GUI inside a Microsoft Edge WebView2
// (Chromium) control and bridges the embedded HTML frontend to the Go core.
// It replaces the old pure-Win32 window; the agent/config/mc layers are untouched.
package webviewhost

import "encoding/json"

// StatusInfo is the one-line status shown in the header.
type StatusInfo struct {
	Text string `json:"text"`
	Good bool   `json:"good"`
	Dot  string `json:"dot"` // ok | warn | busy
}

// RoomMember mirrors protocol.RoomMember for the UI.
type RoomMember struct {
	Username  string `json:"username"`
	VirtualIP string `json:"virtualIP"`
	Host      bool   `json:"host"`
}

// RoomInfo is the room section state.
type RoomInfo struct {
	In      bool         `json:"in"`
	Code    string       `json:"code"`
	Members []RoomMember `json:"members"`
}

// GameInfo is the game section state.
type GameInfo struct {
	Java     string `json:"java"`
	Jar      string `json:"jar"`
	Running  bool   `json:"running"`
	State    string `json:"state"`
	Addr     string `json:"addr"` // joinable address ip:25565, "" if none
	Launcher string `json:"launcher"`
	GameDir  string `json:"gameDir"`
}

// Settings is the settings section state.
type Settings struct {
	Name      string `json:"name"`
	Server    string `json:"server"`
	UpdateURL string `json:"updateUrl"`
}

// UpdateInfo is the About-panel self-update state.
type UpdateInfo struct {
	Status  string `json:"status"`            // "" | checking | error:... | current | ready
	Ready   bool   `json:"ready"`             // new version downloaded, pending install
	Version string `json:"version"`           // version that is ready to install
	Notes   string `json:"notes"`             // release notes from the manifest
}

// State is the full renderable state pushed to the frontend.
type State struct {
	Status     StatusInfo `json:"status"`
	Fullscreen bool       `json:"fullscreen"`
	Version    string     `json:"version"`
	Settings   Settings   `json:"settings"`
	Room       RoomInfo   `json:"room"`
	Game       GameInfo   `json:"game"`
	Update     UpdateInfo `json:"update"`
}

// MarshalState encodes state to JSON.
func MarshalState(s State) (string, error) {
	b, err := json.Marshal(s)
	if err != nil {
		return "", err
	}
	return string(b), nil
}
