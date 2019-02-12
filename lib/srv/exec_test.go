/*
Copyright 2015-2018 Gravitational, Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package srv

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/user"
	"path"
	"path/filepath"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/gravitational/teleport/lib/auth"
	authority "github.com/gravitational/teleport/lib/auth/testauthority"
	"github.com/gravitational/teleport/lib/backend/lite"
	"github.com/gravitational/teleport/lib/events"
	"github.com/gravitational/teleport/lib/pam"
	"github.com/gravitational/teleport/lib/services"
	rsession "github.com/gravitational/teleport/lib/session"
	"github.com/gravitational/teleport/lib/utils"

	"github.com/docker/docker/pkg/term"
	"github.com/jonboulle/clockwork"
	"gopkg.in/check.v1"
)

func TestExec(t *testing.T) { check.TestingT(t) }

// ExecSuite also implements ssh.ConnMetadata
type ExecSuite struct {
	usr        *user.User
	ctx        *ServerContext
	localAddr  net.Addr
	remoteAddr net.Addr
}

var _ = check.Suite(&ExecSuite{})
var _ = fmt.Printf

func (s *ExecSuite) SetUpSuite(c *check.C) {
	utils.InitLoggerForTests(testing.Verbose())

	bk, err := lite.NewWithConfig(context.TODO(), lite.Config{Path: c.MkDir()})
	c.Assert(err, check.IsNil)

	clusterName, err := services.NewClusterName(services.ClusterNameSpecV2{
		ClusterName: "localhost",
	})
	c.Assert(err, check.IsNil)

	c.Assert(err, check.IsNil)
	a, err := auth.NewAuthServer(&auth.InitConfig{
		Backend:     bk,
		Authority:   authority.New(),
		ClusterName: clusterName,
	})
	c.Assert(err, check.IsNil)
	a.SetClusterName(clusterName)

	// set static tokens
	staticTokens, err := services.NewStaticTokens(services.StaticTokensSpecV2{
		StaticTokens: []services.ProvisionTokenV1{},
	})
	c.Assert(err, check.IsNil)
	err = a.SetStaticTokens(staticTokens)
	c.Assert(err, check.IsNil)

	dir := c.MkDir()
	f, err := os.Create(filepath.Join(dir, "fake"))
	c.Assert(err, check.IsNil)

	s.usr, _ = user.Current()
	s.ctx = &ServerContext{IsTestStub: true, srv: &fakeServer{accessPoint: a, id: "00000000-0000-0000-0000-000000000000"}}
	s.ctx.Identity.Login = s.usr.Username
	s.ctx.session = &session{id: "xxx", term: &fakeTerminal{f: f}}
	s.ctx.Identity.TeleportUser = "galt"
	s.ctx.Conn = &ssh.ServerConn{Conn: s}
	s.ctx.ExecRequest = &localExec{Ctx: s.ctx}

	term, err := newLocalTerminal(s.ctx)
	c.Assert(err, check.IsNil)
	term.SetTermType("xterm")
	s.ctx.session.term = term

	s.localAddr, _ = utils.ParseAddr("127.0.0.1:3022")
	s.remoteAddr, _ = utils.ParseAddr("10.0.0.5:4817")
}

func (s *ExecSuite) TearDownSuite(c *check.C) {
	s.ctx.session.term.Close()
}

func (s *ExecSuite) TestOSCommandPrep(c *check.C) {
	expectedEnv := []string{
		"LANG=en_US.UTF-8",
		getDefaultEnvPath("1000", defaultLoginDefsPath),
		fmt.Sprintf("HOME=%s", s.usr.HomeDir),
		fmt.Sprintf("USER=%s", s.usr.Username),
		"SHELL=/bin/sh",
		"SSH_TELEPORT_USER=galt",
		"SSH_SESSION_WEBPROXY_ADDR=<proxyhost>:3080",
		"SSH_TELEPORT_HOST_UUID=00000000-0000-0000-0000-000000000000",
		"SSH_TELEPORT_CLUSTER_NAME=localhost",
		"TERM=xterm",
		"SSH_CLIENT=10.0.0.5 4817 3022",
		"SSH_CONNECTION=10.0.0.5 4817 127.0.0.1 3022",
		fmt.Sprintf("SSH_TTY=%v", s.ctx.session.term.TTY().Name()),
		"SSH_SESSION_ID=xxx",
	}

	// empty command (simple shell)
	cmd, err := prepareInteractiveCommand(s.ctx)
	c.Assert(err, check.IsNil)
	c.Assert(cmd, check.NotNil)
	c.Assert(cmd.Path, check.Equals, "/bin/sh")
	c.Assert(cmd.Args, check.DeepEquals, []string{"-sh"})
	c.Assert(cmd.Dir, check.Equals, s.usr.HomeDir)
	c.Assert(cmd.Env, check.DeepEquals, expectedEnv)

	// non-empty command (exec a prog)
	s.ctx.IsTestStub = true
	s.ctx.ExecRequest.SetCommand("ls -lh /etc")
	cmd, err = prepareCommand(s.ctx)
	c.Assert(err, check.IsNil)
	c.Assert(cmd, check.NotNil)
	c.Assert(cmd.Path, check.Equals, "/bin/sh")
	c.Assert(cmd.Args, check.DeepEquals, []string{"/bin/sh", "-c", "ls -lh /etc"})
	c.Assert(cmd.Dir, check.Equals, s.usr.HomeDir)
	c.Assert(cmd.Env, check.DeepEquals, expectedEnv)

	// command without args
	s.ctx.ExecRequest.SetCommand("top")
	cmd, err = prepareCommand(s.ctx)
	c.Assert(err, check.IsNil)
	c.Assert(cmd.Path, check.Equals, "/bin/sh")
	c.Assert(cmd.Args, check.DeepEquals, []string{"/bin/sh", "-c", "top"})
}

func (s *ExecSuite) TestLoginDefsParser(c *check.C) {
	expectedEnvSuPath := "PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin:/bar"
	expectedSuPath := "PATH=/usr/local/bin:/usr/bin:/bin:/foo"

	c.Assert(getDefaultEnvPath("0", "../../fixtures/login.defs"), check.Equals, expectedEnvSuPath)
	c.Assert(getDefaultEnvPath("1000", "../../fixtures/login.defs"), check.Equals, expectedSuPath)
	c.Assert(getDefaultEnvPath("1000", "bad/file"), check.Equals, defaultEnvPath)
}

// implementation of ssh.Conn interface
func (s *ExecSuite) User() string                                           { return s.usr.Username }
func (s *ExecSuite) SessionID() []byte                                      { return []byte{1, 2, 3} }
func (s *ExecSuite) ClientVersion() []byte                                  { return []byte{1} }
func (s *ExecSuite) ServerVersion() []byte                                  { return []byte{1} }
func (s *ExecSuite) RemoteAddr() net.Addr                                   { return s.remoteAddr }
func (s *ExecSuite) LocalAddr() net.Addr                                    { return s.localAddr }
func (s *ExecSuite) Close() error                                           { return nil }
func (s *ExecSuite) SendRequest(string, bool, []byte) (bool, []byte, error) { return false, nil, nil }
func (s *ExecSuite) OpenChannel(string, []byte) (ssh.Channel, <-chan *ssh.Request, error) {
	return nil, nil, nil
}
func (s *ExecSuite) Wait() error { return nil }

// findExecutable helper finds a given executable name (like 'ls') in $PATH
// and returns the full path
func findExecutable(execName string) string {
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		fp := path.Join(dir, execName)
		if utils.IsFile(fp) {
			return fp
		}
	}
	return "not found in $PATH: " + execName
}

type fakeTerminal struct {
	f *os.File
}

// AddParty adds another participant to this terminal. We will keep the
// Terminal open until all participants have left.
func (f *fakeTerminal) AddParty(delta int) {
}

// Run will run the terminal.
func (f *fakeTerminal) Run() error {
	return nil
}

// Wait will block until the terminal is complete.
func (f *fakeTerminal) Wait() (*ExecResult, error) {
	return nil, nil
}

// Kill will force kill the terminal.
func (f *fakeTerminal) Kill() error {
	return nil
}

// PTY returns the PTY backing the terminal.
func (f *fakeTerminal) PTY() io.ReadWriter {
	return nil
}

// TTY returns the TTY backing the terminal.
func (f *fakeTerminal) TTY() *os.File {
	return f.f
}

// Close will free resources associated with the terminal.
func (f *fakeTerminal) Close() error {
	return f.f.Close()
}

// GetWinSize returns the window size of the terminal.
func (f *fakeTerminal) GetWinSize() (*term.Winsize, error) {
	return &term.Winsize{}, nil
}

// SetWinSize sets the window size of the terminal.
func (f *fakeTerminal) SetWinSize(params rsession.TerminalParams) error {
	return nil
}

// GetTerminalParams is a fast call to get cached terminal parameters
// and avoid extra system call.
func (f *fakeTerminal) GetTerminalParams() rsession.TerminalParams {
	return rsession.TerminalParams{}
}

// SetTerminalModes sets the terminal modes from "pty-req"
func (f *fakeTerminal) SetTerminalModes(ssh.TerminalModes) {
	return
}

// GetTermType gets the terminal type set in "pty-req"
func (f *fakeTerminal) GetTermType() string {
	return "xterm"
}

// SetTermType sets the terminal type from "pty-req"
func (f *fakeTerminal) SetTermType(string) {
}

// fakeServer is stub for tests
type fakeServer struct {
	accessPoint auth.AccessPoint
	id          string
}

func (f *fakeServer) ID() string {
	return f.id
}

func (f *fakeServer) GetNamespace() string {
	return ""
}

func (f *fakeServer) AdvertiseAddr() string {
	return ""
}

func (f *fakeServer) Component() string {
	return ""
}

func (f *fakeServer) PermitUserEnvironment() bool {
	return true
}

func (f *fakeServer) EmitAuditEvent(string, events.EventFields) {
}

func (f *fakeServer) GetAuditLog() events.IAuditLog {
	return nil
}

func (f *fakeServer) GetAccessPoint() auth.AccessPoint {
	return f.accessPoint
}

func (f *fakeServer) GetSessionServer() rsession.Service {
	return nil
}

func (f *fakeServer) GetDataDir() string {
	return ""
}

func (f *fakeServer) GetPAM() (*pam.Config, error) {
	return nil, nil
}

func (f *fakeServer) GetClock() clockwork.Clock {
	return nil
}

func (f *fakeServer) GetInfo() services.Server {
	return nil
}
