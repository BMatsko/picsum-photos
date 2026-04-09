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
	"time"

	"github.com/DMarby/picsum-photos/internal/storage"
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
func (p *Provider) Get(ctx context.Context, id string) ([]byte, error) {
	remotePath := path.Join(p.basePath, id+".jpg")

	f, err := p.client.Open(remotePath)
	if err != nil {
		// pkg/sftp surfaces "does not exist" as os.ErrNotExist via errors.Is
		return nil, storage.ErrNotFound
	}
	defer f.Close()

	var buf bytes.Buffer
	if _, err := io.Copy(&buf, f); err != nil {
		return nil, fmt.Errorf("sftp: reading %s: %w", remotePath, err)
	}
	return buf.Bytes(), nil
}

// Put writes image bytes to the SFTP server at <basePath>/<id>.jpg.
// Used by the admin upload handler.
func (p *Provider) Put(id string, data []byte) error {
	remotePath := path.Join(p.basePath, id+".jpg")

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

// Delete removes a file from the SFTP server.
func (p *Provider) Delete(id string) error {
	return p.client.Remove(path.Join(p.basePath, id+".jpg"))
}

// Close shuts down the SFTP session.
func (p *Provider) Close() {
	p.client.Close()
}
