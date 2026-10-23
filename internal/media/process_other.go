//go:build !unix

package media

import (
	"os/exec"
	"time"
)

func configureCancellation(cmd *exec.Cmd) { cmd.WaitDelay = time.Second }
