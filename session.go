// Acknowledgement: the scp implementation is heavily influenced by https://github.com/deoxxa/scp

package ssh

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

	gossh "golang.org/x/crypto/ssh"
)

type sessionHandle struct {
	s      *gossh.Session
	stdin  io.WriteCloser
	stdout io.Reader
	stderr io.Reader
}

func newSessionHandle(c *gossh.Client, cmd string) (*sessionHandle, error) {
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
	c *gossh.Client
	h *sessionHandle
}

func NewSession(hostport, user, pwd string, to time.Duration) (*Session, error) {
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
		Timeout:         to,
	}

	c, err := gossh.Dial("tcp", hostport, cfg)
	if err != nil {
		return nil, err
	}

	h, err := newSessionHandle(c, "/bin/bash")
	if err != nil {
		c.Close()
		return nil, err
	}

	return &Session{
		c: c,
		h: h,
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

	if _, err := s.h.stdin.Write([]byte(cmd + "\n")); err != nil {
		return nil, nil, err
	}
	// TODO: if the following writes fail, how could we clear the data from stdout/stderr
	if _, err := s.h.stdin.Write([]byte("echo '" + string(endMark) + "'\n")); err != nil {
		return nil, nil, err
	}
	if _, err := s.h.stdin.Write([]byte("echo '" + string(endMark) + "' >&2\n")); err != nil {
		return nil, nil, err
	}

	outCh := make(chan []byte, 16)
	errCh := make(chan []byte, 16)
	go recv(s.h.stdout, outCh)
	go recv(s.h.stderr, errCh)

	return outCh, errCh, nil
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

func copyPathTo(c *gossh.Client, src, target string) error {
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

func Log(outCh chan []byte, errCh chan []byte) {
	var wg sync.WaitGroup
	if outCh != nil {
		wg.Add(1)
		go func(g *sync.WaitGroup) {
			for line := range outCh {
				fmt.Printf("%v\n", string(line))
			}
			g.Done()
		}(&wg)
	}
	if errCh != nil {
		wg.Add(1)
		go func(g *sync.WaitGroup) {
			for line := range errCh {
				fmt.Printf("%v\n", string(line))
			}
			g.Done()
		}(&wg)
	}
	wg.Wait()
}

func (s *Session) Close() error {
	s.h.close()
	return s.c.Close()
}
