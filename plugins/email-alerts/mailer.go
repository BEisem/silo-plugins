package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	mail "github.com/wneessen/go-mail"
)

type Outgoing struct {
	To       string
	ToName   string
	Rendered *Rendered
	// UnsubscribeURL answers a GET with a confirmation page and an RFC 8058
	// one-click POST directly.
	UnsubscribeURL string
}

func sendMail(ctx context.Context, cfg SMTPConfig, msgs ...Outgoing) error {
	if cfg.Host == "" || cfg.FromAddress == "" {
		return errors.New("SMTP is not configured")
	}
	opts := []mail.Option{
		mail.WithPort(cfg.Port),
		mail.WithTimeout(30 * time.Second),
	}
	switch cfg.Security {
	case "tls":
		opts = append(opts, mail.WithSSL())
	case "none":
		opts = append(opts, mail.WithTLSPolicy(mail.NoTLS))
	default:
		opts = append(opts, mail.WithTLSPolicy(mail.TLSMandatory))
	}
	if cfg.Username != "" {
		opts = append(opts,
			mail.WithSMTPAuth(mail.SMTPAuthAutoDiscover),
			mail.WithUsername(cfg.Username),
			mail.WithPassword(cfg.Password),
		)
	}
	client, err := mail.NewClient(cfg.Host, opts...)
	if err != nil {
		return fmt.Errorf("smtp client: %w", err)
	}

	built := make([]*mail.Msg, 0, len(msgs))
	for _, o := range msgs {
		m, err := buildMsg(cfg, o)
		if err != nil {
			return err
		}
		built = append(built, m)
	}
	if err := client.DialAndSendWithContext(ctx, built...); err != nil {
		return fmt.Errorf("smtp send: %w", err)
	}
	return nil
}

func buildMsg(cfg SMTPConfig, o Outgoing) (*mail.Msg, error) {
	m := mail.NewMsg()
	fromName := cfg.FromName
	if fromName == "" {
		fromName = "Silo"
	}
	if err := m.FromFormat(fromName, cfg.FromAddress); err != nil {
		return nil, fmt.Errorf("from address: %w", err)
	}
	if o.ToName != "" {
		if err := m.AddToFormat(o.ToName, o.To); err != nil {
			return nil, fmt.Errorf("recipient %q: %w", o.To, err)
		}
	} else if err := m.To(o.To); err != nil {
		return nil, fmt.Errorf("recipient %q: %w", o.To, err)
	}
	if cfg.ReplyTo != "" {
		if err := m.ReplyTo(cfg.ReplyTo); err != nil {
			return nil, fmt.Errorf("reply-to: %w", err)
		}
	}
	m.Subject(o.Rendered.Subject)
	m.SetMessageID()
	m.SetDate()
	m.SetGenHeader(mail.HeaderXMailer, "Silo Email Alerts plugin")
	m.SetGenHeader("Auto-Submitted", "auto-generated")
	if o.UnsubscribeURL != "" {
		m.SetGenHeader(mail.HeaderListUnsubscribe, "<"+o.UnsubscribeURL+">")
		m.SetGenHeader(mail.HeaderListUnsubscribePost, "List-Unsubscribe=One-Click")
	}
	text := strings.TrimSpace(o.Rendered.Text)
	if text == "" {
		text = o.Rendered.Subject
	}
	m.SetBodyString(mail.TypeTextPlain, text)
	m.AddAlternativeString(mail.TypeTextHTML, o.Rendered.HTML)
	for _, img := range o.Rendered.Images {
		name := strings.SplitN(img.CID, "@", 2)[0] + extFor(img.ContentType)
		if err := m.EmbedReader(name, bytes.NewReader(img.Data),
			mail.WithFileContentID(img.CID),
			mail.WithFileContentType(mail.ContentType(img.ContentType)),
		); err != nil {
			return nil, fmt.Errorf("embed image: %w", err)
		}
	}
	return m, nil
}

func extFor(ct string) string {
	switch ct {
	case "image/png":
		return ".png"
	case "image/webp":
		return ".webp"
	case "image/gif":
		return ".gif"
	default:
		return ".jpg"
	}
}
