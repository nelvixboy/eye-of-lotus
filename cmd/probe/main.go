// Command probe exercises the D-Bus client and the systemd wrapper the way
// the panel uses them, and prints what each step returned.
package main

import (
	"fmt"

	"github.com/nelvixboy/eye-of-lotus/internal/systemd"
)

func main() {
	c := systemd.New("")
	fmt.Println("== exactly the panel's path ==")
	fmt.Printf("Connect:  %v\n", c.Connect())
	fmt.Printf("Version:  %q  (denied by policy on many distributions)\n", c.Version())

	units, err := c.ListUnits()
	fmt.Printf("ListUnits: err=%v units=%d\n", err, len(units))
	if err == nil {
		for i, u := range units {
			if i >= 5 {
				break
			}
			fmt.Printf("   %s %s/%s\n", u.Name, u.ActiveState, u.SubState)
		}
		d, derr := c.Detail(units[0].Path)
		fmt.Printf("Detail:    err=%v enabled=%q restarts=%d\n", derr, d.UnitFileState, d.NRestarts)
	}
}
