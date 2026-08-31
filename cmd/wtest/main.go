// Command wtest is a minimal wintun adapter-creation probe: it creates an
// adapter with the canonical "Wintun" tunnel type, holds it open, and reports
// its LUID so the operator can check whether it appears as a real interface.
package main

import (
	"fmt"
	"time"

	"golang.zx2c4.com/wintun"
)

func main() {
	a, err := wintun.CreateAdapter("EliaukTest", "Wintun", nil)
	if err != nil {
		fmt.Println("create:", err)
		return
	}
	fmt.Printf("adapter created, LUID=%d\n", a.LUID())
	sess, err := a.StartSession(0x400000)
	if err != nil {
		fmt.Println("session:", err)
		return
	}
	fmt.Println("session started")
	fmt.Println("holding 30s — inspect with Get-NetAdapter now")
	time.Sleep(30 * time.Second)
	sess.End()
	a.Close()
	fmt.Println("closed")
}
