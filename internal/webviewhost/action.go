package webviewhost

import "encoding/json"

// Action is one user action decoded from the frontend. The Type selects the
// handler; the other fields carry the per-type payload.
type Action struct {
	Type     string `json:"type"`
	Name     string `json:"name,omitempty"`
	Server   string `json:"server,omitempty"`
	Code     string `json:"code,omitempty"`
	Text     string `json:"text,omitempty"`
	Java      string `json:"java,omitempty"`
	Jar       string `json:"jar,omitempty"`
	Launcher  string `json:"launcher,omitempty"`
	GameDir   string `json:"gameDir,omitempty"`
	UpdateURL string `json:"updateUrl,omitempty"`
}

// Action type constants — the frontend's vocabulary.
const (
	ActReady      = "ready"
	ActSave       = "save"
	ActQuit       = "quit"
	ActRoomCreate = "roomCreate"
	ActRoomJoin   = "roomJoin"
	ActRoomLeave  = "roomLeave"
	ActGameDetect = "gameDetect"
	ActSrvStart   = "srvStart"
	ActSrvStop    = "srvStop"
	ActCopy       = "copy"
	ActMCAdd       = "mcAdd"
	ActLaunch      = "launch"
	ActFullscreen  = "fullscreen"
	ActCheckUpdate = "checkUpdate"
	ActApplyUpdate = "applyUpdate"
)

// ParseAction decodes a frontend action from its JSON form.
func ParseAction(s string) (Action, error) {
	var a Action
	err := json.Unmarshal([]byte(s), &a)
	return a, err
}
