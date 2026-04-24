package protocol

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
)

const Version = 1

type PathSpec struct {
	Path string `json:"path"`
	Line int    `json:"line,omitempty"`
	Col  int    `json:"col,omitempty"`
}

type DiffPair struct {
	A string `json:"a"`
	B string `json:"b"`
}

type Request struct {
	V        int        `json:"v"`
	Host     string     `json:"host"`
	User     string     `json:"user,omitempty"`
	Port     int        `json:"port,omitempty"`
	Cwd      string     `json:"cwd"`
	Paths    []PathSpec `json:"paths"`
	Wait     bool       `json:"wait,omitempty"`
	Add      bool       `json:"add,omitempty"`
	New      bool       `json:"new,omitempty"`
	Existing bool       `json:"existing,omitempty"`
	Diffs    []DiffPair `json:"diffs,omitempty"`
}

type Frame struct {
	T    string `json:"t"`
	D    []byte `json:"d,omitempty"`
	Code int    `json:"code,omitempty"`
	Msg  string `json:"msg,omitempty"`
}

const (
	FrameOut   = "out"
	FrameErr   = "err"
	FrameExit  = "exit"
	FrameError = "error"
)

func EncodeRequest(w io.Writer, r *Request) error {
	if r.V == 0 {
		r.V = Version
	}
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	_, err = w.Write(b)
	return err
}

func DecodeRequest(br *bufio.Reader) (*Request, error) {
	line, err := br.ReadBytes('\n')
	if err != nil {
		return nil, err
	}
	var r Request
	if err := json.Unmarshal(line, &r); err != nil {
		return nil, fmt.Errorf("decode request: %w", err)
	}
	if r.V != Version {
		return nil, fmt.Errorf("unsupported protocol version %d (want %d)", r.V, Version)
	}
	return &r, nil
}

type FrameWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func NewFrameWriter(w io.Writer) *FrameWriter { return &FrameWriter{w: w} }

func (fw *FrameWriter) Write(f Frame) error {
	fw.mu.Lock()
	defer fw.mu.Unlock()
	b, err := json.Marshal(f)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	_, err = fw.w.Write(b)
	return err
}

func DecodeFrame(br *bufio.Reader) (*Frame, error) {
	line, err := br.ReadBytes('\n')
	if err != nil {
		if errors.Is(err, io.EOF) && len(line) == 0 {
			return nil, io.EOF
		}
		if len(line) == 0 {
			return nil, err
		}
	}
	var f Frame
	if err := json.Unmarshal(line, &f); err != nil {
		return nil, fmt.Errorf("decode frame: %w", err)
	}
	return &f, nil
}
