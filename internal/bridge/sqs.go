package bridge

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
)

// SQSQueue adapts the AWS SDK to the Queue interface.
type SQSQueue struct {
	Client   *sqs.Client
	QueueURL string
}

// OpenSQS builds an SQS client from the standard SDK credential chain
// (AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY env, shared config, ...).
func OpenSQS(ctx context.Context, region, queueURL string) (*SQSQueue, error) {
	cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(region))
	if err != nil {
		return nil, fmt.Errorf("aws config: %w", err)
	}
	return &SQSQueue{Client: sqs.NewFromConfig(cfg), QueueURL: queueURL}, nil
}

// Receive long-polls for up to 20s. The 60s visibility timeout covers a slow
// /send plus the delete; an unprocessed message reappears after it.
func (q *SQSQueue) Receive(ctx context.Context) ([]Message, error) {
	out, err := q.Client.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{
		QueueUrl:            aws.String(q.QueueURL),
		MaxNumberOfMessages: 10,
		WaitTimeSeconds:     20,
		VisibilityTimeout:   60,
	})
	if err != nil {
		return nil, err
	}
	msgs := make([]Message, 0, len(out.Messages))
	for _, m := range out.Messages {
		msgs = append(msgs, Message{
			ID:     aws.ToString(m.MessageId),
			Handle: aws.ToString(m.ReceiptHandle),
			Body:   aws.ToString(m.Body),
		})
	}
	return msgs, nil
}

func (q *SQSQueue) Delete(ctx context.Context, handle string) error {
	_, err := q.Client.DeleteMessage(ctx, &sqs.DeleteMessageInput{
		QueueUrl:      aws.String(q.QueueURL),
		ReceiptHandle: aws.String(handle),
	})
	return err
}
