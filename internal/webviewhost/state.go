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

// FriendCard is one friend row.
type FriendCard struct {
	Key    string `json:"key"`   // stable key: KeyFP (account) or Code (legacy)
	Name   string `json:"name"`  // display name
	State  string `json:"state"` // connected | connecting | online | offline
	Online bool   `json:"online"`
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
	Java    string `json:"java"`
	Jar     string `json:"jar"`
	Running bool   `json:"running"`
	State   string `json:"state"`
	Addr    string `json:"addr"` // joinable address ip:25565, "" if none
}

// Settings is the settings section state.
type Settings struct {
	Name   string `json:"name"`
	Server string `json:"server"`
}

// State is the full renderable state pushed to the frontend.
type State struct {
	Status   StatusInfo   `json:"status"`
	Code     string       `json:"code"`
	Account  string       `json:"account"`
	LoggedIn bool         `json:"loggedIn"`
	AddHint  string       `json:"addHint"`
	Settings Settings     `json:"settings"`
	Friends  []FriendCard `json:"friends"`
	Room     RoomInfo     `json:"room"`
	Game     GameInfo     `json:"game"`
}

// MarshalState encodes state to JSON.
func MarshalState(s State) (string, error) {
	b, err := json.Marshal(s)
	if err != nil {
		return "", err
	}
	return string(b), nil
}
