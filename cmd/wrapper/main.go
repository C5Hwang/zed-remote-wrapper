package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"zed-remote-wrapper/internal/paths"
	"zed-remote-wrapper/internal/protocol"
)

const usage = `zed-remote-wrapper — forwards file-open requests to a local Zed over an SSH unix socket.

USAGE:
  zed [OPTIONS] [PATH[:LINE[:COL]]]...

OPTIONS:
  -a, --add           Add paths to the current workspace.
  -n, --new           Open in a new workspace.
  -e, --existing      Open in an existing window.
  -v, --version       Print wrapper version.
  -h, --help          Show this message.
  --                  End of options; remaining args are literal paths.

CONFIGURATION
  Values are read first from environment variables, then from the config files
  ~/.config/zed-remote.conf and /etc/zed-remote.conf.

  LC_ZED_REMOTE_HOST   SSH host alias used by the local listener to open Zed.
                       Config key: host=<alias>  (required)

  LC_ZED_REMOTE_SOCK   Unix socket path that the wrapper dials.  Must match the
                       path used in sshd_config RemoteForward on the server.
                       Config key: sock=<path>  (required if not set via env)

  LC_ZED_REMOTE_USER   SSH username forwarded to the listener so it can connect
                       back to the remote server.  Defaults to empty (listener
                       uses its own ssh config).
                       Config key: user=<username>

  LC_ZED_REMOTE_PORT   SSH port forwarded to the listener.  Defaults to 22.
                       Config key: port=<number>
`

const version = "zed-remote-wrapper 0.1.0"

type cliArgs struct {
	Add, New, Existing bool
	Paths              []string
}

func parseArgs(argv []string) (*cliArgs, error) {
	c := &cliArgs{}
	rest := false
	i := 0
	for i < len(argv) {
		a := argv[i]
		if rest {
			c.Paths = append(c.Paths, a)
			i++
			continue
		}
		switch {
		case a == "--":
			rest = true
		case a == "-a" || a == "--add":
			c.Add = true
		case a == "-n" || a == "--new":
			c.New = true
		case a == "-e" || a == "--existing":
			c.Existing = true
		case a == "-v" || a == "--version":
			fmt.Println(version)
			os.Exit(0)
		case a == "-h" || a == "--help":
			fmt.Print(usage)
			os.Exit(0)
		case strings.HasPrefix(a, "-"):
			return nil, fmt.Errorf("unknown option %q (use -- to pass literal paths starting with -)", a)
		default:
			c.Paths = append(c.Paths, a)
		}
		i++
	}
	return c, nil
}

func resolveUser() string {
	if v := os.Getenv("LC_ZED_REMOTE_USER"); v != "" {
		return v
	}
	home, _ := os.UserHomeDir()
	for _, p := range []string{filepath.Join(home, ".config", "zed-remote.conf"), "/etc/zed-remote.conf"} {
		if v := readConfValue(p, "user"); v != "" {
			return v
		}
	}
	return ""
}

func resolvePort() int {
	if v := os.Getenv("LC_ZED_REMOTE_PORT"); v != "" {
		if p, err := strconv.Atoi(v); err == nil && p > 0 {
			return p
		}
	}
	home, _ := os.UserHomeDir()
	for _, p := range []string{filepath.Join(home, ".config", "zed-remote.conf"), "/etc/zed-remote.conf"} {
		if v := readConfValue(p, "port"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				return n
			}
		}
	}
	return 22
}

func resolveHost() string {
	if v := os.Getenv("LC_ZED_REMOTE_HOST"); v != "" {
		return v
	}
	home, _ := os.UserHomeDir()
	for _, p := range []string{filepath.Join(home, ".config", "zed-remote.conf"), "/etc/zed-remote.conf"} {
		if v := readConfValue(p, "host"); v != "" {
			return v
		}
	}
	return ""
}

func readConfValue(path, key string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	for s.Scan() {
		line := strings.TrimSpace(s.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		if strings.TrimSpace(k) == key {
			return strings.Trim(strings.TrimSpace(v), `"'`)
		}
	}
	return ""
}

func socketPath() (string, error) {
	if v := os.Getenv("LC_ZED_REMOTE_SOCK"); v != "" {
		return v, nil
	}
	home, _ := os.UserHomeDir()
	for _, p := range []string{filepath.Join(home, ".config", "zed-remote.conf"), "/etc/zed-remote.conf"} {
		if v := readConfValue(p, "sock"); v != "" {
			return v, nil
		}
	}
	return "", fmt.Errorf("socket path not configured: set LC_ZED_REMOTE_SOCK or add sock= to ~/.config/zed-remote.conf")
}

func die(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}

func main() {
	args, err := parseArgs(os.Args[1:])
	if err != nil {
		die("%v", err)
	}

	host := resolveHost()
	if host == "" {
		die("host not configured: set LC_ZED_REMOTE_HOST or add host=ALIAS to ~/.config/zed-remote.conf")
	}

	cwd, err := os.Getwd()
	if err != nil {
		die("getwd -- %v", err)
	}
	home, _ := os.UserHomeDir()

	req := &protocol.Request{
		V:        protocol.Version,
		Host:     host,
		User:     resolveUser(),
		Port:     resolvePort(),
		Cwd:      cwd,
		Add:      args.Add,
		New:      args.New,
		Existing: args.Existing,
	}
	for _, p := range args.Paths {
		ps, err := paths.ParsePathSpec(p, cwd, home)
		if err != nil {
			die("resolve %q -- %v", p, err)
		}
		req.Paths = append(req.Paths, ps)
	}

	sock, err := socketPath()
	if err != nil {
		die("%v", err)
	}

	conn, err := net.Dial("unix", sock)
	if err != nil {
		die("%s not present -- %v", sock, err)
	}
	defer conn.Close()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	go func() {
		<-sigCh
		conn.Close()
	}()

	if err := protocol.EncodeRequest(conn, req); err != nil {
		die("send request -- %v", err)
	}

	br := bufio.NewReader(conn)
	exitCode := 0
	for {
		f, err := protocol.DecodeFrame(br)
		if err != nil {
			if errors.Is(err, io.EOF) {
				fmt.Fprintln(os.Stderr, "listener closed connection")
				os.Exit(1)
			}
			die("read frame -- %v", err)
		}
		switch f.T {
		case protocol.FrameOut:
			os.Stdout.Write(f.D)
		case protocol.FrameErr:
			os.Stderr.Write(f.D)
		case protocol.FrameError:
			fmt.Fprintf(os.Stderr, "listener error -- %s\n", f.Msg)
			exitCode = 1
		case protocol.FrameExit:
			exitCode = f.Code
			os.Exit(exitCode)
		}
	}
}
