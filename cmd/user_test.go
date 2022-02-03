package cmd

import (
	"github.com/stretchr/testify/require"
	"github.com/urfave/cli/v2"
	"heckel.io/ntfy/server"
	"heckel.io/ntfy/test"
	"path/filepath"
	"testing"
)

func TestCLI_User_Add(t *testing.T) {
	s, conf, port := newTestServerWithAuth(t)
	defer test.StopServer(t, s, port)

	app, stdin, _, stderr := newTestApp()
	stdin.WriteString("mypass\nmypass")
	require.Nil(t, runUserCommand(app, conf, "add", "phil"))
	require.Contains(t, stderr.String(), "user phil added with role user")
}

func TestCLI_User_Add_Exists(t *testing.T) {
	s, conf, port := newTestServerWithAuth(t)
	defer test.StopServer(t, s, port)

	app, stdin, _, stderr := newTestApp()
	stdin.WriteString("mypass\nmypass")
	require.Nil(t, runUserCommand(app, conf, "add", "phil"))
	require.Contains(t, stderr.String(), "user phil added with role user")

	app, stdin, _, stderr = newTestApp()
	stdin.WriteString("mypass\nmypass")
	err := runUserCommand(app, conf, "add", "phil")
	require.Error(t, err)
	require.Contains(t, err.Error(), "user phil already exists")
}

func TestCLI_User_Add_Admin(t *testing.T) {
	s, conf, port := newTestServerWithAuth(t)
	defer test.StopServer(t, s, port)

	app, stdin, _, stderr := newTestApp()
	stdin.WriteString("mypass\nmypass")
	require.Nil(t, runUserCommand(app, conf, "add", "--role=admin", "phil"))
	require.Contains(t, stderr.String(), "user phil added with role admin")
}

func TestCLI_User_Add_Password_Mismatch(t *testing.T) {
	s, conf, port := newTestServerWithAuth(t)
	defer test.StopServer(t, s, port)

	app, stdin, _, _ := newTestApp()
	stdin.WriteString("mypass\nNOTMATCH")
	err := runUserCommand(app, conf, "add", "phil")
	require.Error(t, err)
	require.Contains(t, err.Error(), "passwords do not match: try it again, but this time type slooowwwlly")
}

func TestCLI_User_ChangePass(t *testing.T) {
	s, conf, port := newTestServerWithAuth(t)
	defer test.StopServer(t, s, port)

	// Add user
	app, stdin, _, stderr := newTestApp()
	stdin.WriteString("mypass\nmypass")
	require.Nil(t, runUserCommand(app, conf, "add", "phil"))
	require.Contains(t, stderr.String(), "user phil added with role user")

	// Change pass
	app, stdin, _, stderr = newTestApp()
	stdin.WriteString("newpass\nnewpass")
	require.Nil(t, runUserCommand(app, conf, "change-pass", "phil"))
	require.Contains(t, stderr.String(), "changed password for user phil")
}

func newTestServerWithAuth(t *testing.T) (s *server.Server, conf *server.Config, port int) {
	conf = server.NewConfig()
	conf.AuthFile = filepath.Join(t.TempDir(), "user.db")
	conf.AuthDefaultRead = false
	conf.AuthDefaultWrite = false
	s, port = test.StartServerWithConfig(t, conf)
	return
}

func runUserCommand(app *cli.App, conf *server.Config, args ...string) error {
	var defaultAccess string
	if conf.AuthDefaultRead && conf.AuthDefaultWrite {
		defaultAccess = "read-write"
	} else if conf.AuthDefaultRead && !conf.AuthDefaultWrite {
		defaultAccess = "read-only"
	} else if !conf.AuthDefaultRead && conf.AuthDefaultWrite {
		defaultAccess = "write-only"
	} else if !conf.AuthDefaultRead && !conf.AuthDefaultWrite {
		defaultAccess = "deny-all"
	}
	userArgs := []string{
		"ntfy",
		"user",
		"--auth-file=" + conf.AuthFile,
		"--auth-default-access=" + defaultAccess,
	}
	return app.Run(append(userArgs, args...))
}
