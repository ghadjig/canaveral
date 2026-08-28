package cli

import "syscall"

// syscallExec replaces the current process image, handing the terminal to the
// target program so TUIs get direct control of stdin/stdout.
func syscallExec(bin string, argv, env []string) error {
	return syscall.Exec(bin, argv, env)
}
