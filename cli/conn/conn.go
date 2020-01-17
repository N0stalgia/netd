// NetD makes network device operations easy.
// Copyright (C) 2019  sky-cloud.net
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU General Public License for more details.
//
// You should have received a copy of the GNU General Public License
// along with this program.  If not, see <https://www.gnu.org/licenses/>.

package conn

import (
	"bufio"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"

	"github.com/sky-cloud-tec/netd/cli"
	"github.com/sky-cloud-tec/netd/protocol"
	"github.com/songtianyi/rrframework/logs"

	"github.com/sky-cloud-tec/netd/common"
	"golang.org/x/crypto/ssh"

	"github.com/ziutek/telnet"
)

var (
	conns map[string]*CliConn
	semas map[string]chan struct{}
)

func init() {
	conns = make(map[string]*CliConn, 0)
	semas = make(map[string]chan struct{}, 0)
}

// CliConn cli connection
type CliConn struct {
	t    int                  // connection type 0 = ssh, 1 = telnet
	mode string               // device cli mode
	req  *protocol.CliRequest // cli request
	op   cli.Operator         // cli operator

	conn   *telnet.Conn // telnet connection
	client *ssh.Client  // ssh client

	session *ssh.Session   // ssh session
	r       io.Reader      // ssh session stdout
	w       io.WriteCloser // ssh session stdin
}

// Acquire cli conn
func Acquire(req *protocol.CliRequest, op cli.Operator) (*CliConn, error) {
	// limit concurrency to 1
	// there only one req for one connection always
	logs.Info(req.LogPrefix, "Acquiring sema...")
	if semas[req.Address] == nil {
		semas[req.Address] = make(chan struct{}, 1)
	}
	// try
	semas[req.Address] <- struct{}{}
	logs.Info(req.LogPrefix, "sema acquired")
	if req.Mode == "" {
		req.Mode = op.GetStartMode()
	}
	// if cli conn already created
	if v, ok := conns[req.Address]; ok {
		v.req = req
		v.op = op
		logs.Info(req.LogPrefix, "cli conn exist")
		return v, nil
	}
	c, err := newCliConn(req, op)
	if err != nil {
		return nil, err
	}
	conns[req.Address] = c
	return c, nil
}

// Release cli conn
func Release(req *protocol.CliRequest) {
	if len(semas[req.Address]) > 0 {
		logs.Info(req.LogPrefix, "Releasing sema")
		<-semas[req.Address]
	}
	logs.Info(req.LogPrefix, "sema released")
}

func newCliConn(req *protocol.CliRequest, op cli.Operator) (*CliConn, error) {
	logs.Info(req.LogPrefix, "creating cli conn...")
	if strings.ToLower(req.Protocol) == "ssh" {
		sshConfig := &ssh.ClientConfig{
			User:            req.Auth.Username,
			Auth:            []ssh.AuthMethod{ssh.Password(req.Auth.Password)},
			HostKeyCallback: ssh.InsecureIgnoreHostKey(),
			Timeout:         5 * time.Second,
		}
		sshConfig.SetDefaults()
		sshConfig.Ciphers = append(sshConfig.Ciphers, []string{"aes128-cbc", "3des-cbc"}...)
		client, err := ssh.Dial("tcp", req.Address, sshConfig)
		if err != nil {
			logs.Error(req.LogPrefix, "dial", req.Address, "error", err)
			return nil, fmt.Errorf("%s dial %s error, %s", req.LogPrefix, req.Address, err)
		}
		c := &CliConn{t: common.SSHConn, client: client, req: req, op: op, mode: op.GetStartMode()}
		if err := c.init(); err != nil {
			c.Close()
			return nil, err
		}
		return c, nil
	} else if strings.ToLower(req.Protocol) == "telnet" {
		conn, err := telnet.DialTimeout("tcp", req.Address, 5*time.Second)
		if err != nil {
			return nil, fmt.Errorf("[ %s ] dial %s error, %s", req.Device, req.Address, err)
		}
		c := &CliConn{t: common.TELNETConn, conn: conn, req: req, op: op, mode: op.GetStartMode()}
		return c, nil
	}
	return nil, fmt.Errorf("protocol %s not support", req.Protocol)
}

func (s *CliConn) heartbeat() {
	go func() {
		tick := time.Tick(30 * time.Second)
		for {
			select {
			case <-tick:
				// try
				logs.Info(s.req.LogPrefix, "Acquiring heartbeat sema...")
				semas[s.req.Address] <- struct{}{}
				logs.Info(s.req.LogPrefix, "heartbeat sema acquired")
				if _, err := s.writeBuff(""); err != nil {
					semas[s.req.Address] <- struct{}{}
					logs.Critical(s.req.LogPrefix, "heartbeat error,", err)
					s.Close()
					return
				}
				if _, _, err := s.readBuff(); err != nil {
					semas[s.req.Address] <- struct{}{}
					logs.Critical(s.req.LogPrefix, "heartbeat error,", err)
					s.Close()
					return
				}
				<-semas[s.req.Address]
			}
		}
	}()
}

func (s *CliConn) init() error {
	if s.t == common.SSHConn {
		f := s.op.GetSSHInitializer()
		var err error
		s.r, s.w, s.session, err = f(s.client, s.req)
		if err != nil {
			return err
		}
		// read login prompt
		_, prompt, err := s.readBuff()
		if err != nil {
			return fmt.Errorf("read after login failed, %s", err)
		}
		// enable cases
		if s.mode == "login_or_login_enable" {
			// check prompt
			loginPrompts := s.op.GetPrompts("login")
			if cli.Match(loginPrompts, prompt) {
				s.mode = "login"
				if s.mode != s.req.Mode {
					// login is not the target mode, need transition
					// enter privileged mode
					if _, err := s.writeBuff("enable" + s.op.GetLinebreak() + s.req.EnablePwd); err != nil {
						return fmt.Errorf("enter privileged mode err, %s", err)
					}
					s.mode = "login_enable"
					if _, _, err := s.readBuff(); err != nil {
						s.mode = "login"
						return fmt.Errorf("readBuff after enable err, %s", err)
					}
					if err := s.closePage(); err != nil {
						return err
					}
				}
			}
		} else {
			if strings.EqualFold(s.req.Vendor, "Paloalto") && strings.EqualFold(s.req.Type, "PAN-OS") {
				// set format
				if s.req.Format != "" {
					if _, err := s.writeBuff("set cli config-output-format " + s.req.Format); err != nil {
						return err
					}
				}
				// close page
				if err := s.closePage(); err != nil {
                                	return err
                        	}
			} else if strings.EqualFold(s.req.Vendor, "fortinet") && strings.EqualFold(s.req.Type, "fortigate-VM64-KVM") {
				if pts := s.op.GetPrompts(s.req.Mode); pts != nil {
					//no vdom
					if !strings.Contains(pts[0].String(), s.req.Mode) {
						return s.closePage()
					}
					logs.Debug("entering domain global...")
					if _, err := s.writeBuff("config global"); err != nil {
						return err
					}
					if err := s.closePage(); err != nil {
						return err
					}
					logs.Debug("exiting vdom global ...") 
					if _, err := s.writeBuff("end"); err != nil {
						return err
					}
					if _, _, err := s.readBuff(); err != nil {
						return err;
					}
				}
			} else {
				if err := s.closePage(); err != nil {
                                	return err
                        	}		
			}
		}
	}
	s.heartbeat()
	return nil
}

func (s *CliConn) closePage() error {
	if strings.EqualFold(s.req.Vendor, "cisco") && (strings.EqualFold(s.req.Type, "asa") || strings.EqualFold(s.req.Type, "asav")) {
		// ===config or normal both ok===
		// set terminal pager
		if _, err := s.writeBuff("terminal pager 0"); err != nil {
			return err
		}
		// set page lines
		if _, err := s.writeBuff("terminal pager lines 0"); err != nil {
			return err
		}
	} else if strings.EqualFold(s.req.Vendor, "cisco") && strings.EqualFold(s.req.Type, "ios") {
		if _, err := s.writeBuff("terminal length 0"); err != nil {
			return err
		}
	} else if strings.EqualFold(s.req.Vendor, "Paloalto") && strings.EqualFold(s.req.Type, "PAN-OS") {
		// set pager
		if _, err := s.writeBuff("set cli pager off"); err != nil {
			return err
		}
	} else if strings.EqualFold(s.req.Vendor, "hillstone") && strings.EqualFold(s.req.Type, "SG-6000-VM01") {
		// set pager
		if _, err := s.writeBuff("terminal length 0"); err != nil {
			return err
		}
	} else if strings.EqualFold(s.req.Vendor, "fortinet") && strings.EqualFold(s.req.Type, "fortigate-VM64-KVM") {
		// set console
		if _, err := s.writeBuff("config system console\n\tset output standard\nend"); err != nil {
			return err
		}
	}
	return nil
}

// Close cli conn
func (s *CliConn) Close() error {
	delete(conns, s.req.Address)
	if s.t == common.TELNETConn {
		if s.conn == nil {
			logs.Info("telnet conn nil when close")
			return nil
		}
		return s.conn.Close()
	}
	if s.session != nil {
		if err := s.session.Close(); err != nil {
			return err
		}
	} else {
		logs.Notice("ssh session nil when close")
	}
	if s.client == nil {
		logs.Notice("ssh conn nil when close")
		return nil
	}
	return s.client.Close()
}

func (s *CliConn) read(buff []byte) (int, error) {
	if s.t == common.SSHConn {
		return s.r.Read(buff)
	}
	return s.conn.Read(buff)
}

func (s *CliConn) write(b []byte) (int, error) {
	if s.t == common.SSHConn {
		return s.w.Write(b)
	}
	return s.conn.Write(b)
}

type readBuffOut struct {
	err    error
	ret    string
	prompt string
}

func (s *CliConn) findLastLine(t string) string {
	scanner := bufio.NewScanner(strings.NewReader(t))
	var last string
	for scanner.Scan() {
		s := scanner.Text()
		if len(s) > 0 {
			last = s
		}
	}
	return last
}

// AnyPatternMatches return matched string slice if any pattern fullfil
func (s *CliConn) anyPatternMatches(t string, patterns []*regexp.Regexp) []string {
	for _, v := range patterns {
		matches := v.FindStringSubmatch(t)
		if len(matches) != 0 {
			return matches
		}
	}
	return nil
}

func (s *CliConn) readLines() *readBuffOut {
	buf := make([]byte, 1000)
	var (
		waitingString, lastLine string
		errRes                  error
	)
	for {
		n, err := s.read(buf) //this reads the ssh/telnet terminal
		if err != nil {
			// something wrong
			logs.Error(s.req.LogPrefix, "io.Reader read error,", err)
			errRes = err
			break
		}
		// for every line
		current := string(buf[:n])
		logs.Debug(s.req.LogPrefix, "(", n, ")", current)
		lastLine = s.findLastLine(waitingString + current)
		if s.op.GetPrompts(s.mode) == nil {
			logs.Error(s.req.LogPrefix, "no patterns for mode", s.mode)
			errRes = fmt.Errorf("%s no patterns for mode %s", s.req.LogPrefix, s.mode)
			break
		}
		matches := s.anyPatternMatches(lastLine, s.op.GetPrompts(s.mode))
		if len(matches) > 0 {
			logs.Info(s.req.LogPrefix, "prompt matched", s.mode, ":", matches)
			waitingString = strings.TrimSuffix(waitingString+current, matches[0])
			break
		}
		// add current line to result string
		waitingString += current
	}
	return &readBuffOut{
		errRes,
		waitingString,
		lastLine,
	}
}

// return cmd output, prompt, error
func (s *CliConn) readBuff() (string, string, error) {
	// buffered chan
	ch := make(chan *readBuffOut, 1)

	go func() {
		ch <- s.readLines()
	}()

	select {
	case res := <-ch:
		if res.err == nil {
			scanner := bufio.NewScanner(strings.NewReader(res.ret))
			for scanner.Scan() {
				matches := s.anyPatternMatches(scanner.Text(), s.op.GetErrPatterns())
				if len(matches) > 0 {
					logs.Info(s.req.LogPrefix, "err pattern matched,", matches)
					return "", res.prompt, fmt.Errorf("err pattern matched, %s", matches)
				}
			}
		}
		return res.ret, res.prompt, res.err
	case <-time.After(s.req.Timeout):
		return "", "", fmt.Errorf("read stdout timeout after %q", s.req.Timeout)
	}
}

func (s *CliConn) writeBuff(cmd string) (int, error) {
	return s.write([]byte(cmd + s.op.GetLinebreak()))
}

// Exec execute cli cmds
func (s *CliConn) Exec() (map[string]string, error) {
	// transit to target mode
	if s.req.Mode != s.mode {
		cmds := s.op.GetTransitions(s.mode, s.req.Mode)
		// use target mode prompt
		logs.Info(s.req.LogPrefix, s.mode, "-->", s.req.Mode)
		s.mode = s.req.Mode
		for _, v := range cmds {
			logs.Info(s.req.LogPrefix, "exec", "<", v, ">")
			if _, err := s.writeBuff(v); err != nil {
				logs.Error(s.req.LogPrefix, "write buff failed,", err)
				return nil, fmt.Errorf("write buff failed, %s", err)
			}
			_, _, err := s.readBuff()
			if err != nil {
				logs.Error(s.req.LogPrefix, "readBuff failed,", err)
				return nil, fmt.Errorf("readBuff failed, %s", err)
			}
		}
	}
	cmdstd := make(map[string]string, 0)
	// do execute cli commands
	for _, v := range s.req.Commands {
		logs.Info(s.req.LogPrefix, "exec", "<", v, ">")
		if _, err := s.writeBuff(v); err != nil {
			logs.Error(s.req.LogPrefix, "write buff failed,", err)
			return cmdstd, fmt.Errorf("write buff failed, %s", err)
		}
		ret, _, err := s.readBuff()
		if err != nil {
			logs.Error(s.req.LogPrefix, "readBuff failed,", err)
			return cmdstd, fmt.Errorf("readBuff failed, %s", err)
		}
		cmdstd[v] = ret
	}
	return cmdstd, nil
}
