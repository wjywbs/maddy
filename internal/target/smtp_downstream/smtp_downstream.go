// Package smtp_downstream provides smtp_downstream module that implements
// transparent forwarding or messages to configured list of SMTP servers.
//
// Like remote module, this implementation doesn't handle atomic
// delivery properly since it is impossible to do with SMTP protocol
//
// Interfaces implemented:
// - module.DeliveryTarget
package smtp_downstream

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"runtime/trace"

	"github.com/emersion/go-message/textproto"
	"github.com/foxcpp/maddy/internal/buffer"
	"github.com/foxcpp/maddy/internal/config"
	"github.com/foxcpp/maddy/internal/exterrors"
	"github.com/foxcpp/maddy/internal/log"
	"github.com/foxcpp/maddy/internal/module"
	"github.com/foxcpp/maddy/internal/smtpconn"
	"github.com/foxcpp/maddy/internal/target"
	"golang.org/x/net/idna"
)

func moduleError(err error) error {
	if err == nil {
		return nil
	}

	return exterrors.WithFields(err, map[string]interface{}{
		"target": "smtp_downstream",
	})
}

type Downstream struct {
	instName   string
	targetsArg []string

	requireTLS      bool
	attemptStartTLS bool
	hostname        string
	endpoints       []config.Endpoint
	saslFactory     saslClientFactory
	tlsConfig       tls.Config

	log log.Logger
}

func NewDownstream(_, instName string, _, inlineArgs []string) (module.Module, error) {
	return &Downstream{
		instName:   instName,
		targetsArg: inlineArgs,
		log:        log.Logger{Name: "smtp_downstream"},
	}, nil
}

func (u *Downstream) Init(cfg *config.Map) error {
	var targetsArg []string
	cfg.Bool("debug", true, false, &u.log.Debug)
	cfg.Bool("require_tls", false, false, &u.requireTLS)
	cfg.Bool("attempt_starttls", false, true, &u.attemptStartTLS)
	cfg.String("hostname", true, true, "", &u.hostname)
	cfg.StringList("targets", false, false, nil, &targetsArg)
	cfg.Custom("auth", false, false, func() (interface{}, error) {
		return nil, nil
	}, saslAuthDirective, &u.saslFactory)
	cfg.Custom("tls_client", true, false, func() (interface{}, error) {
		return tls.Config{}, nil
	}, config.TLSClientBlock, &u.tlsConfig)

	if _, err := cfg.Process(); err != nil {
		return err
	}

	// INTERNATIONALIZATION: See RFC 6531 Section 3.7.1.
	var err error
	u.hostname, err = idna.ToASCII(u.hostname)
	if err != nil {
		return fmt.Errorf("smtp_downstream: cannot represent the hostname as an A-label name: %w", err)
	}

	u.targetsArg = append(u.targetsArg, targetsArg...)
	for _, tgt := range u.targetsArg {
		endp, err := config.ParseEndpoint(tgt)
		if err != nil {
			return err
		}

		u.endpoints = append(u.endpoints, endp)
	}

	if len(u.endpoints) == 0 {
		return fmt.Errorf("smtp_downstream: at least one target endpoint is required")
	}

	return nil
}

func (u *Downstream) Name() string {
	return "smtp_downstream"
}

func (u *Downstream) InstanceName() string {
	return u.instName
}

type delivery struct {
	u   *Downstream
	log log.Logger

	msgMeta  *module.MsgMetadata
	mailFrom string
	body     io.ReadCloser
	hdr      textproto.Header

	conn *smtpconn.C
}

func (u *Downstream) Start(ctx context.Context, msgMeta *module.MsgMetadata, mailFrom string) (module.Delivery, error) {
	defer trace.StartRegion(ctx, "smtp_downstream/Start").End()

	d := &delivery{
		u:        u,
		log:      target.DeliveryLogger(u.log, msgMeta),
		msgMeta:  msgMeta,
		mailFrom: mailFrom,
	}
	if err := d.connect(ctx); err != nil {
		return nil, err
	}

	if err := d.conn.Mail(ctx, mailFrom, msgMeta.SMTPOpts); err != nil {
		d.conn.Close()
		return nil, err
	}
	return d, nil
}

func (d *delivery) connect(ctx context.Context) error {
	// TODO: Review possibility of connection pooling here.
	var lastErr error

	conn := smtpconn.New()
	conn.Log = d.log
	conn.Hostname = d.u.hostname
	conn.AddrInSMTPMsg = false

	for _, endp := range d.u.endpoints {
		didTLS, err := conn.Connect(ctx, endp, d.u.attemptStartTLS, &d.u.tlsConfig)
		if err != nil {
			if len(d.u.endpoints) != 1 {
				d.log.Msg("connect error", err, "downstream_server", net.JoinHostPort(endp.Host, endp.Port))
			}
			lastErr = err
			continue
		}

		d.log.DebugMsg("connected", "downstream_server", conn.ServerName())

		if !didTLS && d.u.requireTLS {
			conn.Close()
			lastErr = errors.New("TLS is required, but unsupported by downstream")
			continue
		}

		lastErr = nil
		break
	}
	if lastErr != nil {
		return moduleError(lastErr)
	}

	if d.u.saslFactory != nil {
		saslClient, err := d.u.saslFactory(d.msgMeta)
		if err != nil {
			conn.Close()
			return err
		}

		if err := conn.Client().Auth(saslClient); err != nil {
			conn.Close()
			return err
		}
	}

	d.conn = conn

	return nil
}

func (d *delivery) AddRcpt(ctx context.Context, rcptTo string) error {
	return moduleError(d.conn.Rcpt(ctx, rcptTo))
}

func (d *delivery) Body(ctx context.Context, header textproto.Header, body buffer.Buffer) error {
	r, err := body.Open()
	if err != nil {
		return exterrors.WithFields(err, map[string]interface{}{"target": "smtp_downstream"})
	}

	d.body = r
	d.hdr = header
	return nil
}

func (d *delivery) Abort(ctx context.Context) error {
	if d.body != nil {
		d.body.Close()
	}
	d.conn.Close()
	return nil
}

func (d *delivery) Commit(ctx context.Context) error {
	defer d.conn.Close()
	defer d.body.Close()

	return moduleError(d.conn.Data(ctx, d.hdr, d.body))
}

func init() {
	module.Register("smtp_downstream", NewDownstream)
}
