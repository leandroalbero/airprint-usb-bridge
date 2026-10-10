//go:build !unix

package cups

import "os/exec"

func ownProcessGroup(*exec.Cmd) {}

func killGroup(*exec.Cmd) {}
