package rewrite

import "os/exec"

// exec_LookPath and runCmd keep os/exec use in tests to one place.
func exec_LookPath(name string) (string, error) { return exec.LookPath(name) }

func runCmd(name string, args ...string) ([]byte, error) {
	return exec.Command(name, args...).CombinedOutput()
}
