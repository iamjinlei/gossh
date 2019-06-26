package ssh

import (
	"bufio"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
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

func (s *Session) CopyTo(src string, target string) error {
	// The scp implementation is heavily influenced by https://github.com/deoxxa/scp
	sess, err := s.c.NewSession()
	if err != nil {
		return err
	}
	defer sess.Close()

	stdout, err := sess.StdoutPipe()
	if err != nil {
		return err
	}

	stdin, err := sess.StdinPipe()
	if err != nil {
		return err
	}

	rw := bufio.NewReadWriter(bufio.NewReader(stdout), bufio.NewWriter(stdin))

	if err := sess.Start("scp -tr ~/"); err != nil {
		return err
	}

	//fi, err := os.Stat(src)

	if err := sendScpCmd(fmt.Sprintf("D0755 0 %s\n", target+"_1")); err != nil {
		return err
	}
	if err := s.copyFileTo(rw, src); err != ni {
		return err
	}
	if err := sendScpCmd(fmt.Sprintf("D0755 0 %s\n", target+"_2")); err != nil {
		return err
	}
	if err := s.copyFileTo(rw, src); err != ni {
		return err
	}

	return s.copyFileTo(rw, src)
}

func sendScpCmd(rw *bufio.ReadWriter, cmd string) error {
	if _, err := rw.WriteString(cmd + "\n"); err != nil {
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
	_, err := handleScpResp(rw)
	return err
}

func handleScpResp(rw *bufio.ReadWriter) ([]warnings, error) {
	var warnings []string
	if b, err := rw.ReadByte(); err != nil {
		return nil, err
	} else if b == 1 || b == 2 {
		msg, err := rw.ReadString('\n')
		if err != nil {
			return nil, err
		}

		msg = strings.TrimSpace(msg)
		if b == 2 {
			return nil, fmt.Errorf(msg)
		}

		warnings = append(warnings, msg)
	}

	return warnings, nil
}

func copyFileTo(rw *bufio.ReadWriter, src string) error {
	fi, err := os.Stat(src)
	if err != nil {
		return err
	}

	if err := sendScpCmd(fmt.Sprintf("C0%s %d %s\n", strconv.FormatUint(uint64(fi.Mode()), 8), fi.Size(), fi.Name())); err != nil {
		return err
	}

	file, err := os.Open(src)
	if err != nil {
		return err
	}
	if _, err := io.Copy(rw, file); err != nil {
		return err
	}
	if err := rw.WriteByte(0); err != nil {
		return err
	}
	if err := rw.Flush(); err != nil {
		return err
	}

	if _, err := handleScpResp(rw); err != nil {
		return err
	}

	return nil
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
	return s.c.Close()
}
