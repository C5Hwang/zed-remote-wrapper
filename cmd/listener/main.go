package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"zed-remote-wrapper/internal/protocol"
)

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
			log.Fatalf("user home dir: %v", err)
		}
		sockPath = filepath.Join(home, ".zed-remote.sock")
	}

	zedPath := *zedFlag
	if zedPath == "" {
		p, err := exec.LookPath("zed")
		if err != nil {
			log.Fatalf("zed binary not found on $PATH; install Zed CLI or pass --zed <path>: %v", err)
		}
		zedPath = p
	} else if _, err := os.Stat(zedPath); err != nil {
		log.Fatalf("zed binary not found at %s: %v", zedPath, err)
	}
	// Overwrite flag value so handleConn reads the resolved path.
	*zedFlag = zedPath

	_ = os.Remove(sockPath)
	l, err := net.Listen("unix", sockPath)
	if err != nil {
		log.Fatalf("listen %s: %v", sockPath, err)
	}
	if err := os.Chmod(sockPath, 0600); err != nil {
		log.Fatalf("chmod %s: %v", sockPath, err)
	}
	log.Printf("listening on %s (zed=%s)", sockPath, zedPath)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		s := <-sigCh
		log.Printf("received %s, shutting down", s)
		l.Close()
		_ = os.Remove(sockPath)
		os.Exit(0)
	}()

	for {
		conn, err := l.Accept()
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				continue
			}
			log.Printf("accept: %v", err)
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
		_ = fw.Write(protocol.Frame{T: protocol.FrameError, Msg: fmt.Sprintf("parse request: %v", err)})
		return
	}
	_ = c.SetReadDeadline(time.Time{})

	if *verboseFlag {
		log.Printf("request: host=%s paths=%d wait=%v add=%v new=%v existing=%v diffs=%d",
			req.Host, len(req.Paths), req.Wait, req.Add, req.New, req.Existing, len(req.Diffs))
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
		_ = fw.Write(protocol.Frame{T: protocol.FrameError, Msg: fmt.Sprintf("stdout pipe: %v", err)})
		return
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		_ = fw.Write(protocol.Frame{T: protocol.FrameError, Msg: fmt.Sprintf("stderr pipe: %v", err)})
		return
	}

	if err := cmd.Start(); err != nil {
		_ = fw.Write(protocol.Frame{T: protocol.FrameError, Msg: fmt.Sprintf("exec zed: %v", err)})
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
		log.Printf("client disconnected, terminating pid=%d", pgid)
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
	if req.Wait {
		args = append(args, "--wait")
	}
	if req.Add {
		args = append(args, "--add")
	}
	if req.New {
		args = append(args, "--new")
	}
	if req.Existing {
		args = append(args, "--existing")
	}
	for _, d := range req.Diffs {
		args = append(args, "--diff", sshURL(req.Host, d.A, 0, 0), sshURL(req.Host, d.B, 0, 0))
	}
	for _, p := range req.Paths {
		args = append(args, sshURL(req.Host, p.Path, p.Line, p.Col))
	}
	return args, nil
}

func sshURL(host, path string, line, col int) string {
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	u := &url.URL{Scheme: "ssh", Host: host, Path: path}
	out := u.String()
	if line > 0 {
		out += fmt.Sprintf(":%d", line)
		if col > 0 {
			out += fmt.Sprintf(":%d", col)
		}
	}
	return out
}
