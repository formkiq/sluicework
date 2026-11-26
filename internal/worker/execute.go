package worker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"sluicework/internal/state"
)

const maxFileInputs = 256 << 10

var placeholder = regexp.MustCompile(`\{\{([A-Za-z_][A-Za-z0-9_]*)\}\}`)

// Bound expansion before allocation. The same input can occur many times in
// an approved argument, and operating systems impose a total argv size limit.
func renderArgs(templates []string, values map[string]string) ([]string, error) {
	const capBytes = 128 << 10
	args := make([]string, 0, len(templates))
	remaining := capBytes
	for _, t := range templates {
		var arg strings.Builder
		prev := 0
		for _, m := range placeholder.FindAllStringSubmatchIndex(t, -1) {
			value := values[t[m[2]:m[3]]]
			if arg.Len()+m[0]-prev+len(value) > remaining {
				return nil, errors.New("rendered arguments exceed 128 KiB")
			}
			arg.WriteString(t[prev:m[0]])
			arg.WriteString(value)
			prev = m[1]
		}
		if arg.Len()+len(t)-prev+1 > remaining {
			return nil, errors.New("rendered arguments exceed 128 KiB")
		}
		arg.WriteString(t[prev:])
		remaining -= arg.Len() + 1
		args = append(args, arg.String())
	}
	return args, nil
}

// readInput opens a regular file beneath the configured input directory. OpenRoot
// applies the directory boundary throughout path resolution, including symlinks.
func (c *client) readInput(path string, remaining int) ([]byte, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, errors.New("file inputs require a clean absolute path")
	}
	rootPath := c.config.InputRoot
	if rootPath == "" {
		rootPath = "/"
	}
	relative, err := filepath.Rel(rootPath, path)
	if err != nil || !filepath.IsLocal(relative) {
		return nil, errors.New("selected input is outside the input directory")
	}
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	f, err := root.OpenFile(relative, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() {
		return nil, errors.New("selected input is not a regular file")
	}
	if secret, err := os.Stat(c.config.CredentialFile); err == nil && os.SameFile(st, secret) {
		return nil, errors.New("worker credential is a protected file")
	}
	data, err := io.ReadAll(io.LimitReader(f, int64(remaining)+1))
	if err != nil {
		return nil, err
	}
	if len(data) > remaining {
		return nil, errors.New("selected files exceed the input size limit")
	}
	return data, nil
}

func (c *client) inputs(claim state.Claim, scratch string) (map[string]string, map[string]string, error) {
	values := make(map[string]string, len(claim.Job.Inputs))
	files := make(map[string]string)
	remaining := maxFileInputs
	for _, input := range claim.Job.Inputs {
		value, exists := claim.Inputs[input.Name]
		if !exists && input.Required {
			return nil, nil, fmt.Errorf("input %q is required", input.Name)
		}
		values[input.Name] = value
		if len(input.Paths) == 0 || value == "" {
			continue
		}
		if !slices.Contains(input.Paths, value) {
			return nil, nil, errors.New("selected file was not approved for this input")
		}
		data, err := c.readInput(value, remaining)
		if err != nil {
			return nil, nil, err
		}
		remaining -= len(data)
		if claim.Job.Type == "ai" {
			if !utf8.Valid(data) {
				return nil, nil, errors.New("AI file inputs must contain UTF-8 text")
			}
			files[input.Name] = string(data)
		} else {
			f, err := os.CreateTemp(scratch, "input-*")
			if err != nil {
				return nil, nil, err
			}
			_, err = f.Write(data)
			if err == nil {
				err = f.Chmod(0400)
			}
			closeErr := f.Close()
			if err != nil {
				return nil, nil, err
			}
			if closeErr != nil {
				return nil, nil, closeErr
			}
			values[input.Name] = f.Name()
		}
	}
	return values, files, nil
}

func failed(err error) state.Completion {
	return state.Completion{State: "failed", Error: err.Error()}
}

func (c *client) execute(ctx context.Context, claim state.Claim) state.Completion {
	if (claim.Job.OS != "" && claim.Job.OS != runtime.GOOS) || (claim.Job.Arch != "" && claim.Job.Arch != runtime.GOARCH) {
		return failed(errors.New("job requires a different operating system or CPU architecture"))
	}
	scratch, err := os.MkdirTemp(c.config.WorkDir, "attempt-*")
	if err != nil {
		return failed(err)
	}
	defer os.RemoveAll(scratch)
	values, files, err := c.inputs(claim, scratch)
	if err != nil {
		return failed(err)
	}
	if ctx.Err() != nil {
		return failed(ctx.Err())
	}
	switch claim.Job.Type {
	case "ai":
		var result struct {
			Text      string `json:"text"`
			Truncated bool   `json:"truncated"`
		}
		_, err := c.post(ctx, attemptRoute(claim.AttemptID, "infer"), map[string]any{"inputs": files}, &result)
		if err != nil {
			return failed(fmt.Errorf("inference request failed: %w", err))
		}
		return state.Completion{State: "succeeded", Output: result.Text, OutputTruncated: result.Truncated}
	case "script":
		return c.script(ctx, claim, scratch, values)
	default:
		return failed(errors.New("unsupported job type"))
	}
}

type boundedOutput struct {
	bytes.Buffer
	limit     int
	truncated bool
}

func (b *boundedOutput) Write(p []byte) (int, error) {
	n := min(len(p), max(0, b.limit-b.Len()))
	b.Buffer.Write(p[:n])
	b.truncated = b.truncated || n < len(p)
	return len(p), nil
}

type logWriter struct {
	ctx      context.Context
	c        *client
	attempt  string
	mu       sync.Mutex
	sequence int64
}

func (w *logWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	total := len(p)
	for len(p) > 0 {
		n := min(len(p), 16<<10)
		// A JSON log event must contain complete UTF-8 characters.
		if n < len(p) {
			for n > 0 && !utf8.RuneStart(p[n]) {
				n--
			}
			if n == 0 {
				n = min(len(p), 16<<10)
			}
		}
		chunk := p[:n]
		_, err := w.c.short(w.ctx, attemptRoute(w.attempt, "logs"), map[string]any{"text": strings.ToValidUTF8(string(chunk), "�"), "sequence": w.sequence + 1}, nil)
		if err != nil {
			if permanent(err) || w.ctx.Err() != nil {
				return total - len(p), err
			}
			if !pause(w.ctx, 250*time.Millisecond) {
				return total - len(p), w.ctx.Err()
			}
			continue
		}
		w.sequence++
		p = p[len(chunk):]
	}
	return total, nil
}

// Each OS pipe can split a multibyte rune across reads. Keep at most three
// trailing bytes per stream until its next write.
type logStream struct {
	writer *logWriter
	tail   []byte
}

func (s *logStream) Write(data []byte) (int, error) {
	size := len(data)
	if len(s.tail) > 0 {
		data = append(s.tail, data...)
		s.tail = nil
	}
	for i := len(data) - 1; i >= max(0, len(data)-utf8.UTFMax); i-- {
		if !utf8.RuneStart(data[i]) {
			continue
		}
		if !utf8.FullRune(data[i:]) {
			s.tail = append([]byte(nil), data[i:]...)
			data = data[:i]
		}
		break
	}
	_, err := s.writer.Write(data)
	if err != nil {
		return 0, err
	}
	return size, nil
}

func (s *logStream) finish() error {
	if len(s.tail) == 0 {
		return nil
	}
	_, err := s.writer.Write(s.tail)
	return err
}

func (c *client) script(ctx context.Context, claim state.Claim, scratch string, values map[string]string) state.Completion {
	job := claim.Job
	if !filepath.IsAbs(job.Command) {
		return failed(errors.New("approved command must be an absolute executable path"))
	}
	if job.WorkDir != "" && !filepath.IsLocal(job.WorkDir) {
		return failed(errors.New("working directory must be inside the attempt directory"))
	}
	dir := filepath.Join(scratch, job.WorkDir)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return failed(err)
	}
	args, err := renderArgs(job.Args, values)
	if err != nil {
		return failed(err)
	}
	cmd, err := c.scriptCommand(ctx, job.Command, scratch, args)
	if err != nil {
		return failed(err)
	}
	cmd.Dir = dir
	cmd.Env = []string{"HOME=" + scratch, "TMPDIR=" + scratch, "PATH=/usr/bin:/bin:/usr/sbin:/sbin:/opt/homebrew/bin:/usr/local/bin", "LANG=C"}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	cmd.WaitDelay = time.Second
	out := boundedOutput{limit: job.OutputLimit}
	var stdout, stderr logStream
	if job.DiscardOutput {
		cmd.Stdout = io.Discard
		cmd.Stderr = io.Discard
	} else {
		logs := &logWriter{ctx: ctx, c: c, attempt: claim.AttemptID}
		stdout.writer = logs
		stderr.writer = logs
		cmd.Stdout = io.MultiWriter(&out, &stdout)
		cmd.Stderr = &stderr
	}
	if err := cmd.Start(); err != nil {
		return failed(err)
	}
	err = cmd.Wait()
	// An approved command can fork helpers that outlive its main process.
	// Relinquish its whole process group before confirming completion.
	syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	if err == nil {
		err = errors.Join(stdout.finish(), stderr.finish())
	}
	result := state.Completion{State: "succeeded", Output: strings.ToValidUTF8(out.String(), "�"), OutputTruncated: out.truncated}
	if err != nil {
		result.State = "failed"
		result.Error = err.Error()
	}
	return result
}
