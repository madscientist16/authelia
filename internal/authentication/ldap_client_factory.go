package authentication

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/go-ldap/ldap/v3"

	"github.com/authelia/authelia/v4/internal/configuration/schema"
	"github.com/authelia/authelia/v4/internal/utils"
)

// LDAPClientFactory an interface describing factories that produce LDAPConnection implementations.
type LDAPClientFactory interface {
	Initialize() (err error)
	GetClient(opts ...LDAPClientFactoryOption) (client ldap.Client, err error)
	Shutdown() (err error)
}

// NewLDAPClientFactoryStandard create a concrete ldap connection factory.
func NewLDAPClientFactoryStandard(config *schema.AuthenticationBackendLDAP, certs *x509.CertPool, dialer LDAPClientDialer) *LDAPClientStandardFactory {
	if dialer == nil {
		dialer = &LDAPClientDialerStandard{}
	}

	tlsc := utils.NewTLSConfig(config.TLS, certs)

	opts := []ldap.DialOpt{
		ldap.DialWithDialer(&net.Dialer{Timeout: config.Timeout}),
		ldap.DialWithTLSConfig(tlsc),
	}

	return &LDAPClientStandardFactory{
		config: config,
		tls:    tlsc,
		opts:   opts,
		dialer: dialer,
	}
}

// LDAPClientStandardFactory the production implementation of an ldap connection factory.
type LDAPClientStandardFactory struct {
	config *schema.AuthenticationBackendLDAP
	tls    *tls.Config
	opts   []ldap.DialOpt
	dialer LDAPClientDialer
}

func (f *LDAPClientStandardFactory) Initialize() (err error) {
	return nil
}

func (f *LDAPClientStandardFactory) GetClient(opts ...LDAPClientFactoryOption) (client ldap.Client, err error) {
	config := &LDAPClientFactoryOptions{
		Address:  f.config.Address.String(),
		Username: f.config.User,
		Password: f.config.Password,
	}

	for _, opt := range opts {
		opt(config)
	}

	if client, err = f.dialer.DialURL(config.Address, f.opts...); err != nil {
		return nil, fmt.Errorf("error occurred dialing address: %w", err)
	}

	if f.tls != nil && f.config.StartTLS {
		if err = client.StartTLS(f.tls); err != nil {
			_ = client.Close()

			return nil, fmt.Errorf("error occurred performing starttls: %w", err)
		}
	}

	if config.Password == "" {
		err = client.UnauthenticatedBind(config.Username)
	} else {
		err = client.Bind(config.Username, config.Password)
	}

	if err != nil {
		_ = client.Close()

		return nil, fmt.Errorf("error occurred performing bind: %w", err)
	}

	return client, nil
}

func (f *LDAPClientStandardFactory) Shutdown() (err error) {
	return nil
}

// NewLDAPConnectionFactoryPooled is a decorator for a LDAPClientFactory that performs pooling.
func NewLDAPConnectionFactoryPooled(factory LDAPClientFactory, count, retries int, timeout time.Duration) (pool *LDAPClientPooledFactory) {
	if count <= 0 {
		count = 5
	}

	if retries <= 0 {
		retries = 2
	}

	if timeout.Seconds() <= 0 {
		timeout = time.Second * 10
	}

	sleep := timeout / time.Duration(retries)

	return &LDAPClientPooledFactory{
		factory: factory,
		count:   count,
		timeout: timeout,
		sleep:   sleep,
	}
}

// LDAPClientPooledFactory is a LDAPClientFactory that takes another LDAPClientFactory and pools the
// factory generated connections using a channel for thread safety.
type LDAPClientPooledFactory struct {
	factory LDAPClientFactory

	count int

	timeout time.Duration
	sleep   time.Duration

	clients chan *LDAPClientPooled

	closing bool

	mu sync.Mutex
	wg sync.WaitGroup
}

func (f *LDAPClientPooledFactory) Initialize() (err error) {
	f.clients = make(chan *LDAPClientPooled, f.count)

	var (
		errs   []error
		client *LDAPClientPooled
	)

	for i := 0; i < f.count; i++ {
		if client, err = f.new(); err != nil {
			errs = append(errs, err)

			continue
		}

		f.clients <- client
	}

	if len(errs) == f.count {
		return fmt.Errorf("errors occurred initializing the client pool: no connections could be established")
	}

	return nil
}

// GetClient opens new client using the pool.
func (f *LDAPClientPooledFactory) GetClient(opts ...LDAPClientFactoryOption) (conn ldap.Client, err error) {
	if len(opts) != 0 {
		return f.factory.GetClient(opts...)
	}

	return f.acquire(context.Background())
}

func (f *LDAPClientPooledFactory) new() (pooled *LDAPClientPooled, err error) {
	f.mu.Lock()

	capacity, active := cap(f.clients), len(f.clients)

	if active >= capacity {
		f.mu.Unlock()

		return nil, fmt.Errorf("error occurred establishing new client for the pool: pool is already the maximum size")
	}

	var client ldap.Client

	if client, err = f.factory.GetClient(); err != nil {
		f.mu.Unlock()

		return nil, err
	}

	f.wg.Add(1)

	f.mu.Unlock()

	return &LDAPClientPooled{pool: f, Client: client}, nil
}

func (f *LDAPClientPooledFactory) relinquish(client *LDAPClientPooled) (err error) {
	f.mu.Lock()

	if f.closing {
		f.mu.Unlock()

		return client.Client.Close()
	}

	f.mu.Unlock()

	// Prevent extra connections from being added to the f and hanging around.
	if cap(f.clients) == len(f.clients) {
		return client.Client.Close()
	}

	f.clients <- client

	return nil
}

func (f *LDAPClientPooledFactory) acquire(ctx context.Context) (client *LDAPClientPooled, err error) {
	f.mu.Lock()

	if f.closing {
		f.mu.Unlock()

		return nil, fmt.Errorf("error acquiring client: the pool is closed")
	}

	f.mu.Unlock()

	if cap(f.clients) != f.count {
		if err = f.Initialize(); err != nil {
			return nil, err
		}
	}

	ctx, cancel := context.WithTimeout(ctx, f.timeout)
	defer cancel()

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case client = <-f.clients:
		if client.IsClosing() || client.Client == nil {
			f.wg.Done()

			for {
				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				default:
					if client, err = f.new(); err != nil {
						time.Sleep(f.sleep)

						continue
					}

					return client, nil
				}
			}
		}

		return client, nil
	}
}

func (f *LDAPClientPooledFactory) Shutdown() (err error) {
	f.mu.Lock()

	f.closing = true

	f.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second*10)

	defer cancel()

	go func() {
		select {
		case client := <-f.clients:
			_ = client.Client.Close()
			f.wg.Done()
		case <-ctx.Done():
			return
		}
	}()

	f.wg.Wait()

	f.mu.Lock()

	close(f.clients)

	f.mu.Unlock()

	return nil
}

// LDAPClientPooled is a decorator for the ldap.Client which handles the pooling functionality. i.e. prevents the client
// from being closed and instead relinquishes the connection back to the pool.
type LDAPClientPooled struct {
	pool *LDAPClientPooledFactory

	ldap.Client
}

// Close the LDAPClientPooled by relinquishing access to it and making it available in the pool again.
func (c *LDAPClientPooled) Close() (err error) {
	return c.pool.relinquish(c)
}
