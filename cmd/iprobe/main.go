// Command iprobe creates the wintun adapter with vnic.Open (which assigns
// 10.99.0.2/24 via netsh) and holds it open so the operator can inspect the
// real interface state — name, ifIndex, IP address, route — from PowerShell.
package main

import (
	"fmt"
	"time"

	"eliaukvpn/internal/vnic"
)

func main() {
	ad, err := vnic.Open("Eliauk-admcast", "10.99.0.2", "255.255.255.0")
	if err != nil {
		fmt.Println("adapter:", err)
		return
	}
	defer ad.Close()
	fmt.Println("adapter open — holding 60s, inspect with Get-NetAdapter / Get-NetIPAddress now")
	time.Sleep(60 * time.Second)
	fmt.Println("done")
}
