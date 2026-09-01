package webviewhost

import "encoding/json"

// Action is one user action decoded from the frontend. The Type selects the
// handler; the other fields carry the per-type payload.
type Action struct {
	Type   string `json:"type"`
	Name   string `json:"name,omitempty"`
	Server string `json:"server,omitempty"`
	User   string `json:"user,omitempty"`
	Pass   string `json:"pass,omitempty"`
	Input  string `json:"input,omitempty"`
	Key    string `json:"key,omitempty"`
	Code   string `json:"code,omitempty"`
	Text   string `json:"text,omitempty"`
	Java   string `json:"java,omitempty"`
	Jar    string `json:"jar,omitempty"`
}

// Action type constants — the frontend's vocabulary.
const (
	ActReady        = "ready"
	ActSave         = "save"
	ActQuit         = "quit"
	ActLogin        = "login"
	ActRegister     = "register"
	ActLogout       = "logout"
	ActAddFriend    = "addFriend"
	ActDeleteFriend = "deleteFriend"
	ActConnect      = "connect"
	ActRoomCreate   = "roomCreate"
	ActRoomJoin     = "roomJoin"
	ActRoomLeave    = "roomLeave"
	ActGameDetect   = "gameDetect"
	ActSrvStart     = "srvStart"
	ActSrvStop      = "srvStop"
	ActCopy         = "copy"
	ActMCAdd        = "mcAdd"
	ActLaunch       = "launch"
)

// ParseAction decodes a frontend action from its JSON form.
func ParseAction(s string) (Action, error) {
	var a Action
	err := json.Unmarshal([]byte(s), &a)
	return a, err
}
