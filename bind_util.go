package main

import (
	"fmt"
	"strconv"
)

func itoa(i int) string                          { return strconv.Itoa(i) }
func sscan1(s, f string, a *int) (int, error)    { return fmt.Sscanf(s, f, a) }
func sscan2(s, f string, a, b *int) (int, error) { return fmt.Sscanf(s, f, a, b) }
