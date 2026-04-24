package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"os/user"
	"path/filepath"
	"strings"
	"syscall"

	"zed-remote-wrapper/internal/paths"
	"zed-remote-wrapper/internal/protocol"
)

const usage = `zed-remote wrapper — forwards file-open requests to a local Zed over an SSH unix socket.

USAGE:
  zed [OPTIONS] [PATH[:LINE[:COL]]]...

OPTIONS:
  -w, --wait          Wait for the opened paths to close before exiting.
  -a, --add           Add paths to the current workspace.
  -n, --new           Open in a new workspace.
  -e, --existing      Open in an existing window.
      --diff A B      Open a diff view between A and B. Can be repeated.
  -H, --host ALIAS    Override the ssh_config alias (default: $LC_ZED_REMOTE_HOST
                      or host= from ~/.config/zed-remote.conf or /etc/zed-remote.conf).
  -v, --version       Print wrapper version.
  -h, --help          Show this message.
  --                  End of options; remaining args are literal paths.

The wrapper connects to /tmp/zed-$USER.sock which must be forwarded via SSH
RemoteForward to a listener running on the local macOS laptop.
`

const version = "zed-remote-wrapper 0.1.0"

type cliArgs struct {
	Wait, Add, New, Existing bool
	Host                     string
	Paths                    []string
	DiffPairs                [][2]string
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
		case a == "-w" || a == "--wait":
			c.Wait = true
		case a == "-a" || a == "--add":
			c.Add = true
		case a == "-n" || a == "--new":
			c.New = true
		case a == "-e" || a == "--existing":
			c.Existing = true
		case a == "-H" || a == "--host":
			if i+1 >= len(argv) {
				return nil, fmt.Errorf("%s requires an argument", a)
			}
			c.Host = argv[i+1]
			i++
		case strings.HasPrefix(a, "-H="):
			c.Host = a[3:]
		case strings.HasPrefix(a, "--host="):
			c.Host = a[len("--host="):]
		case a == "--diff":
			if i+2 >= len(argv) {
				return nil, fmt.Errorf("--diff requires two arguments")
			}
			c.DiffPairs = append(c.DiffPairs, [2]string{argv[i+1], argv[i+2]})
			i += 2
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

func resolveHost(flagHost string) string {
	if flagHost != "" {
		return flagHost
	}
	if v := os.Getenv("LC_ZED_REMOTE_HOST"); v != "" {
		return v
	}
	home, _ := os.UserHomeDir()
	for _, p := range []string{filepath.Join(home, ".config", "zed-remote.conf"), "/etc/zed-remote.conf"} {
		if v := readHostFromConf(p); v != "" {
			return v
		}
	}
	return ""
}

func readHostFromConf(path string) string {
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
		if strings.TrimSpace(k) == "host" {
			return strings.Trim(strings.TrimSpace(v), `"'`)
		}
	}
	return ""
}

func socketPath() (string, error) {
	u, err := user.Current()
	if err != nil {
		return "", err
	}
	return filepath.Join("/tmp", "zed-"+u.Username+".sock"), nil
}

func die(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "zed-remote: "+format+"\n", args...)
	os.Exit(1)
}

func main() {
	args, err := parseArgs(os.Args[1:])
	if err != nil {
		die("%v", err)
	}

	host := resolveHost(args.Host)
	if host == "" {
		die("not inside a configured ssh session (set LC_ZED_REMOTE_HOST, use -H <alias>, or write host=ALIAS to ~/.config/zed-remote.conf)")
	}

	cwd, err := os.Getwd()
	if err != nil {
		die("getwd: %v", err)
	}
	home, _ := os.UserHomeDir()

	req := &protocol.Request{
		V:        protocol.Version,
		Host:     host,
		Cwd:      cwd,
		Wait:     args.Wait,
		Add:      args.Add,
		New:      args.New,
		Existing: args.Existing,
	}
	for _, p := range args.Paths {
		ps, err := paths.ParsePathSpec(p, cwd, home)
		if err != nil {
			die("resolve %q: %v", p, err)
		}
		req.Paths = append(req.Paths, ps)
	}
	for _, d := range args.DiffPairs {
		a, err := paths.ParsePathSpec(d[0], cwd, home)
		if err != nil {
			die("resolve --diff %q: %v", d[0], err)
		}
		b, err := paths.ParsePathSpec(d[1], cwd, home)
		if err != nil {
			die("resolve --diff %q: %v", d[1], err)
		}
		req.Diffs = append(req.Diffs, protocol.DiffPair{A: a.Path, B: b.Path})
	}

	sock, err := socketPath()
	if err != nil {
		die("%v", err)
	}

	conn, err := net.Dial("unix", sock)
	if err != nil {
		die("%s not present; RemoteForward failed (is the local listener running? is sshd AcceptEnv LC_*?): %v", sock, err)
	}
	defer conn.Close()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	go func() {
		<-sigCh
		conn.Close()
	}()

	if err := protocol.EncodeRequest(conn, req); err != nil {
		die("send request: %v", err)
	}

	br := bufio.NewReader(conn)
	exitCode := 0
	for {
		f, err := protocol.DecodeFrame(br)
		if err != nil {
			if errors.Is(err, io.EOF) {
				fmt.Fprintln(os.Stderr, "zed-remote: listener closed connection")
				os.Exit(1)
			}
			die("read frame: %v", err)
		}
		switch f.T {
		case protocol.FrameOut:
			os.Stdout.Write(f.D)
		case protocol.FrameErr:
			os.Stderr.Write(f.D)
		case protocol.FrameError:
			fmt.Fprintf(os.Stderr, "zed-remote: listener error: %s\n", f.Msg)
			exitCode = 1
		case protocol.FrameExit:
			exitCode = f.Code
			os.Exit(exitCode)
		}
	}
}
