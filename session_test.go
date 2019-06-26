package ssh

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestSession(t *testing.T) {
	s, err := NewSession("127.0.0.1:22", "lei", "", time.Minute)
	assert.NoError(t, err)

	defer s.Close()

	exec := func(cmd string) {
		outCh, errCh, err := s.Run(cmd)
		assert.NoError(t, err)
		Log(outCh, errCh)
	}

	exec("echo 'Hello world!'")
	exec("whoami")
	exec("pwd")
	exec("which sh")
	exec("awk -F= '/^NAME/{print $2}' /etc/os-release")
	exec("find ~/ | head -n 20")

	assert.NoError(t, s.CopyTo("/Users/lei/go/src/github.com/iamjinlei/ssh/session.go", "scp_test"))
}
