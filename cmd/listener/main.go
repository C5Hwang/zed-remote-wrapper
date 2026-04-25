package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"zed-remote-wrapper/internal/protocol"
)

const (
	ansiReset  = "\x1b[0m"
	ansiCyan   = "\x1b[36m"
	ansiYellow = "\x1b[33m"
	ansiRed    = "\x1b[31m"
)

const jsonBorder = "────────────────────────────────────────"

var useColor = isTTY(os.Stderr)

func isTTY(f *os.File) bool {
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return (fi.Mode() & os.ModeCharDevice) != 0
}

func levelTag(level, color string) string {
	if !useColor {
		return level
	}
	return color + level + ansiReset
}

func logf(level, color, format string, args ...any) {
	ts := time.Now().Format("15:04:05.000")
	msg := fmt.Sprintf(format, args...)
	fmt.Fprintf(os.Stderr, "%s  %s  zed-remote-listener  %s\n", ts, levelTag(level, color), msg)
}

func logInfof(format string, args ...any)  { logf("INFO ", ansiCyan, format, args...) }
func logWarnf(format string, args ...any)  { logf("WARN ", ansiYellow, format, args...) }
func logErrorf(format string, args ...any) { logf("ERROR", ansiRed, format, args...) }
func logFatalf(format string, args ...any) { logErrorf(format, args...); os.Exit(1) }

func logRequestDump(req *protocol.Request) {
	logInfof("received request")
	pretty, err := json.MarshalIndent(req, "", "  ")
	if err != nil {
		logErrorf("marshal request for log -- %v", err)
		return
	}
	fmt.Fprintln(os.Stderr, jsonBorder)
	os.Stderr.Write(pretty)
	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, jsonBorder)
}

var (
	socketFlag  = flag.String("socket", "", "Unix socket path to listen on (default: $HOME/.zed-remote.sock)")
	zedFlag     = flag.String("zed", "", "Path to the local zed CLI (default: first `zed` on $PATH)")
	verboseFlag = flag.Bool("v", false, "Verbose logging")
)

func main() {
	flag.Parse()

	sockPath := *socketFlag
	if sockPath == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			logFatalf("user home dir -- %v", err)
		}
		sockPath = filepath.Join(home, ".zed-remote.sock")
	}

	zedPath := *zedFlag
	if zedPath == "" {
		p, err := exec.LookPath("zed")
		if err != nil {
			logFatalf("zed binary not found on $PATH -- %v", err)
		}
		zedPath = p
	} else if _, err := os.Stat(zedPath); err != nil {
		logFatalf("zed binary not found at %s -- %v", zedPath, err)
	}
	// Overwrite flag value so handleConn reads the resolved path.
	*zedFlag = zedPath

	_ = os.Remove(sockPath)
	l, err := net.Listen("unix", sockPath)
	if err != nil {
		logFatalf("listen %s -- %v", sockPath, err)
	}
	if err := os.Chmod(sockPath, 0600); err != nil {
		logFatalf("chmod %s -- %v", sockPath, err)
	}
	logInfof("listening on %s zed=%s", sockPath, zedPath)

	closing := make(chan struct{})
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		s := <-sigCh
		logInfof("received %s, shutting down", s)
		close(closing)
		l.Close()
		_ = os.Remove(sockPath)
		os.Exit(0)
	}()

	for {
		conn, err := l.Accept()
		if err != nil {
			select {
			case <-closing:
				return
			default:
			}
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				continue
			}
			logErrorf("accept failed -- %v", err)
			return
		}
		go handleConn(conn)
	}
}

func handleConn(c net.Conn) {
	defer c.Close()
	fw := protocol.NewFrameWriter(c)

	_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
	br := bufio.NewReader(c)
	req, err := protocol.DecodeRequest(br)
	if err != nil {
		_ = fw.Write(protocol.Frame{T: protocol.FrameError, Msg: fmt.Sprintf("parse request -- %v", err)})
		return
	}
	_ = c.SetReadDeadline(time.Time{})

	if *verboseFlag {
		logRequestDump(req)
	}

	args, err := buildZedArgs(req)
	if err != nil {
		_ = fw.Write(protocol.Frame{T: protocol.FrameError, Msg: err.Error()})
		return
	}

	cmd := exec.Command(*zedFlag, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = fw.Write(protocol.Frame{T: protocol.FrameError, Msg: fmt.Sprintf("stdout pipe -- %v", err)})
		return
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		_ = fw.Write(protocol.Frame{T: protocol.FrameError, Msg: fmt.Sprintf("stderr pipe -- %v", err)})
		return
	}

	if *verboseFlag {
		logInfof("executing command: %s", strconv.Quote(strings.Join(cmd.Args, " ")))
	}

	if err := cmd.Start(); err != nil {
		_ = fw.Write(protocol.Frame{T: protocol.FrameError, Msg: fmt.Sprintf("exec zed -- %v", err)})
		return
	}
	pgid := cmd.Process.Pid

	go pump(fw, protocol.FrameOut, stdout)
	go pump(fw, protocol.FrameErr, stderr)

	// Detect client disconnect → kill process group.
	disconnected := make(chan struct{})
	go func() {
		buf := make([]byte, 64)
		for {
			if _, err := c.Read(buf); err != nil {
				close(disconnected)
				return
			}
		}
	}()

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	select {
	case err := <-done:
		code := exitCode(err)
		_ = fw.Write(protocol.Frame{T: protocol.FrameExit, Code: code})
	case <-disconnected:
		logWarnf("client disconnected, terminating pid=%d", pgid)
		_ = syscall.Kill(-pgid, syscall.SIGTERM)
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			_ = syscall.Kill(-pgid, syscall.SIGKILL)
			<-done
		}
	}
}

func pump(fw *protocol.FrameWriter, t string, r io.Reader) {
	buf := make([]byte, 4096)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			data := make([]byte, n)
			copy(data, buf[:n])
			_ = fw.Write(protocol.Frame{T: t, D: data})
		}
		if err != nil {
			return
		}
	}
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	if ee, ok := err.(*exec.ExitError); ok {
		return ee.ExitCode()
	}
	return 1
}

// buildZedArgs converts a Request into Zed CLI arguments using ssh:// URLs.
func buildZedArgs(req *protocol.Request) ([]string, error) {
	if req.Host == "" {
		return nil, fmt.Errorf("empty host in request")
	}
	var args []string
	if req.Add {
		args = append(args, "--add")
	}
	if req.New {
		args = append(args, "--new")
	}
	if req.Existing {
		args = append(args, "--existing")
	}
	for _, p := range req.Paths {
		args = append(args, sshURL(req.Host, req.User, req.Port, p.Path, p.Line, p.Col))
	}
	return args, nil
}

func sshURL(host, user string, port int, path string, line, col int) string {
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	h := host
	if port != 22 {
		h = net.JoinHostPort(host, strconv.Itoa(port))
	}
	u := &url.URL{Scheme: "ssh", Host: h, Path: path}
	if user != "" {
		u.User = url.User(user)
	}
	out := u.String()
	if line > 0 {
		out += fmt.Sprintf(":%d", line)
		if col > 0 {
			out += fmt.Sprintf(":%d", col)
		}
	}
	return out
}
