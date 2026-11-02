package main

import "fmt"

func dispatch(cmd string, args []string) error {
	switch cmd {
	case "sim":
		return cmdSim(args)
	case "replay":
		return cmdReplay(args)
	case "cut":
		return cmdCut(args)
	case "bench":
		return cmdBench(args)
	}
	return fmt.Errorf("unknown command %q", cmd)
}
