package mail

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/sesv2"
	"github.com/aws/aws-sdk-go-v2/service/sesv2/types"
)

// SESSender delivers through the Amazon SES API (SendEmail with raw content),
// for domains whose DNS and DKIM live in AWS (byuisresearch.com). The MIME
// email-api already built is handed over untouched; SES adds its DKIM
// signature.
type SESSender struct {
	Client *sesv2.Client
}

// OpenSES builds the client from the standard SDK credential chain.
func OpenSES(ctx context.Context, region string) (*SESSender, error) {
	cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(region))
	if err != nil {
		return nil, fmt.Errorf("aws config: %w", err)
	}
	return &SESSender{Client: sesv2.NewFromConfig(cfg)}, nil
}

func (s *SESSender) Send(ctx context.Context, from string, rcpts []string, raw []byte) error {
	_, err := s.Client.SendEmail(ctx, &sesv2.SendEmailInput{
		FromEmailAddress: aws.String(from),
		Destination:      &types.Destination{ToAddresses: rcpts},
		Content:          &types.EmailContent{Raw: &types.RawMessage{Data: raw}},
	})
	if err != nil {
		return fmt.Errorf("ses send: %w", err)
	}
	return nil
}

// Ping checks the account can send at all (credentials, region, sending
// not paused). Identity verification problems surface on Send.
func (s *SESSender) Ping(ctx context.Context) error {
	out, err := s.Client.GetAccount(ctx, &sesv2.GetAccountInput{})
	if err != nil {
		return fmt.Errorf("ses: %w", err)
	}
	if !out.SendingEnabled {
		return fmt.Errorf("ses: sending disabled for the account")
	}
	return nil
}
