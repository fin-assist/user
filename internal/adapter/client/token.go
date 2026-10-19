package client

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
)

// FileToken reads the token from a file and rereads it when the file changes: kubelet
// rotates the projected ServiceAccount token at ~80 % of its hour (grpc-api.md 3.3); on the
// stand the file comes from the token source of FIN-119.
type FileToken struct {
	Path string

	mu    sync.Mutex
	mtime time.Time
	size  int64
	token string
}

// Token returns the current token.
func (f *FileToken) Token(context.Context) (string, error) {
	st, err := os.Stat(f.Path)
	if err != nil {
		return "", err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.token != "" && st.ModTime().Equal(f.mtime) && st.Size() == f.size {
		return f.token, nil
	}
	b, err := os.ReadFile(f.Path)
	if err != nil {
		return "", err
	}
	t := strings.TrimSpace(string(b))
	if t == "" {
		return "", fmt.Errorf("%s is empty", f.Path)
	}
	f.token, f.mtime, f.size = t, st.ModTime(), st.Size()
	return t, nil
}

// DevToken presents "dev:<service>", which the dev verifiers accept. Only for
// AUTH_MODE=insecure-dev.
type DevToken string

// Token returns the dev token.
func (d DevToken) Token(context.Context) (string, error) {
	if d == "" {
		return "", errors.New("empty dev token")
	}
	return "dev:" + string(d), nil
}
