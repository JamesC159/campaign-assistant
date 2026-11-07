// Package mail sends transactional email: SES in production, an in-memory
// mailbox in dev and tests (PROVIDER=mock), viewable at /dev/mailbox.
package mail

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sesv2"
	"github.com/aws/aws-sdk-go-v2/service/sesv2/types"
)

// Message is one plain-text email.
type Message struct {
	To      string    `json:"to"`
	Subject string    `json:"subject"`
	Body    string    `json:"body"`
	SentAt  time.Time `json:"sent_at"`
}

// Sender sends email.
type Sender interface {
	Send(ctx context.Context, m Message) error
}

// Mailbox is the mock Sender: it keeps the most recent messages in memory.
type Mailbox struct {
	Log *slog.Logger
	mu  sync.Mutex
	msg []Message
}

const mailboxSize = 100

// Send implements Sender.
func (b *Mailbox) Send(_ context.Context, m Message) error {
	m.SentAt = time.Now()
	b.mu.Lock()
	b.msg = append(b.msg, m)
	if len(b.msg) > mailboxSize {
		b.msg = b.msg[len(b.msg)-mailboxSize:]
	}
	b.mu.Unlock()
	if b.Log != nil {
		b.Log.Info("mock email sent (see /dev/mailbox)", "to", m.To, "subject", m.Subject)
	}
	return nil
}

// Messages returns sent messages, newest first.
func (b *Mailbox) Messages() []Message {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]Message, len(b.msg))
	for i, m := range b.msg {
		out[len(b.msg)-1-i] = m
	}
	return out
}

// SESAPI is the slice of the SES v2 client used here.
type SESAPI interface {
	SendEmail(ctx context.Context, in *sesv2.SendEmailInput, optFns ...func(*sesv2.Options)) (*sesv2.SendEmailOutput, error)
}

// SES sends through Amazon SES v2. From must be a verified identity.
type SES struct {
	Client SESAPI
	From   string
}

// Send implements Sender.
func (s SES) Send(ctx context.Context, m Message) error {
	_, err := s.Client.SendEmail(ctx, &sesv2.SendEmailInput{
		FromEmailAddress: aws.String(s.From),
		Destination:      &types.Destination{ToAddresses: []string{m.To}},
		Content: &types.EmailContent{Simple: &types.Message{
			Subject: &types.Content{Data: aws.String(m.Subject), Charset: aws.String("UTF-8")},
			Body:    &types.Body{Text: &types.Content{Data: aws.String(m.Body), Charset: aws.String("UTF-8")}},
		}},
	})
	if err != nil {
		return fmt.Errorf("ses send: %w", err)
	}
	return nil
}
