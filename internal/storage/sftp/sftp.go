// Package sftp provides an SFTP-backed image storage provider.
// Images are stored as <basePath>/<id>.jpg on the remote server.
package sftp

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"path"
	"strings"
	"time"

	"github.com/DMarby/picsum-photos/internal/storage"
	imageformat "github.com/DMarby/picsum-photos/internal/storage/format"
	pkgsftp "github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

// Provider implements storage.Provider over SFTP.
type Provider struct {
	client   *pkgsftp.Client
	basePath string
}

// Config holds SFTP connection parameters.
type Config struct {
	Host     string // e.g. "sftp.example.com"
	Port     string // e.g. "22"
	User     string
	Password string // leave empty if using PrivateKey
	BasePath string // remote directory, e.g. "/images"
}

// New dials the SFTP server and returns a ready Provider.
func New(cfg Config) (*Provider, error) {
	if cfg.Port == "" {
		cfg.Port = "22"
	}

	authMethods := []ssh.AuthMethod{}
	if cfg.Password != "" {
		authMethods = append(authMethods, ssh.Password(cfg.Password))
	}
	if len(authMethods) == 0 {
		return nil, fmt.Errorf("sftp: no auth method provided (set PICSUM_SFTP_PASSWORD)")
	}

	sshCfg := &ssh.ClientConfig{
		User:            cfg.User,
		Auth:            authMethods,
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), // acceptable for private storage servers
		Timeout:         15 * time.Second,
	}

	addr := net.JoinHostPort(cfg.Host, cfg.Port)
	conn, err := ssh.Dial("tcp", addr, sshCfg)
	if err != nil {
		return nil, fmt.Errorf("sftp: ssh dial %s: %w", addr, err)
	}

	client, err := pkgsftp.NewClient(conn)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("sftp: open sftp session: %w", err)
	}

	return &Provider{client: client, basePath: cfg.BasePath}, nil
}

// Get fetches the image bytes for the given id from the SFTP server.
// The remote files are stored as <basePath>/<id>.jpg, but we still try the
// other supported extensions as a fallback for older uploads.
func (p *Provider) Get(ctx context.Context, id string) ([]byte, error) {
	lookupID := normalizeStorageID(id)
	candidates := append([]string{".jpg"}, imageformat.SupportedExtensions...)
	seen := map[string]struct{}{}
	for _, ext := range candidates {
		if _, ok := seen[ext]; ok {
			continue
		}
		seen[ext] = struct{}{}

		remotePath := path.Join(p.basePath, lookupID+ext)
		f, err := p.client.Open(remotePath)
		if err != nil {
			if isNotFoundErr(err) {
				continue
			}
			return nil, fmt.Errorf("sftp: open %s: %w", remotePath, err)
		}
		defer f.Close()
		var buf bytes.Buffer
		if _, err := io.Copy(&buf, f); err != nil {
			return nil, fmt.Errorf("sftp: reading %s: %w", remotePath, err)
		}
		return buf.Bytes(), nil
	}
	return nil, storage.ErrNotFound
}

// Put writes image bytes to the SFTP server at <basePath>/<id>.<ext>.
// ext should be ".jpg" or ".png" — defaults to ".jpg" if empty.
func (p *Provider) Put(id string, data []byte) error {
	return p.PutWithExt(id, ".jpg", data)
}

// PutWithExt writes image bytes preserving the given extension.
func (p *Provider) PutWithExt(id, ext string, data []byte) error {
	if ext == "" {
		ext = ".jpg"
	}
	remotePath := path.Join(p.basePath, normalizeStorageID(id)+ext)

	// Ensure base directory exists
	_ = p.client.MkdirAll(p.basePath)

	f, err := p.client.Create(remotePath)
	if err != nil {
		return fmt.Errorf("sftp: create %s: %w", remotePath, err)
	}
	defer f.Close()

	if _, err := f.Write(data); err != nil {
		return fmt.Errorf("sftp: write %s: %w", remotePath, err)
	}
	return nil
}

// Delete removes a file from the SFTP server (tries .jpg and .png).
func (p *Provider) Delete(id string) error {
	lookupID := normalizeStorageID(id)
	for _, ext := range imageformat.SupportedExtensions {
		p.client.Remove(path.Join(p.basePath, lookupID+ext))
	}
	return nil
}

// Close shuts down the SFTP session.
func (p *Provider) Close() {
	p.client.Close()
}

func isNotFoundErr(err error) bool {
	if err == nil {
		return false
	}
	lower := strings.ToLower(err.Error())
	return strings.Contains(lower, "no such file") || strings.Contains(lower, "not exist") || strings.Contains(lower, "does not exist")
}

func normalizeStorageID(id string) string {
	id = strings.TrimSpace(id)
	id = path.Base(id)
	lower := strings.ToLower(id)
	for _, ext := range imageformat.SupportedExtensions {
		if strings.HasSuffix(lower, ext) {
			return id[:len(id)-len(ext)]
		}
	}
	return id
}
