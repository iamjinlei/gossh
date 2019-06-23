package ssh

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestSession(t *testing.T) {
	s, err := NewSession("127.0.0.1:22", "lei", "")
	assert.NoError(t, err)

	exec := func(cmd string) {
		outCh, errCh, err := s.Run(cmd)
		assert.NoError(t, err)
		for line := range outCh {
			t.Log("INFO:  " + string(line))
		}
		for line := range errCh {
			t.Log("ERROR: " + string(line))
		}
	}

	exec("echo 'Hello world!'")
	exec("whoami")
	exec("pwd")
	exec("which sh")
	exec("awk -F= '/^NAME/{print $2}' /etc/os-release")
	exec("find ~/ | head -n 20")
}
