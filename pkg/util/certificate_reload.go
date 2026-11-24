/*
Copyright 2026 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package util //nolint:revive

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/fsnotify/fsnotify"
	"google.golang.org/grpc/credentials"
	"k8s.io/apimachinery/pkg/util/sets"
	certutil "k8s.io/client-go/util/cert"
	"k8s.io/klog/v2"
)

const (
	// The watcher is closed and created again every tlsReloadInterval. Tearing
	// down fsnotify watches has open bugs on kqueue and Windows, so only Linux
	// watches and the other platforms poll.
	// https://github.com/fsnotify/fsnotify/issues/788 and https://github.com/fsnotify/fsnotify/issues/768.
	watchTLSFiles = runtime.GOOS == "linux"

	// tlsReloadInterval is the poll period, the lifetime of the file watches,
	// the watch retry backoff, and the cap on the reload retry backoff.
	tlsReloadInterval = time.Minute
	// tlsReloadRetryDelay is the initial backoff after a failed reload.
	tlsReloadRetryDelay = 5 * time.Millisecond
)

// TLSReloadHooks are optional callbacks invoked after every TLS reload
// attempt, for example to record metrics. Either field may be nil.
type TLSReloadHooks struct {
	// OnSuccess is called after every successful reload attempt, including
	// attempts that only confirmed the files are unchanged.
	OnSuccess func()
	// OnFailure is called after every failed reload attempt. A read that
	// overlaps a rotation is retried once before it counts as a failure.
	OnFailure func()
}

// GetReloadingServerTLSConfig validates the configured files and returns a
// server TLS config whose identity and trust material are refreshed
// asynchronously. Full handshakes present the latest serving certificate.
func GetReloadingServerTLSConfig(ctx context.Context, caFile, certFile, keyFile string, nextProtos []string, cipherSuites []uint16, minVersion uint16, hooks TLSReloadHooks) (*tls.Config, error) {
	reloader, err := newServerTLSConfigReloader(caFile, certFile, keyFile, nextProtos, cipherSuites, minVersion, hooks)
	if err != nil {
		return nil, err
	}
	reloader.material.start(ctx)
	return reloader.tlsConfig(), nil
}

type serverTLSConfigReloader struct {
	material     *tlsMaterialReloader
	nextProtos   []string
	cipherSuites []uint16
	minVersion   uint16
}

func newServerTLSConfigReloader(caFile, certFile, keyFile string, nextProtos []string, cipherSuites []uint16, minVersion uint16, hooks TLSReloadHooks) (*serverTLSConfigReloader, error) {
	if certFile == "" || keyFile == "" {
		return nil, fmt.Errorf("server certificate and key files must be configured")
	}
	material, err := newTLSMaterialReloader(caFile, certFile, keyFile, hooks)
	if err != nil {
		return nil, err
	}
	return &serverTLSConfigReloader{
		material:     material,
		nextProtos:   append([]string(nil), nextProtos...),
		cipherSuites: append([]uint16(nil), cipherSuites...),
		minVersion:   minVersion,
	}, nil
}

func (r *serverTLSConfigReloader) currentTLSConfig() *tls.Config {
	material := r.material.current.Load()

	// crypto/tls does not modify the returned config, so it can share the
	// reloader's copies of nextProtos and cipherSuites. crypto/tls takes the
	// session ticket keys from the listener's config, so sessions still resume
	// after a reload.
	config := &tls.Config{ // #nosec G402 -- the caller supplies the minimum version.
		Certificates: []tls.Certificate{*material.certificate},
		NextProtos:   r.nextProtos,
		MinVersion:   r.minVersion,
		CipherSuites: r.cipherSuites,
	}
	if r.material.caFile != "" {
		config.ClientAuth = tls.RequireAndVerifyClientCert
		config.ClientCAs = material.caPool
	}
	return config
}

func (r *serverTLSConfigReloader) tlsConfig() *tls.Config {
	config := r.currentTLSConfig().Clone()
	config.GetConfigForClient = func(*tls.ClientHelloInfo) (*tls.Config, error) {
		return r.currentTLSConfig(), nil
	}
	return config
}

// tlsMaterial is one published generation of validated TLS material. It also
// carries the PEM it was parsed from, so that a reload can detect that the
// files are unchanged without parsing them again.
type tlsMaterial struct {
	certificatePEM []byte
	privateKeyPEM  []byte
	caPEM          []byte
	certificate    *tls.Certificate
	caPool         *x509.CertPool
}

// tlsMaterialReloader reloads the configured TLS files as one unit: a new
// generation is published only after every file has been read, parsed and
// validated, and any failure keeps the previous generation in place until a
// retry succeeds.
type tlsMaterialReloader struct {
	caFile   string
	certFile string
	keyFile  string

	// reloadInterval is tlsReloadInterval, except in tests.
	reloadInterval time.Duration

	hooks TLSReloadHooks

	mu      sync.Mutex
	current atomic.Pointer[tlsMaterial]
	// reloadCh schedules a reload; its capacity of one absorbs bursts of
	// notifications into a single pending reload.
	reloadCh chan struct{}
}

func newTLSMaterialReloader(caFile, certFile, keyFile string, hooks TLSReloadHooks) (*tlsMaterialReloader, error) {
	if (certFile == "") != (keyFile == "") {
		return nil, fmt.Errorf("certificate and key files must be configured together")
	}
	if certFile == "" && caFile == "" {
		return nil, fmt.Errorf("at least one TLS identity or CA file must be configured")
	}

	reloader := &tlsMaterialReloader{
		caFile:         caFile,
		certFile:       certFile,
		keyFile:        keyFile,
		reloadInterval: tlsReloadInterval,
		hooks:          hooks,
		reloadCh:       make(chan struct{}, 1),
	}
	err := reloader.load()
	if errors.Is(err, errTLSFileChanged) {
		// A projected Secret may rotate during the initial read.
		err = reloader.reload()
	}
	if err != nil {
		return nil, err
	}
	if reloader.hooks.OnSuccess != nil {
		reloader.hooks.OnSuccess()
	}
	return reloader, nil
}

// start begins refreshing the material until ctx is canceled.
func (r *tlsMaterialReloader) start(ctx context.Context) {
	if watchTLSFiles {
		go r.watch(ctx)
	} else {
		klog.InfoS("Polling TLS files without a file watcher", "os", runtime.GOOS)
	}
	go r.run(ctx)
}

var errTLSFileChanged = errors.New("TLS file changed while reading")

type tlsFile struct {
	path string
	info os.FileInfo
	pem  []byte
}

func (file *tlsFile) readFrom(f *os.File) error {
	var err error
	file.info, err = f.Stat()
	if err != nil {
		return err
	}
	// A pipe or device cannot be read again for a reload.
	if !file.info.Mode().IsRegular() {
		return fmt.Errorf("cannot reload non-regular TLS file %s", f.Name())
	}
	file.pem, err = io.ReadAll(f)
	return err
}

// reload publishes a new generation if any file changed. If any file fails to
// load or validate, the previously published generation stays in place.
func (r *tlsMaterialReloader) reload() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.load()
}

func (r *tlsMaterialReloader) load() error {
	cert, key, ca := tlsFile{path: r.certFile}, tlsFile{path: r.keyFile}, tlsFile{path: r.caFile}
	for _, file := range []*tlsFile{&cert, &key, &ca} {
		if file.path == "" {
			continue
		}
		// O_NONBLOCK keeps a FIFO from blocking the open. readFrom rejects it.
		f, err := os.OpenFile(file.path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
		if errors.Is(err, os.ErrNotExist) {
			// A rotation can remove the file after its symlink was resolved.
			return fmt.Errorf("%w: %w", errTLSFileChanged, err)
		}
		if err != nil {
			return err
		}
		// Keep the inode alive through verification, even if its link is removed.
		defer f.Close()
		if err := file.readFrom(f); err != nil {
			return err
		}
	}
	return r.publish(cert, key, ca)
}

func (r *tlsMaterialReloader) publish(cert, key, ca tlsFile) error {
	// A projected Secret can replace every file through one symlink swap.
	// Verify each path still names the file descriptor we read.
	for _, file := range []tlsFile{cert, key, ca} {
		if file.path == "" {
			continue
		}
		after, err := os.Stat(file.path)
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("%w: %w", errTLSFileChanged, err)
		}
		if err != nil {
			return err
		}
		if !os.SameFile(file.info, after) || file.info.Size() != after.Size() || !file.info.ModTime().Equal(after.ModTime()) {
			return fmt.Errorf("%w: %s", errTLSFileChanged, file.path)
		}
	}

	current := r.current.Load()
	if current != nil && bytes.Equal(cert.pem, current.certificatePEM) &&
		bytes.Equal(key.pem, current.privateKeyPEM) && bytes.Equal(ca.pem, current.caPEM) {
		return nil
	}

	next := &tlsMaterial{
		certificatePEM: cert.pem,
		privateKeyPEM:  key.pem,
		caPEM:          ca.pem,
	}
	if r.certFile != "" {
		// As at startup, the pair must parse and match. Validity dates and key
		// usage are left to the peer, which checks them in the handshake.
		certificate, err := tls.X509KeyPair(cert.pem, key.pem)
		if err != nil {
			return fmt.Errorf("failed to load X509 key pair %s and %s: %w", r.certFile, r.keyFile, err)
		}
		// X509KeyPair parses only the leaf certificate.
		for _, der := range certificate.Certificate[1:] {
			if _, err := x509.ParseCertificate(der); err != nil {
				return fmt.Errorf("failed to parse certificate chain %s: %w", r.certFile, err)
			}
		}
		next.certificate = &certificate
	}
	if r.caFile != "" {
		caPool, err := certutil.NewPoolFromBytes(ca.pem)
		if err != nil {
			return fmt.Errorf("failed to parse CA certificate bundle %s: %w", r.caFile, err)
		}
		next.caPool = caPool
	}
	r.current.Store(next)
	klog.V(2).InfoS("Reloaded TLS material", "certificate", r.certFile, "key", r.keyFile, "ca", r.caFile)
	return nil
}

// requestReload schedules a reload; a reload already pending absorbs it.
func (r *tlsMaterialReloader) requestReload() {
	select {
	case r.reloadCh <- struct{}{}:
	default:
	}
}

// run reloads on change notifications, on a poll interval as a fallback, and
// with exponential backoff after failures, until ctx is canceled.
func (r *tlsMaterialReloader) run(ctx context.Context) {
	ticker := time.NewTicker(r.reloadInterval)
	defer ticker.Stop()

	retryDelay := tlsReloadRetryDelay
	var retryCh <-chan time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-r.reloadCh:
		case <-retryCh:
			retryCh = nil
		}
		// A reload error written to a watched directory can trigger another
		// notification. Wait for the retry timer before reading the files again.
		if retryCh != nil {
			continue
		}

		if err := r.reload(); err != nil {
			// A read often overlaps a rotation, for example a projected Secret
			// update. The first such read after a success is retried without
			// counting a failure. If the retry fails too, it is a failure.
			overlap := errors.Is(err, errTLSFileChanged) && retryDelay == tlsReloadRetryDelay
			if !overlap {
				if r.hooks.OnFailure != nil {
					r.hooks.OnFailure()
				}
				klog.ErrorS(err, "Failed to reload TLS material; keeping the last valid material", "certificate", r.certFile, "key", r.keyFile, "ca", r.caFile)
			}
			retryCh = time.After(retryDelay)
			retryDelay = min(2*retryDelay, r.reloadInterval)
			continue
		}
		if r.hooks.OnSuccess != nil {
			r.hooks.OnSuccess()
		}
		retryDelay = tlsReloadRetryDelay
	}
}

func (r *tlsMaterialReloader) watch(ctx context.Context) {
	for {
		err := r.watchOnce(ctx)
		if ctx.Err() != nil {
			return
		}
		if err == nil {
			continue // Refresh the watch registrations.
		}
		r.requestReload()
		klog.ErrorS(err, "TLS certificate watch failed; polling remains active")

		timer := time.NewTimer(r.reloadInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

// watchOnce requests a reload for every event in the directories that hold the
// TLS files. It returns nil after reloadInterval so that watch registers the
// directories again and follows one that was replaced.
func (r *tlsMaterialReloader) watchOnce(ctx context.Context) error {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("failed to create file watcher: %w", err)
	}
	defer watcher.Close()

	directories, err := r.watchDirectories()
	if err != nil {
		return err
	}
	// fsnotify can report an error while holding the lock needed by Add.
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	go func() {
		for err := range watcher.Errors {
			if errors.Is(err, fsnotify.ErrEventOverflow) {
				r.requestReload()
				continue
			}
			cancel(err)
		}
		cancel(errors.New("file watcher error channel closed"))
	}()

	for directory := range directories {
		if err := watcher.Add(directory); err != nil {
			return fmt.Errorf("failed to watch directory %s: %w", directory, err)
		}
	}
	lifetime := time.NewTimer(r.reloadInterval)
	defer lifetime.Stop()
	// Catch changes made before registration or while replacing the watches.
	r.requestReload()

	for {
		select {
		case <-ctx.Done():
			return context.Cause(ctx)
		case <-lifetime.C:
			return nil
		case _, ok := <-watcher.Events:
			if !ok {
				return fmt.Errorf("file watcher event channel closed")
			}
			r.requestReload()
		}
	}
}

// watchDirectories returns the directories that hold the TLS files.
func (r *tlsMaterialReloader) watchDirectories() (sets.Set[string], error) {
	directories := sets.New[string]()
	for _, file := range []string{r.caFile, r.certFile, r.keyFile} {
		if file == "" {
			continue
		}
		// Resolve the parent in filesystem order. The file itself may be a
		// projected Secret symlink that is replaced during rotation.
		parent, _ := filepath.Split(file)
		if parent == "" {
			parent = "."
		}
		dir, err := filepath.EvalSymlinks(parent)
		if err != nil {
			return nil, fmt.Errorf("failed to resolve TLS watch directory %s: %w", parent, err)
		}
		directories.Insert(dir)
	}
	return directories, nil
}

// GetReloadingClientTLSCredentials validates the configured files and returns
// client transport credentials whose identity and trust material are refreshed
// asynchronously. New connections use the latest valid material.
func GetReloadingClientTLSCredentials(ctx context.Context, caFile, certFile, keyFile, serverName string, protos []string, hooks TLSReloadHooks) (credentials.TransportCredentials, error) {
	if caFile == "" {
		return nil, fmt.Errorf("client CA file must be configured")
	}
	material, err := newTLSMaterialReloader(caFile, certFile, keyFile, hooks)
	if err != nil {
		return nil, err
	}
	material.start(ctx)

	baseConfig := &tls.Config{ // #nosec G402 -- TLS 1.2 is intentionally the minimum for compatibility.
		MinVersion: tls.VersionTLS12,
		NextProtos: append([]string(nil), protos...),
	}
	// Preserve GetClientTLSConfig's authority selection for CA-only clients.
	if certFile != "" {
		baseConfig.ServerName = serverName
	}
	return newReloadingClientTLSCredentials(baseConfig, material), nil
}

func clientTLSConfig(baseConfig *tls.Config, material *tlsMaterial) *tls.Config {
	config := baseConfig.Clone()
	config.RootCAs = material.caPool
	if material.certificate != nil {
		config.Certificates = []tls.Certificate{*material.certificate}
	}
	return config
}

// reloadingClientTLSCredentials builds transport credentials from the latest
// validated TLS material for every handshake, so reconnects pick up rotated
// certificates without any redial logic.
type reloadingClientTLSCredentials struct {
	baseConfig *tls.Config
	// baseCreds answers Info() without rebuilding credentials on every call.
	baseCreds credentials.TransportCredentials
	material  *tlsMaterialReloader
}

func newReloadingClientTLSCredentials(baseConfig *tls.Config, material *tlsMaterialReloader) *reloadingClientTLSCredentials {
	return &reloadingClientTLSCredentials{
		baseConfig: baseConfig,
		baseCreds:  credentials.NewTLS(baseConfig),
		material:   material,
	}
}

func (c *reloadingClientTLSCredentials) currentCredentials() credentials.TransportCredentials {
	return credentials.NewTLS(clientTLSConfig(c.baseConfig, c.material.current.Load()))
}

func (c *reloadingClientTLSCredentials) ClientHandshake(ctx context.Context, authority string, rawConn net.Conn) (net.Conn, credentials.AuthInfo, error) {
	return c.currentCredentials().ClientHandshake(ctx, authority, rawConn)
}

func (c *reloadingClientTLSCredentials) ServerHandshake(rawConn net.Conn) (net.Conn, credentials.AuthInfo, error) {
	return c.currentCredentials().ServerHandshake(rawConn)
}

func (c *reloadingClientTLSCredentials) Info() credentials.ProtocolInfo {
	return c.baseCreds.Info()
}

func (c *reloadingClientTLSCredentials) Clone() credentials.TransportCredentials {
	return newReloadingClientTLSCredentials(c.baseConfig.Clone(), c.material)
}

// OverrideServerName implements credentials.TransportCredentials. Like the
// gRPC implementation it replaces, it is not safe for concurrent use; gRPC
// itself no longer calls this method.
func (c *reloadingClientTLSCredentials) OverrideServerName(serverName string) error {
	c.baseConfig.ServerName = serverName
	c.baseCreds = credentials.NewTLS(c.baseConfig)
	return nil
}
