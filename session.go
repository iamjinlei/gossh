// Acknowledgement: the scp implementation is heavily influenced by https://github.com/deoxxa/scp

package gossh

import (
	"bufio"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	ssh "golang.org/x/crypto/ssh"
)

type sessionHandle struct {
	s      *ssh.Session
	stdin  io.WriteCloser
	stdout io.Reader
	stderr io.Reader
}

func newSessionHandle(c *ssh.Client, cmd string) (*sessionHandle, error) {
	s, err := c.NewSession()
	if err != nil {
		return nil, err
	}

	stdout, err := s.StdoutPipe()
	if err != nil {
		s.Close()
		return nil, err
	}

	stderr, err := s.StderrPipe()
	if err != nil {
		s.Close()
		return nil, err
	}

	stdin, err := s.StdinPipe()
	if err != nil {
		s.Close()
		return nil, err
	}

	h := &sessionHandle{
		s:      s,
		stdout: stdout,
		stderr: stderr,
		stdin:  stdin,
	}

	if err := s.Start(cmd); err != nil {
		h.close()
		return nil, err
	}

	return h, nil
}

func (h *sessionHandle) close() {
	h.stdin.Close()
	h.s.Close()
}

type Session struct {
	c *ssh.Client
}

func NewSession(hostport, user, pwd string, to time.Duration) (*Session, error) {
	var am ssh.AuthMethod
	if len(pwd) > 0 {
		am = ssh.Password(pwd)
	} else {
		pk, err := ioutil.ReadFile(filepath.Join(os.Getenv("HOME"), "/.ssh/id_rsa"))
		if err != nil {
			return nil, err
		}
		signer, err := ssh.ParsePrivateKey(pk)
		if err != nil {
			return nil, err
		}
		am = ssh.PublicKeys(signer)
	}

	deadline := time.Now().Add(to)
	ticker := time.NewTicker(time.Second)
	expire := time.NewTimer(to)
	var c *ssh.Client
	var err error
	for c == nil {
		select {
		case <-expire.C:
			return nil, err
		case <-ticker.C:
			cfg := &ssh.ClientConfig{
				User:            user,
				Auth:            []ssh.AuthMethod{am},
				HostKeyCallback: ssh.InsecureIgnoreHostKey(),
				BannerCallback:  func(message string) error { return nil }, // ignore banner
				Timeout:         deadline.Sub(time.Now()),
			}

			if c, err = ssh.Dial("tcp", hostport, cfg); err != nil {
				c = nil
			}
		}
	}

	return &Session{
		c: c,
	}, nil
}

type Cmd struct {
	h     *sessionHandle
	outCh chan []byte
	errCh chan []byte
}

func (c *Cmd) TailLog() {
	var wg sync.WaitGroup
	if c.outCh != nil {
		wg.Add(1)
		go func(g *sync.WaitGroup) {
			for line := range c.outCh {
				fmt.Printf("%v\n", string(line))
			}
			g.Done()
		}(&wg)
	}
	if c.errCh != nil {
		wg.Add(1)
		go func(g *sync.WaitGroup) {
			for line := range c.errCh {
				fmt.Printf("%v\n", string(line))
			}
			g.Done()
		}(&wg)
	}
	wg.Wait()
}

func (c *Cmd) Close() {
	c.h.close()
}

func (s *Session) Run(cmd string) (*Cmd, error) {
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
			} else if err == io.EOF {
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

	h, err := newSessionHandle(s.c, "/bin/bash")
	if err != nil {
		return nil, err
	}

	if _, err := h.stdin.Write([]byte(cmd + "\n")); err != nil {
		return nil, err
	}
	// TODO: if the following writes fail, how could we clear the data from stdout/stderr
	if _, err := h.stdin.Write([]byte("echo '" + string(endMark) + "'\n")); err != nil {
		return nil, err
	}
	if _, err := h.stdin.Write([]byte("echo '" + string(endMark) + "' >&2\n")); err != nil {
		return nil, err
	}

	outCh := make(chan []byte, 16)
	errCh := make(chan []byte, 16)
	go recv(h.stdout, outCh)
	go recv(h.stderr, errCh)

	return &Cmd{
		h:     h,
		outCh: outCh,
		errCh: errCh,
	}, nil
}

func (s *Session) CopyTo(src string, target string) error {
	target = strings.TrimSpace(target)
	targetBase := "/"
	if !path.IsAbs(target) {
		targetBase = "."
	}
	h, err := newSessionHandle(s.c, "scp -tr "+targetBase)
	if err != nil {
		return err
	}

	rw := bufio.NewReadWriter(bufio.NewReader(h.stdout), bufio.NewWriter(h.stdin))

	// build remote dirs if needed
	pathParts := strings.Split(target, "/")
	cnt := 0
	for _, part := range pathParts {
		if strings.TrimSpace(part) == "" {
			continue
		}
		cnt++
		if err := sendScpCmd(rw, fmt.Sprintf("D0755 0 "+part)); err != nil {
			return err
		}
	}
	for i := 0; i < cnt; i++ {
		if err := sendScpCmd(rw, "E"); err != nil {
			return err
		}
	}
	h.close()

	return copyPathTo(s.c, src, target)
}

func sendScpCmd(rw *bufio.ReadWriter, cmd string) error {
	if _, err := rw.WriteString(cmd + "\n"); err != nil {
		return err
	}
	if err := rw.Flush(); err != nil {
		return err
	}
	return handleScpResp(rw)
}

func handleScpResp(rw *bufio.ReadWriter) error {
	if b, err := rw.ReadByte(); err != nil {
		return err
	} else if b == 1 || b == 2 {
		msg, err := rw.ReadString('\n')
		if err != nil {
			return err
		}

		msg2, err := rw.ReadString('\n')
		return fmt.Errorf(strings.TrimSpace(msg) + msg2)
	}
	return nil
}

func copyPathTo(c *ssh.Client, src, target string) error {
	// start sink from the target
	h, err := newSessionHandle(c, "scp -tr "+target)
	if err != nil {
		return err
	}

	// leak safe guard
	success := []bool{false}
	defer func() {
		if !success[0] {
			h.close()
		}
	}()

	rw := bufio.NewReadWriter(bufio.NewReader(h.stdout), bufio.NewWriter(h.stdin))

	fi, err := os.Stat(src)
	if !fi.IsDir() {
		return copyFileTo(rw, src)
	}

	children, err := ioutil.ReadDir(src)
	if err != nil {
		return err
	}

	// copy children files
	for _, fi := range children {
		if fi.IsDir() {
			continue
		}
		if err := copyFileTo(rw, filepath.Join(src, fi.Name())); err != nil {
			return err
		}
	}

	// make children dirs
	for _, fi := range children {
		if !fi.IsDir() {
			continue
		}
		if err := sendScpCmd(rw, fmt.Sprintf("D0755 0 "+fi.Name())); err != nil {
			return err
		}
		if err := sendScpCmd(rw, "E"); err != nil {
			return err
		}
	}

	success[0] = true
	h.close()

	for _, fi := range children {
		if fi.IsDir() {
			if err := copyPathTo(c, filepath.Join(src, fi.Name()), filepath.Join(target, fi.Name())); err != nil {
				return err
			}
		}
	}

	return nil
}

func copyFileTo(rw *bufio.ReadWriter, src string) error {
	fi, err := os.Stat(src)
	if err != nil {
		return err
	}

	if err := sendScpCmd(rw, fmt.Sprintf("C0%s %d %s", strconv.FormatUint(uint64(fi.Mode()), 8), fi.Size(), fi.Name())); err != nil {
		return err
	}

	file, err := os.Open(src)
	if err != nil {
		return err
	}
	if _, err := io.Copy(rw, file); err != nil {
		return err
	}
	if err := rw.Flush(); err != nil {
		return err
	}
	if err := rw.WriteByte(0); err != nil {
		return err
	}
	if err := rw.Flush(); err != nil {
		return err
	}

	return handleScpResp(rw)
}

func (s *Session) Close() error {
	return s.c.Close()
}
