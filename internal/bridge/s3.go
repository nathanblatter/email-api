package bridge

import (
	"context"
	"fmt"
	"io"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// S3Objects adapts the AWS SDK to the ObjectStore interface.
type S3Objects struct {
	Client *s3.Client
}

// OpenS3 builds an S3 client from the standard SDK credential chain.
func OpenS3(ctx context.Context, region string) (*S3Objects, error) {
	cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(region))
	if err != nil {
		return nil, fmt.Errorf("aws config: %w", err)
	}
	return &S3Objects{Client: s3.NewFromConfig(cfg)}, nil
}

// Get reads one object whole. SES caps received mail at 40 MB and /inbound
// at 30 MB, so reading into memory is fine and the cap is enforced here too.
func (o *S3Objects) Get(ctx context.Context, bucket, key string) ([]byte, error) {
	out, err := o.Client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)})
	if err != nil {
		return nil, err
	}
	defer out.Body.Close()
	return io.ReadAll(io.LimitReader(out.Body, maxInboundObject))
}

// maxInboundObject mirrors api.MaxInboundBytes plus the prepended header.
const maxInboundObject = 30<<20 + 1024
