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

func NewSession(hostport, user, pwd string, skPath string, to time.Duration) (*Session, error) {
	var am ssh.AuthMethod
	if len(pwd) > 0 {
		am = ssh.Password(pwd)
	} else {
		if skPath == "" {
			skPath = filepath.Join(os.Getenv("HOME"), "/.ssh/id_rsa")
		}
		pk, err := ioutil.ReadFile(skPath)
		if err != nil {
			return nil, err
		}
		signer, err := ssh.ParsePrivateKey(pk)
		if err != nil {
			return nil, err
		}
		am = ssh.PublicKeys(signer)
	}

	if int64(to) == 0 {
		to = 365 * 24 * time.Hour
	}

	cfg := &ssh.ClientConfig{
		User:            user,
		Auth:            []ssh.AuthMethod{am},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		BannerCallback:  func(message string) error { return nil }, // ignore banner
		Timeout:         to,
	}

	c, err := ssh.Dial("tcp", hostport, cfg)
	if err != nil {
		return nil, err
	}

	return &Session{
		c: c,
	}, nil
}

func NewSessionWithRetry(hostport, user, pwd string, skPath string, to time.Duration) (*Session, error) {
	if int64(to) == 0 {
		to = 365 * 24 * time.Hour
	}

	deadline := time.Now().Add(to)

	s, err := NewSession(hostport, user, pwd, skPath, time.Second)
	if err != nil {
		ticker := time.NewTicker(time.Second)
		timeout := time.After(deadline.Sub(time.Now()))
		for err != nil {
			if strings.Contains(err.Error(), "unable to authenticate") {
				return nil, err
			}

			select {
			case <-timeout:
				return nil, fmt.Errorf("connection timed out %v", err)
			case <-ticker.C:
				s, err = NewSession(hostport, user, pwd, skPath, time.Second)
			}
		}
	}

	return s, err
}

type Cmd struct {
	h     *sessionHandle
	outCh chan []byte
	errCh chan []byte
}

func (c *Cmd) Stdout() chan []byte {
	return c.outCh
}

func (c *Cmd) Stderr() chan []byte {
	return c.errCh
}

func (c *Cmd) CombinedOut() chan []byte {
	ch := make(chan []byte)

	var wg sync.WaitGroup
	if c.outCh != nil {
		wg.Add(1)
		go func(g *sync.WaitGroup, out chan []byte) {
			for line := range c.outCh {
				out <- line
			}
			g.Done()
		}(&wg, ch)
	}
	if c.errCh != nil {
		wg.Add(1)
		go func(g *sync.WaitGroup, out chan []byte) {
			for line := range c.errCh {
				out <- line
			}
			g.Done()
		}(&wg, ch)
	}

	go func(g *sync.WaitGroup, out chan []byte) {
		g.Wait()
		close(out)
	}(&wg, ch)

	return ch
}

func (c *Cmd) Close() {
	c.h.close()
}

func (s *Session) Run(cmd string) (*Cmd, error) {
	endMark := fmt.Sprintf("$$__%v__$$", time.Now().UnixNano())

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

			if len(bytes) == len(endMark) && string(bytes) == endMark {
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
	if _, err := h.stdin.Write([]byte("echo '" + endMark + "'\n")); err != nil {
		return nil, err
	}
	if _, err := h.stdin.Write([]byte("echo '" + endMark + "' >&2\n")); err != nil {
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
