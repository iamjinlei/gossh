package ssh

import (
	"bufio"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"time"

	gossh "golang.org/x/crypto/ssh"
)

type Session struct {
	c      *gossh.Client
	s      *gossh.Session
	stdin  io.WriteCloser
	stdout io.Reader
	stderr io.Reader
}

func NewSession(hostport, user, pwd string) (*Session, error) {
	var am gossh.AuthMethod
	if len(pwd) > 0 {
		am = gossh.Password(pwd)
	} else {
		pk, err := ioutil.ReadFile(filepath.Join(os.Getenv("HOME"), "/.ssh/id_rsa"))
		if err != nil {
			return nil, err
		}
		signer, err := gossh.ParsePrivateKey(pk)
		if err != nil {
			return nil, err
		}
		am = gossh.PublicKeys(signer)
	}

	cfg := &gossh.ClientConfig{
		User:            user,
		Auth:            []gossh.AuthMethod{am},
		HostKeyCallback: gossh.InsecureIgnoreHostKey(),
		BannerCallback:  func(message string) error { return nil }, // ignore banner
		Timeout:         30 * time.Second,
	}

	c, err := gossh.Dial("tcp", hostport, cfg)
	if err != nil {
		return nil, err
	}

	s, err := c.NewSession()
	if err != nil {
		c.Close()
		return nil, err
	}

	stdout, err := s.StdoutPipe()
	if err != nil {
		c.Close()
		return nil, err
	}

	stderr, err := s.StderrPipe()
	if err != nil {
		c.Close()
		return nil, err
	}

	stdin, err := s.StdinPipe()
	if err != nil {
		c.Close()
		return nil, err
	}

	if err := s.Start("/bin/bash"); err != nil {
		c.Close()
		return nil, err
	}

	return &Session{
		c:      c,
		s:      s,
		stdin:  stdin,
		stdout: stdout,
		stderr: stderr,
	}, nil
}

func (s *Session) Run(cmd string) (chan []byte, chan []byte, error) {
	endMark := []byte(fmt.Sprintf("$$__%v__$$", time.Now().UnixNano()))

	recv := func(r io.Reader, out chan []byte) {
		br := bufio.NewReaderSize(r, 1024)
		var bytes []byte

		defer close(out)

		for {
			data, isPrefix, err := br.ReadLine()

			if err != nil && err != io.EOF {
				if len(bytes) > 0 {
					out <- bytes
				}

				out <- []byte(fmt.Sprintf("error reading pipe: %v", err))
				return
			}

			bytes = append(bytes, data...)
			if isPrefix {
				continue
			}

			if len(bytes) == len(endMark) && string(bytes) == string(endMark) {
				return
			}
			out <- bytes
			bytes = nil
		}
	}

	if _, err := s.stdin.Write([]byte(cmd + "\n")); err != nil {
		return nil, nil, err
	}
	// TODO: if the following writes fail, how could we clear the data from stdout/stderr
	if _, err := s.stdin.Write([]byte("echo '" + string(endMark) + "'\n")); err != nil {
		return nil, nil, err
	}
	if _, err := s.stdin.Write([]byte("echo '" + string(endMark) + "' >&2\n")); err != nil {
		return nil, nil, err
	}

	outCh := make(chan []byte, 16)
	errCh := make(chan []byte, 16)
	go recv(s.stdout, outCh)
	go recv(s.stderr, errCh)

	return outCh, errCh, nil
}

func (s *Session) Close() error {
	return s.c.Close()
}
