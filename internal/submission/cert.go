package submission

import (
	"crypto/tls"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"
)

// LoadTLS returns a TLS configuration that serves the certificate in
// certFile and keyFile. The files are checked on each handshake and
// reloaded when they change, so a renewed certificate (Let's Encrypt, for
// example) is used without a restart. If a reload fails, for instance
// because only one of the two files has been replaced so far, the previous
// certificate stays in use.
func LoadTLS(certFile, keyFile string, log *slog.Logger) (*tls.Config, error) {
	c := &certFiles{certFile: certFile, keyFile: keyFile, log: log}
	if err := c.load(); err != nil {
		return nil, err
	}
	return &tls.Config{MinVersion: tls.VersionTLS12, GetCertificate: c.get}, nil
}

type stamp struct {
	mod  time.Time
	size int64
}

type certFiles struct {
	certFile, keyFile string
	log               *slog.Logger

	mu     sync.Mutex
	cert   *tls.Certificate
	stamps [2]stamp
}

func (c *certFiles) current() ([2]stamp, error) {
	var out [2]stamp
	for i, name := range []string{c.certFile, c.keyFile} {
		fi, err := os.Stat(name)
		if err != nil {
			return out, err
		}
		out[i] = stamp{fi.ModTime(), fi.Size()}
	}
	return out, nil
}

func (c *certFiles) load() error {
	st, err := c.current()
	if err != nil {
		return fmt.Errorf("TLS certificate: %w", err)
	}
	cert, err := tls.LoadX509KeyPair(c.certFile, c.keyFile)
	if err != nil {
		return fmt.Errorf("TLS certificate: %w", err)
	}
	c.cert, c.stamps = &cert, st
	return nil
}

func (c *certFiles) get(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if st, err := c.current(); err == nil && st != c.stamps {
		if err := c.load(); err != nil {
			if c.log != nil {
				c.log.Error("submission: cannot reload the TLS certificate; still serving the previous one", "err", err)
			}
		} else if c.log != nil {
			c.log.Info("submission: reloaded the TLS certificate")
		}
	}
	return c.cert, nil
}
